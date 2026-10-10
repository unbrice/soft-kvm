// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// agent_test.go: wiring tests for the agent loop (SPEC §11.3, §11.4). The
// machine is real; the detector, guard, and runner are fakes; the server is
// real under httptest.

package agent

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/discover"
	"github.com/unbrice/soft-kvm/identity"
	"github.com/unbrice/soft-kvm/model"
	"github.com/unbrice/soft-kvm/server"
	"github.com/unbrice/soft-kvm/state"
)

const testToken = "test-token-12345"

func agentTestBase(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u.Host
}

func startTLSTestServer(t *testing.T, s *server.Server, token string) *httptest.Server {
	t.Helper()
	tlsCfg, err := identity.ServerTLSConfig(token)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	return srv
}

func newAgentTestServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := server.NewServer(statePath, testToken)
	s.SetWaitTimeout(200 * time.Millisecond)
	return s, startTLSTestServer(t, s, testToken)
}

func newTestClient(t *testing.T, token string) *client.Client {
	t.Helper()
	c, err := client.NewClient(token)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
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

func agentMachineConfig(id string) *model.MachineConfig {
	return &model.MachineConfig{
		ID:             id,
		Settle:         50 * time.Millisecond,
		SwitchRetries:  1,
		RetrySpacing:   50 * time.Millisecond,
		Cooldown:       50 * time.Millisecond,
		BreakerWindow:  1 * time.Minute,
		BreakerMax:     10,
		BreakerOpenFor: 1 * time.Minute,
	}
}

func agentTestConfig(t *testing.T, base, statePath string, s *server.Server, det Detector, guard Guard, runner Runner) Config {
	t.Helper()
	return Config{
		ID:             "linux",
		ExplicitServer: base,
		KeyFP:          identity.KeyFingerprint(testToken),
		Detector:       det,
		Guard:          guard,
		Client:         newTestClient(t, testToken),
		Runner:         runner,
		Machine:        agentMachineConfig("linux"),
		AgentStatePath: statePath,
		SwitchCommands: [][]string{{"switch", "cmd"}},
		NotifyArgv:     []string{"notify", "cmd"},
		SwitchTimeout:  1 * time.Second,
		GuardPoll:      20 * time.Millisecond,
		BackoffBase:    10 * time.Millisecond,
		Resolver:       discover.NewResolver(filepath.Join(t.TempDir(), "server")),
	}
}

func runAgent(ctx context.Context, t *testing.T, cfg Config) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
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
	runAgent(ctx, t, cfg)

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
	if _, err := client.Claim(context.Background(), base, "linux", true, 0); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "agent.json")
	if err := state.Save(statePath, state.AgentState{LastOwner: "linux"}); err != nil {
		t.Fatalf("save agent state: %v", err)
	}

	// Make "other" appear live to the server so the agent will switch to it.
	s.SetWaiterCount("other", 1)

	det := newFakeDetector()
	runner := &recordingRunner{}
	guard := &fakeGuard{ok: true}
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)
	// A second switch command (e.g. a USB device) runs after the display one.
	cfg.SwitchCommands = append(cfg.SwitchCommands, []string{"switch", "usb"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)

	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "other", true, 0); err != nil {
		t.Fatalf("force claim other: %v", err)
	}

	// Exit 0 from every command is the receipt: nothing probes before or after.
	waitForLastOwner(t, statePath, "other")
	calls := runner.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 runner calls, got %d: %v", len(calls), calls)
	}
	if !slices.Equal(calls[0], cfg.SwitchCommands[0]) {
		t.Errorf("call 0 = %v, want switch %v", calls[0], cfg.SwitchCommands[0])
	}
	if !slices.Equal(calls[1], cfg.SwitchCommands[1]) {
		t.Errorf("call 1 = %v, want switch %v", calls[1], cfg.SwitchCommands[1])
	}
}

// A retry re-runs only the commands that failed: the peripheral the second
// command already moved must not be chased again (SPEC §4.3).
func TestAgentRetryRunsOnlyFailedCommands(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	if _, err := client.Claim(context.Background(), base, "linux", true, 0); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "agent.json")
	if err := state.Save(statePath, state.AgentState{LastOwner: "linux"}); err != nil {
		t.Fatalf("save agent state: %v", err)
	}

	s.SetWaiterCount("other", 1)

	det := newFakeDetector()
	// Attempt 1: the display command fails, the USB one succeeds. Attempt 2:
	// the display command succeeds.
	runner := &recordingRunner{errs: []error{errors.New("monitor still here"), nil, nil}}
	guard := &fakeGuard{ok: true}
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)
	cfg.SwitchCommands = append(cfg.SwitchCommands, []string{"switch", "usb"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)

	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "other", true, 0); err != nil {
		t.Fatalf("force claim other: %v", err)
	}

	waitForLastOwner(t, statePath, "other")
	calls := runner.Calls()
	want := [][]string{cfg.SwitchCommands[0], cfg.SwitchCommands[1], cfg.SwitchCommands[0]}
	if len(calls) != len(want) {
		t.Fatalf("expected %d runner calls, got %d: %v", len(want), len(calls), calls)
	}
	for i := range want {
		if !slices.Equal(calls[i], want[i]) {
			t.Errorf("call %d = %v, want %v", i, calls[i], want[i])
		}
	}
}

// screen-switch is built in: it goes to the screen runner, never to the argv
// runner as a program called "screen-switch" (SPEC §5.6).
func TestAgentScreenSwitchIsBuiltIn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ScreenRunner (ddcutil) is Linux-only; macOS uses IOAVService")
	}
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	if _, err := client.Claim(context.Background(), base, "linux", true, 0); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "agent.json")
	if err := state.Save(statePath, state.AgentState{LastOwner: "linux"}); err != nil {
		t.Fatalf("save agent state: %v", err)
	}

	s.SetWaiterCount("other", 1)

	det := newFakeDetector()
	runner := &recordingRunner{}
	// The write succeeds; any read after it fails, which is the monitor
	// having left this input.
	screenRunner := &recordingRunner{errs: []error{nil, errors.New("no DDC reply")}}
	guard := &fakeGuard{ok: true}
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)
	cfg.SwitchCommands = [][]string{{"screen-switch", "dp1"}}
	cfg.ScreenRunner = func(ctx context.Context, argv []string) ([]byte, error) {
		return nil, screenRunner.run(ctx, argv)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)

	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "other", true, 0); err != nil {
		t.Fatalf("force claim other: %v", err)
	}

	waitForLastOwner(t, statePath, "other")
	if len(screenRunner.Calls()) == 0 {
		t.Error("screen-switch never reached the screen runner")
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Errorf("screen-switch was exec'd as a program: %v", calls)
	}
}

func waitForLastOwner(t *testing.T, statePath, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var as state.AgentState
		if err := state.Load(statePath, &as); err == nil && as.LastOwner == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent state never reached last_owner=%q (got %q)", want, as.LastOwner)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentGuardsDown(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	if _, err := client.Claim(context.Background(), base, "other", true, 0); err != nil {
		t.Fatalf("seed claim other: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "agent.json")
	if err := state.Save(statePath, state.AgentState{LastOwner: "linux"}); err != nil {
		t.Fatalf("save agent state: %v", err)
	}

	det := newFakeDetector()
	runner := &recordingRunner{}
	guard := &fakeGuard{ok: false}
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)

	// This asserts an absence (no decisions while dormant); it cannot be
	// event-driven. Sleep a small multiple of the injected guard poll.
	time.Sleep(5 * cfg.GuardPoll)
	det.attach()
	time.Sleep(5 * cfg.GuardPoll)

	if det.runCalled.Load() {
		t.Error("detector started while guards were down")
	}
	if len(runner.Calls()) != 0 {
		t.Errorf("expected zero runner calls, got %v", runner.Calls())
	}
	st := s.CurrentState()
	if st.Owner != "other" {
		t.Errorf("server owner changed to %q while guards down", st.Owner)
	}
}

func TestAgentSwitchTimeout(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	// Seed the server so the agent starts with last_owner == me.
	if _, err := client.Claim(context.Background(), base, "linux", true, 0); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "agent.json")
	if err := state.Save(statePath, state.AgentState{LastOwner: "linux"}); err != nil {
		t.Fatalf("save agent state: %v", err)
	}

	// Make "other" appear live so the agent will try to switch to it.
	s.SetWaiterCount("other", 1)

	det := newFakeDetector()
	runner := newHungSwitchRunner()
	guard := &fakeGuard{ok: true}
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)
	cfg.SwitchTimeout = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)

	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "other", true, 0); err != nil {
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
	if server.DefaultWaitTimeout >= client.WaitClientTimeout || client.WaitClientTimeout >= waitTimeout {
		t.Fatalf("wait timeouts out of order: server=%v client=%v agent=%v",
			server.DefaultWaitTimeout, client.WaitClientTimeout, waitTimeout)
	}
	if DefaultSwitchTimeout >= model.SwitchDeadline {
		t.Fatalf("switch timeout %v must be below switch deadline %v",
			DefaultSwitchTimeout, model.SwitchDeadline)
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

func waitForLive(t *testing.T, s *server.Server, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		st := s.CurrentState()
		if st.Live[id] {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiter %q never registered", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The winner publishes the Easy-Switch channel it occupies, so the losing
// host knows where to send the peripherals it still holds (SPEC §5.5).
func TestAgentClaimPublishesOwnChannel(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)

	det := newFakeDetector()
	runner := &recordingRunner{}
	guard := &fakeGuard{ok: true}
	statePath := filepath.Join(t.TempDir(), "agent.json")
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)

	// Count the reads: the channel is a property of the pairing, so it must
	// be read once and cached, not on every claim.
	var reads atomic.Int32
	cfg.OwnChannel = func(context.Context) (uint8, error) {
		reads.Add(1)
		return 2, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)

	waitForLive(t, s, "linux")
	det.attach()

	client := newTestClient(t, testToken)
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := client.State(ctx, base)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		if st.Owner == "linux" && st.OwnerChannel == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner channel not published: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}

	det.attach()
	time.Sleep(100 * time.Millisecond)
	if n := reads.Load(); n != 1 {
		t.Errorf("OwnChannel read %d times, want 1 (cached)", n)
	}
}

// A nil OwnChannel — a trigger that cannot answer HID++ — must not break the
// claim; it just publishes nothing.
func TestAgentClaimWithoutOwnChannel(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)

	det := newFakeDetector()
	runner := &recordingRunner{}
	guard := &fakeGuard{ok: true}
	statePath := filepath.Join(t.TempDir(), "agent.json")
	cfg := agentTestConfig(t, base, statePath, s, det, guard, runner.run)
	cfg.OwnChannel = nil

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)

	waitForLive(t, s, "linux")
	det.attach()

	client := newTestClient(t, testToken)
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := client.State(ctx, base)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		if st.Owner == "linux" {
			if st.OwnerChannel != 0 {
				t.Fatalf("OwnerChannel = %d, want 0", st.OwnerChannel)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("claim did not land: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
