// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// agent_test.go: wiring tests for the agent loop (SPEC §11.3, §11.4). The
// machine is real; the detector, guard, and runner are fakes; the server is
// real under httptest.

package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func agentTestBase(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u.Host
}

func newAgentTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := NewServer(statePath, testToken)
	s.waitTimeout = 200 * time.Millisecond
	return s, startTLSTestServer(t, s)
}

// fakeDetector lets tests inject attach edges. Run is not started until the
// agent's guard allows it.
type fakeDetector struct {
	ch        chan struct{}
	runCalled atomic.Bool
}

func newFakeDetector() *fakeDetector {
	return &fakeDetector{ch: make(chan struct{}, 1)}
}

func (d *fakeDetector) attach() {
	d.ch <- struct{}{}
}

func (d *fakeDetector) Run(ctx context.Context, attach chan<- struct{}) error {
	d.runCalled.Store(true)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.ch:
			select {
			case attach <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

type fakeGuard struct {
	mu sync.RWMutex
	ok bool
}

func (g *fakeGuard) OK(context.Context) (bool, string) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.ok {
		return true, "ok"
	}
	return false, "guards down"
}

type recordingRunner struct {
	mu    sync.Mutex
	calls [][]string
	errs  []error
	idx   int
}

func (r *recordingRunner) run(_ context.Context, argv []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, argv)
	if r.idx < len(r.errs) {
		err := r.errs[r.idx]
		r.idx++
		return err
	}
	return nil
}

func (r *recordingRunner) Calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func agentMachineConfig(id string) *MachineConfig {
	return &MachineConfig{
		ID:             id,
		Settle:         50 * time.Millisecond,
		Confirm:        50 * time.Millisecond,
		SwitchRetries:  1,
		RetrySpacing:   50 * time.Millisecond,
		Cooldown:       50 * time.Millisecond,
		BreakerWindow:  1 * time.Minute,
		BreakerMax:     10,
		BreakerOpenFor: 1 * time.Minute,
	}
}

func agentTestConfig(t *testing.T, base, statePath string, s *Server, det Detector, guard Guard, runner Runner) agentConfig {
	t.Helper()
	return agentConfig{
		id:             "linux",
		explicitServer: base,
		detector:       det,
		guard:          guard,
		client:         newTestClient(t, testToken),
		runner:         runner,
		machine:        agentMachineConfig("linux"),
		agentStatePath: statePath,
		switchArgv:     []string{"switch", "cmd"},
		checkArgv:      []string{"check", "cmd"},
		notifyArgv:     []string{"notify", "cmd"},
		checkTimeout:   1 * time.Second,
		switchTimeout:  1 * time.Second,
		guardPoll:      20 * time.Millisecond,
		backoffBase:    10 * time.Millisecond,
	}
}

func runAgent(ctx context.Context, t *testing.T, ag *agent) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- ag.run(ctx) }()
	t.Cleanup(func() {
		<-ctx.Done()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("agent run returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("agent run did not return after cancel")
		}
	})
}

func TestAgentAttachClaims(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)

	det := newFakeDetector()
	runner := &recordingRunner{}
	guard := &fakeGuard{ok: true}
	statePath := filepath.Join(t.TempDir(), "agent.json")
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag := &agent{cfg: cfg}
	runAgent(ctx, t, ag)

	waitForLive(t, s, "linux")
	det.attach()

	client := newTestClient(t, testToken)
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err := client.State(ctx, base)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		if state.Owner == "linux" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server owner not linux: %+v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentSwitchPath(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	// Seed the server so the agent starts with last_owner == me.
	if _, err := client.Claim(context.Background(), base, "linux", true); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "agent.json")
	if err := saveJSON(statePath, agentState{LastOwner: "linux"}); err != nil {
		t.Fatalf("save agent state: %v", err)
	}

	// Make "other" appear live to the server so the agent will switch to it.
	s.mu.Lock()
	s.waiters["other"] = 1
	s.mu.Unlock()

	det := newFakeDetector()
	// Probe succeeds (veto passed), switch succeeds, confirm probe fails (landed).
	runner := &recordingRunner{errs: []error{nil, nil, errors.New("input inactive")}}
	guard := &fakeGuard{ok: true}
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag := &agent{cfg: cfg}
	runAgent(ctx, t, ag)

	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "other", true); err != nil {
		t.Fatalf("force claim other: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		calls := runner.Calls()
		if len(calls) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 3 runner calls, got %d: %v", len(calls), calls)
		}
		time.Sleep(10 * time.Millisecond)
	}

	calls := runner.Calls()
	if !slices.Equal(calls[0], cfg.checkArgv) {
		t.Errorf("call 0 = %v, want probe %v", calls[0], cfg.checkArgv)
	}
	if !slices.Equal(calls[1], cfg.switchArgv) {
		t.Errorf("call 1 = %v, want switch %v", calls[1], cfg.switchArgv)
	}
	if !slices.Equal(calls[2], cfg.checkArgv) {
		t.Errorf("call 2 = %v, want confirm probe %v", calls[2], cfg.checkArgv)
	}

	var as agentState
	if err := loadJSON(statePath, &as); err != nil {
		t.Fatalf("load agent state: %v", err)
	}
	if as.LastOwner != "other" {
		t.Errorf("last_owner = %q, want other", as.LastOwner)
	}
}

func TestAgentVetoPath(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	if _, err := client.Claim(context.Background(), base, "linux", true); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "agent.json")
	if err := saveJSON(statePath, agentState{LastOwner: "linux"}); err != nil {
		t.Fatalf("save agent state: %v", err)
	}

	s.mu.Lock()
	s.waiters["other"] = 1
	s.mu.Unlock()

	det := newFakeDetector()
	// First probe fails the veto, so no switch runs.
	runner := &recordingRunner{errs: []error{errors.New("input inactive")}}
	guard := &fakeGuard{ok: true}
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag := &agent{cfg: cfg}
	runAgent(ctx, t, ag)

	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "other", true); err != nil {
		t.Fatalf("force claim other: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		calls := runner.Calls()
		if len(calls) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected at least 1 runner call, got %d", len(calls))
		}
		time.Sleep(10 * time.Millisecond)
	}

	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d: %v", len(calls), calls)
	}
	if !slices.Equal(calls[0], cfg.checkArgv) {
		t.Errorf("call 0 = %v, want probe %v", calls[0], cfg.checkArgv)
	}
	if slices.ContainsFunc(calls, func(argv []string) bool { return slices.Equal(argv, cfg.switchArgv) }) {
		t.Error("switch command ran despite veto")
	}

	var as agentState
	if err := loadJSON(statePath, &as); err != nil {
		t.Fatalf("load agent state: %v", err)
	}
	if as.LastOwner != "other" {
		t.Errorf("last_owner = %q, want other", as.LastOwner)
	}
}

func TestAgentGuardsDown(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	if _, err := client.Claim(context.Background(), base, "other", true); err != nil {
		t.Fatalf("seed claim other: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "agent.json")
	if err := saveJSON(statePath, agentState{LastOwner: "linux"}); err != nil {
		t.Fatalf("save agent state: %v", err)
	}

	det := newFakeDetector()
	runner := &recordingRunner{}
	guard := &fakeGuard{ok: false}
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag := &agent{cfg: cfg}
	runAgent(ctx, t, ag)

	// This asserts an absence (no decisions while dormant); it cannot be
	// event-driven. Sleep a small multiple of the injected guard poll.
	time.Sleep(5 * cfg.guardPoll)
	det.attach()
	time.Sleep(5 * cfg.guardPoll)

	if det.runCalled.Load() {
		t.Error("detector started while guards were down")
	}
	if len(runner.Calls()) != 0 {
		t.Errorf("expected zero runner calls, got %v", runner.Calls())
	}
	state := s.currentState()
	if state.Owner != "other" {
		t.Errorf("server owner changed to %q while guards down", state.Owner)
	}
}

func TestAgentSwitchTimeout(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	// Seed the server so the agent starts with last_owner == me.
	if _, err := client.Claim(context.Background(), base, "linux", true); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "agent.json")
	if err := saveJSON(statePath, agentState{LastOwner: "linux"}); err != nil {
		t.Fatalf("save agent state: %v", err)
	}

	// Make "other" appear live so the agent will try to switch to it.
	s.mu.Lock()
	s.waiters["other"] = 1
	s.mu.Unlock()

	det := newFakeDetector()
	runner := newHungSwitchRunner()
	guard := &fakeGuard{ok: true}
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)
	cfg.switchTimeout = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag := &agent{cfg: cfg}
	runAgent(ctx, t, ag)

	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "other", true); err != nil {
		t.Fatalf("force claim other: %v", err)
	}

	// Wait for the first switch to be attempted.
	select {
	case <-runner.called:
	case <-time.After(2 * time.Second):
		t.Fatal("switch runner not called")
	}

	// The agent must stay responsive: the timeout produces a SwitchExit and the
	// machine retries (SwitchRetries=1), so a second switch call arrives.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := runner.SwitchCalls()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 switch calls, got %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !runner.firstCtxCanceled() {
		t.Error("first switch context was not canceled by timeout")
	}
}

func TestTimingOrdering(t *testing.T) {
	if defaultWaitTimeout >= waitClientTimeout || waitClientTimeout >= waitTimeout {
		t.Fatalf("wait timeouts out of order: server=%v client=%v agent=%v",
			defaultWaitTimeout, waitClientTimeout, waitTimeout)
	}
	if defaultSwitchTimeout >= switchDeadline {
		t.Fatalf("switch timeout %v must be below switch deadline %v",
			defaultSwitchTimeout, switchDeadline)
	}
}

func TestBackoff(t *testing.T) {
	b := newBackoff()
	for i := 0; i < 100; i++ {
		if d := b.next(); d < 0 || d > backoffCap {
			t.Fatalf("next() = %v, out of [0, %v]", d, backoffCap)
		}
	}
	if b.cur != backoffCap {
		t.Fatalf("cur = %v, want the %v cap", b.cur, backoffCap)
	}
	b.reset()
	if b.cur != backoffBase {
		t.Fatalf("after reset cur = %v, want %v", b.cur, backoffBase)
	}
	if d := b.next(); d > backoffBase {
		t.Fatalf("next() after reset = %v, want <= %v", d, backoffBase)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait on cancelled ctx = %v, want context.Canceled", err)
	}
}

// hungSwitchRunner blocks on switch commands until their context is canceled,
// recording whether the first such context was canceled.
type hungSwitchRunner struct {
	mu                sync.Mutex
	called            chan struct{}
	switchCalls       int
	firstCtxCanceled_ bool
}

func newHungSwitchRunner() *hungSwitchRunner {
	return &hungSwitchRunner{called: make(chan struct{})}
}

func (r *hungSwitchRunner) run(ctx context.Context, argv []string) error {
	if len(argv) == 0 || argv[0] != "switch" {
		return nil
	}

	r.mu.Lock()
	r.switchCalls++
	calls := r.switchCalls
	r.mu.Unlock()
	if calls == 1 {
		close(r.called)
	}

	<-ctx.Done()

	r.mu.Lock()
	if calls == 1 {
		r.firstCtxCanceled_ = true
	}
	r.mu.Unlock()
	return ctx.Err()
}

func (r *hungSwitchRunner) SwitchCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.switchCalls
}

func (r *hungSwitchRunner) firstCtxCanceled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstCtxCanceled_
}
