// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// agent_test.go: wiring tests for the agent loop (SPEC §11.3, §11.4). The
// decider is real; the detector, guard, runner and the screen and device
// seams are fakes; the server is real under httptest.

package agent

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/discover"
	"github.com/unbrice/soft-kvm/hidpp"
	"github.com/unbrice/soft-kvm/identity"
	"github.com/unbrice/soft-kvm/model"
	"github.com/unbrice/soft-kvm/screen"
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

// recordingRunner records the commands run. Commands run in parallel, so it
// fails them by argv, not by call order: fails holds, per argv joined by
// spaces, the results of its successive runs; a run past them succeeds.
type recordingRunner struct {
	mu    sync.Mutex
	calls [][]string
	fails map[string][]error
}

func (r *recordingRunner) run(_ context.Context, argv []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, argv)
	key := strings.Join(argv, " ")
	if errs := r.fails[key]; len(errs) > 0 {
		r.fails[key] = errs[1:]
		return errs[0]
	}
	return nil
}

// Calls returns the commands run, sorted: parallel runs have no order.
func (r *recordingRunner) Calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.calls)
	slices.SortFunc(out, func(a, b []string) int { return slices.Compare(a, b) })
	return out
}

// sortedCalls sorts calls as Calls does.
func sortedCalls(calls [][]string) [][]string {
	out := slices.Clone(calls)
	slices.SortFunc(out, func(a, b []string) int { return slices.Compare(a, b) })
	return out
}

func agentDeciderConfig(id string) *model.DeciderConfig {
	return &model.DeciderConfig{
		ID:             id,
		OnDeactivation: [][]string{{"switch", "cmd"}},
		Settle:         50 * time.Millisecond,
		SwitchRetries:  1,
		RetrySpacing:   50 * time.Millisecond,
		ArrivalWindow:  500 * time.Millisecond,
		Cooldown:       50 * time.Millisecond,
		BreakerWindow:  1 * time.Minute,
		BreakerMax:     10,
		BreakerPause:   1 * time.Minute,
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
		Decider:        agentDeciderConfig("linux"),
		AgentStatePath: statePath,
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

// seedOwner makes id the owner on the server and records, in agent.json, that
// the agent has followed that ownership: it starts as the owner or as a host
// that was left, not as a fresh install.
func seedOwner(t *testing.T, base, ownerID, statePath, agentLastOwner string) {
	t.Helper()
	c := newTestClient(t, testToken)
	if _, err := c.Claim(context.Background(), base, ownerID, true, false); err != nil {
		t.Fatalf("seed claim %s: %v", ownerID, err)
	}
	rec := state.AgentState{HandledOwner: agentLastOwner, HandledActivation: 1}
	if err := state.Save(statePath, rec); err != nil {
		t.Fatalf("save agent state: %v", err)
	}
}

func TestAgentDeactivationPath(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	statePath := filepath.Join(t.TempDir(), "agent.json")
	seedOwner(t, base, "linux", statePath, "linux")

	det := newFakeDetector()
	runner := &recordingRunner{}
	cfg := agentTestConfig(t, base, statePath, s, det, &fakeGuard{ok: true}, runner.run)
	// A second command (e.g. a notification) runs after the first.
	cfg.Decider.OnDeactivation = append(cfg.Decider.OnDeactivation, []string{"switch", "usb"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)
	waitForLive(t, s, "linux")

	// Nothing about the new owner is checked: not live, no cooldown.
	if _, err := client.Claim(context.Background(), base, "other", true, false); err != nil {
		t.Fatalf("force claim other: %v", err)
	}

	waitForLastOwner(t, statePath, "other")
	if calls := runner.Calls(); !slices.EqualFunc(calls, cfg.Decider.OnDeactivation, slices.Equal[[]string]) {
		t.Fatalf("calls = %v, want the deactivation commands %v in order", calls, cfg.Decider.OnDeactivation)
	}
}

// Ownership coming to this host runs --on-activation, and only that: the
// deactivation commands are for the host that is left (SPEC §4.3).
func TestAgentActivationPath(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	statePath := filepath.Join(t.TempDir(), "agent.json")
	seedOwner(t, base, "other", statePath, "other")

	det := newFakeDetector()
	runner := &recordingRunner{}
	cfg := agentTestConfig(t, base, statePath, s, det, &fakeGuard{ok: true}, runner.run)
	cfg.Decider.OnActivation = [][]string{{"activate", "cmd"}, {"activate", "usb"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)
	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "linux", true, false); err != nil {
		t.Fatalf("force claim linux: %v", err)
	}

	waitForLastOwner(t, statePath, "linux")
	if calls := runner.Calls(); !slices.EqualFunc(calls, sortedCalls(cfg.Decider.OnActivation), slices.Equal[[]string]) {
		t.Errorf("calls = %v, want exactly the activation commands %v", calls, cfg.Decider.OnActivation)
	}
}

// `activate` against the current owner re-runs its -A commands, and an
// attach-driven claim does not (SPEC §4.3).
func TestAgentReactivation(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	statePath := filepath.Join(t.TempDir(), "agent.json")
	seedOwner(t, base, "linux", statePath, "linux")

	det := newFakeDetector()
	runner := &recordingRunner{}
	cfg := agentTestConfig(t, base, statePath, s, det, &fakeGuard{ok: true}, runner.run)
	cfg.Decider.OnActivation = [][]string{{"activate", "cmd"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)
	waitForLive(t, s, "linux")

	// A receiver flap claims the bit it already holds: nothing runs.
	det.attach()
	time.Sleep(300 * time.Millisecond)
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("an attach-driven claim ran %v", calls)
	}

	if _, err := client.Claim(context.Background(), base, "linux", false, true); err != nil {
		t.Fatalf("retake: %v", err)
	}
	waitFor(t, "the activation commands run again", func() bool { return len(runner.Calls()) == 1 })
	waitForRecord(t, statePath, func(as state.AgentState) bool { return as.HandledActivation == 2 })
}

// A retry re-runs only the commands that failed: the peripheral the second
// command already moved must not be chased again (SPEC §4.3).
func TestAgentRetryRunsOnlyFailedCommands(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	statePath := filepath.Join(t.TempDir(), "agent.json")
	seedOwner(t, base, "linux", statePath, "linux")

	det := newFakeDetector()
	// Attempt 1: switch display fails, switch usb succeeds. Attempt 2: switch
	// display succeeds.
	runner := &recordingRunner{fails: map[string][]error{"switch display": {errors.New("monitor still here")}}}
	cfg := agentTestConfig(t, base, statePath, s, det, &fakeGuard{ok: true}, runner.run)
	cfg.Decider.OnDeactivation = [][]string{{"switch", "display"}, {"switch", "usb"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)
	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "other", true, false); err != nil {
		t.Fatalf("force claim other: %v", err)
	}

	waitForLastOwner(t, statePath, "other")
	want := sortedCalls([][]string{cfg.Decider.OnDeactivation[0], cfg.Decider.OnDeactivation[1], cfg.Decider.OnDeactivation[0]})
	if calls := runner.Calls(); !slices.EqualFunc(calls, want, slices.Equal[[]string]) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// take-screen and take-hid are built in: they post this host's declarations
// and are never exec'd as programs (SPEC §5.4).
func TestAgentTakeCommandsAreBuiltIn(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)

	statePath := filepath.Join(t.TempDir(), "agent.json")
	seedOwner(t, base, "other", statePath, "other")

	runner := &recordingRunner{}
	cfg := agentTestConfig(t, base, statePath, s, newFakeDetector(), &fakeGuard{ok: true}, runner.run)
	cfg.Decider.OnActivation = [][]string{{"take-screen"}, {"take-hid"}, {"after", "takes"}}
	cfg.Screens = []screen.Declaration{{Name: "LG HDR WQHD", Write: screen.Write{VCP: 0xF4, Source: 0x50, Value: 0xD0}}}
	cfg.HIDs = []hidpp.Declaration{
		{Name: "keyboard", Target: hidpp.Target{VID: 0x046d, PID: 0xc548, Slot: 1}, Channel: 1},
		{Name: "mouse", Target: hidpp.Target{VID: 0x046d, PID: 0xc548, Slot: 2}, Channel: 1},
	}
	// The keyboard carried the gesture and is already here; the mouse is not.
	cfg.HIDPresent = func(_ context.Context, tg hidpp.Target) (bool, error) { return tg.Slot == 1, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)
	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "linux", true, false); err != nil {
		t.Fatalf("force claim linux: %v", err)
	}
	waitForLastOwner(t, statePath, "linux")

	st, err := client.State(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	want := []state.Assignment{
		{Kind: "hid", Name: "keyboard", Assignee: "linux", Seq: st.Assignments[0].Seq, Channel: 1, Arrived: true},
		{Kind: "hid", Name: "mouse", Assignee: "linux", Seq: st.Assignments[1].Seq, Channel: 1},
		{Kind: "screen", Name: "LG HDR WQHD", Assignee: "linux", Seq: st.Assignments[2].Seq, Input: "F4:50=D0"},
	}
	if !slices.Equal(st.Assignments, want) {
		t.Fatalf("assignments = %+v\nwant    %+v", st.Assignments, want)
	}
	if calls := runner.Calls(); !slices.EqualFunc(calls, [][]string{{"after", "takes"}}, slices.Equal[[]string]) {
		t.Fatalf("exec'd %v, want only the ordinary command", calls)
	}
}

// A take under an activation that is no longer current is refused, and the
// command fails (SPEC §4.3).
func TestAgentTakeUnderStaleActivationFails(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	seedOwner(t, base, "linux", filepath.Join(t.TempDir(), "agent.json"), "linux")

	cfg := agentTestConfig(t, base, filepath.Join(t.TempDir(), "agent.json"), s, newFakeDetector(), &fakeGuard{ok: true}, (&recordingRunner{}).run)
	cfg.Screens = []screen.Declaration{{Name: "LG", Write: screen.Write{VCP: 0x60, Source: 0x51, Value: 0x0F}}}
	a := &agent{cfg: cfg}

	if err := a.runCommand(context.Background(), model.Command{TakeScreen: 99}); !errors.Is(err, client.ErrNotOwner) {
		t.Fatalf("take under a stale activation: %v, want ErrNotOwner", err)
	}
	if err := a.runCommand(context.Background(), model.Command{TakeScreen: 1}); err != nil {
		t.Fatalf("take under the current activation: %v", err)
	}
	if st := s.CurrentState(); len(st.Assignments) != 1 || st.Assignments[0].Input != "60:51=0F" {
		t.Fatalf("assignments = %+v", st.Assignments)
	}
}

// fakeSeams records the writes and sends, and answers from canned results.
type fakeSeams struct {
	mu       sync.Mutex
	writes   []string // "NAME VCP:SRC=VAL"
	sends    []string // "VID:PID:SLOT channel"
	writeErr error
	hang     bool // writes block until their context is done
	sendErr  error
	sendHere bool            // the device is linked to this host before the send
	present  map[string]bool // by target string
}

func (f *fakeSeams) writeScreen(ctx context.Context, w screen.Write, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, name+" "+w.String())
	if f.hang {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return ctx.Err()
	}
	return f.writeErr
}

func (f *fakeSeams) sendHID(_ context.Context, tg hidpp.Target, channel uint8) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, tg.String()+" "+string(rune('0'+channel)))
	return f.sendHere, f.sendErr
}

func (f *fakeSeams) here(_ context.Context, tg hidpp.Target) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.present[tg.String()], nil
}

func (f *fakeSeams) setPresent(tg hidpp.Target, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.present == nil {
		f.present = map[string]bool{}
	}
	f.present[tg.String()] = v
}

func (f *fakeSeams) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

func (f *fakeSeams) Sends() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sends)
}

func TestGiveAwayAssignment(t *testing.T) {
	mouse := hidpp.Declaration{Name: "mouse", Target: hidpp.Target{VID: 0x046d, PID: 0xc548, Slot: 2}, Channel: 1}
	lg := state.Assignment{Kind: state.KindScreen, Name: "LG HDR WQHD", Assignee: "mac", Seq: 3, Input: "F4:50=D1"}
	mouseAssignment := state.Assignment{Kind: state.KindHID, Name: "mouse", Assignee: "mac", Seq: 3, Channel: 3}

	cases := []struct {
		name       string
		assignment state.Assignment
		seams      *fakeSeams
		want       error
		check      func(t *testing.T, f *fakeSeams)
	}{
		{name: "a screen is written, as the wire says", assignment: lg,
			check: func(t *testing.T, f *fakeSeams) {
				if w := f.Writes(); !slices.Equal(w, []string{"LG HDR WQHD F4:50=D1"}) {
					t.Errorf("writes = %q", w)
				}
			}},
		{name: "no monitor of that name is nothing to move", assignment: lg,
			seams: &fakeSeams{writeErr: screen.ErrNoDisplay}, want: model.ErrNotHere},
		{name: "a write that fails is an error", assignment: lg,
			seams: &fakeSeams{writeErr: errors.New("ddcutil missing")}, want: errors.New("ddcutil missing")},
		{name: "a malformed input is an error and writes nothing", assignment: state.Assignment{Kind: state.KindScreen, Name: "LG", Input: "nonsense"},
			want: errors.New("invalid write")},
		{name: "a pair no table lists is an error and writes nothing", assignment: state.Assignment{Kind: state.KindScreen, Name: "LG", Input: "04:51=01"},
			want: errors.New("refusing write"),
			check: func(t *testing.T, f *fakeSeams) {
				if w := f.Writes(); len(w) != 0 {
					t.Errorf("wrote %q", w)
				}
			}},
		{name: "a device is sent to the assignee's channel", assignment: mouseAssignment, seams: &fakeSeams{sendHere: true},
			check: func(t *testing.T, f *fakeSeams) {
				if s := f.Sends(); !slices.Equal(s, []string{"046d:c548:2 3"}) {
					t.Errorf("sends = %q", s)
				}
			}},
		{name: "a device not linked here is nothing to move", assignment: mouseAssignment, want: model.ErrNotHere},
		{name: "a device with no local declaration is nothing to move",
			assignment: state.Assignment{Kind: state.KindHID, Name: "pen", Seq: 3, Channel: 1}, want: model.ErrNotHere,
			check: func(t *testing.T, f *fakeSeams) {
				if s := f.Sends(); len(s) != 0 {
					t.Errorf("sent %q", s)
				}
			}},
		{name: "a send that fails is an error", assignment: mouseAssignment,
			seams: &fakeSeams{sendHere: true, sendErr: errors.New("EACCES")}, want: errors.New("EACCES")},
		{name: "an unknown kind is an error", assignment: state.Assignment{Kind: "led", Name: "x"}, want: errors.New("unknown kind")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.seams
			if f == nil {
				f = &fakeSeams{}
			}
			cfg := Config{WriteScreen: f.writeScreen, SendHID: f.sendHID, SwitchTimeout: time.Second}
			a := &agent{cfg: cfg, hids: map[string]hidpp.Declaration{"mouse": mouse}}
			got := a.giveAwayAssignment(context.Background(), c.assignment)
			switch {
			case c.want == nil || errors.Is(c.want, model.ErrNotHere):
				if !errors.Is(got, c.want) {
					t.Fatalf("error = %v, want %v", got, c.want)
				}
			case got == nil || errors.Is(got, model.ErrNotHere) || !strings.Contains(got.Error(), c.want.Error()):
				t.Fatalf("error = %v, want it to contain %q", got, c.want)
			}
			if c.check != nil {
				c.check(t, f)
			}
		})
	}
}

// A hung write is killed by --switch-timeout and reported as a failure, so one
// stuck monitor cannot freeze the agent (SPEC §4.3).
func TestGiveAwayAssignmentTimesOut(t *testing.T) {
	f := &fakeSeams{hang: true}
	a := &agent{cfg: Config{WriteScreen: f.writeScreen, SwitchTimeout: 30 * time.Millisecond}}
	got := a.giveAwayAssignment(context.Background(), state.Assignment{Kind: state.KindScreen, Name: "LG", Input: "60:51=0F"})
	if !errors.Is(got, errSwitchTimeout) {
		t.Fatalf("error = %v, want errSwitchTimeout", got)
	}
}

// A batch is given away in parallel: the devices do not wait behind a hung
// monitor write, and two devices behind one receiver are sent one after the
// other.
func TestGiveAwayRunsInParallel(t *testing.T) {
	release := make(chan struct{})
	sent := make(chan string, 2)
	var inFlight, overlapped atomic.Int32
	a := &agent{
		cfg: Config{
			SwitchTimeout: time.Minute,
			WriteScreen: func(context.Context, screen.Write, string) error {
				<-release
				return nil
			},
			SendHID: func(_ context.Context, tg hidpp.Target, _ uint8) (bool, error) {
				if inFlight.Add(1) > 1 {
					overlapped.Store(1)
				}
				time.Sleep(10 * time.Millisecond)
				inFlight.Add(-1)
				sent <- tg.String()
				return true, nil
			},
		},
		hids: map[string]hidpp.Declaration{
			"keyboard": {Name: "keyboard", Target: hidpp.Target{VID: 0x046d, PID: 0xc548, Slot: 1}, Channel: 1},
			"mouse":    {Name: "mouse", Target: hidpp.Target{VID: 0x046d, PID: 0xc548, Slot: 2}, Channel: 1},
		},
	}
	batch := []state.Assignment{
		{Kind: state.KindScreen, Name: "LG", Assignee: "mac", Seq: 3, Input: "F4:50=D1"},
		{Kind: state.KindHID, Name: "keyboard", Assignee: "mac", Seq: 3, Channel: 2},
		{Kind: state.KindHID, Name: "mouse", Assignee: "mac", Seq: 3, Channel: 2},
	}
	done := make(chan map[state.AssignmentKey]error)
	go func() { done <- a.giveAwayAll(context.Background(), batch) }()
	for range 2 {
		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Fatal("the devices waited behind the monitor write")
		}
	}
	close(release)
	for key, err := range <-done {
		if err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	if overlapped.Load() != 0 {
		t.Fatal("two sends overlapped on one receiver")
	}
}

// The whole handover, with two real agents on a real server: the Mac claims,
// Linux deactivates and then gives away the Mac's assignments against its
// fakes, and the Mac's arrival watcher reports the mouse once it links (SPEC
// §4.1).
func TestAgentHandover(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)
	dir := t.TempDir()
	linuxState, macState := filepath.Join(dir, "linux.json"), filepath.Join(dir, "mac.json")
	seedOwner(t, base, "linux", linuxState, "linux")
	if err := state.Save(macState, state.AgentState{HandledOwner: "linux", HandledActivation: 1}); err != nil {
		t.Fatal(err)
	}

	lgOnMac := screen.Write{VCP: 0xF4, Source: 0x50, Value: 0xD1}
	mouseOnMac := hidpp.Target{VID: 0x046d, PID: 0xb035}
	mouseOnLinux := hidpp.Target{VID: 0x046d, PID: 0xc548, Slot: 2}

	linuxSeams, macSeams := &fakeSeams{sendHere: true}, &fakeSeams{}
	linuxRunner, macRunner := &recordingRunner{}, &recordingRunner{}

	linux := agentTestConfig(t, base, linuxState, s, newFakeDetector(), &fakeGuard{ok: true}, linuxRunner.run)
	linux.Decider.OnDeactivation = [][]string{{"notify", "left"}}
	linux.WriteScreen, linux.SendHID, linux.HIDPresent = linuxSeams.writeScreen, linuxSeams.sendHID, linuxSeams.here
	linux.HIDs = []hidpp.Declaration{{Name: "mouse", Target: mouseOnLinux, Channel: 1}}

	mac := agentTestConfig(t, base, macState, s, newFakeDetector(), &fakeGuard{ok: true}, macRunner.run)
	mac.ID = "mac"
	mac.Decider = agentDeciderConfig("mac")
	mac.Decider.OnDeactivation = nil
	mac.Decider.OnActivation = [][]string{{"take-screen"}, {"take-hid"}}
	mac.Screens = []screen.Declaration{{Name: "LG HDR WQHD", Write: lgOnMac}}
	mac.HIDs = []hidpp.Declaration{{Name: "mouse", Target: mouseOnMac, Channel: 3}}
	mac.WriteScreen, mac.SendHID, mac.HIDPresent = macSeams.writeScreen, macSeams.sendHID, macSeams.here
	mac.ArrivalPoll, mac.ArrivalFor = 10*time.Millisecond, 5*time.Second
	mac.Resolver = discover.NewResolver(filepath.Join(t.TempDir(), "server"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, linux)
	runAgent(ctx, t, mac)
	waitForLive(t, s, "linux")
	waitForLive(t, s, "mac")

	if _, err := client.Claim(ctx, base, "mac", false, false); err != nil {
		t.Fatalf("mac claim: %v", err)
	}

	// Linux gives the monitor and the mouse to the Mac's input and channel.
	waitFor(t, "linux writes the monitor", func() bool { return len(linuxSeams.Writes()) == 1 })
	waitFor(t, "linux sends the mouse", func() bool { return len(linuxSeams.Sends()) == 1 })
	if w := linuxSeams.Writes(); w[0] != "LG HDR WQHD F4:50=D1" {
		t.Errorf("linux wrote %q", w)
	}
	if sd := linuxSeams.Sends(); sd[0] != "046d:c548:2 3" {
		t.Errorf("linux sent %q", sd)
	}
	if calls := linuxRunner.Calls(); !slices.EqualFunc(calls, [][]string{{"notify", "left"}}, slices.Equal[[]string]) {
		t.Errorf("linux deactivation ran %v", calls)
	}
	if w := macSeams.Writes(); len(w) != 0 {
		t.Errorf("the taker wrote the monitor: %q", w)
	}

	// The mouse links on the Mac; its watcher reports it.
	macSeams.setPresent(mouseOnMac, true)
	waitFor(t, "the mouse assignment arrives", func() bool {
		for _, h := range s.CurrentState().Assignments {
			if h.Kind == state.KindHID && h.Name == "mouse" && h.Arrived && h.Assignee == "mac" && h.Prev == "" {
				return true
			}
		}
		return false
	})

	// Linux marks both assignments handled, and sends nothing more.
	waitForRecord(t, linuxState, func(as state.AgentState) bool {
		return as.HandledOwner == "mac" && len(as.HandledSeqs) == 2
	})
	time.Sleep(200 * time.Millisecond)
	if n := len(linuxSeams.Sends()); n != 1 {
		t.Errorf("linux sent the mouse %d times, want 1", n)
	}
	if n := len(linuxSeams.Writes()); n != 1 {
		t.Errorf("linux wrote the monitor %d times, want 1", n)
	}
}

func TestArrivalWatcher(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)
	if _, err := client.Claim(context.Background(), base, "linux", true, false); err != nil {
		t.Fatal(err)
	}
	mouseT := hidpp.Target{VID: 0x046d, PID: 0xc548, Slot: 2}
	if err := client.Take(context.Background(), base, "linux", 1, []state.Take{
		{Kind: state.KindHID, Name: "mouse", Channel: 1},
		{Kind: state.KindHID, Name: "pen", Channel: 2}, // no local declaration
	}); err != nil {
		t.Fatal(err)
	}

	var polls atomic.Int32
	var present atomic.Bool
	cfg := agentTestConfig(t, base, filepath.Join(t.TempDir(), "agent.json"), s, newFakeDetector(), &fakeGuard{ok: true}, (&recordingRunner{}).run)
	cfg.HIDs = []hidpp.Declaration{{Name: "mouse", Target: mouseT, Channel: 1}}
	cfg.HIDPresent = func(_ context.Context, tg hidpp.Target) (bool, error) {
		if tg != mouseT {
			t.Errorf("polled %v, want only the declared mouse", tg)
		}
		polls.Add(1)
		return present.Load(), nil
	}
	cfg.ArrivalPoll, cfg.ArrivalFor = 10*time.Millisecond, 400*time.Millisecond
	a := &agent{cfg: cfg, hids: map[string]hidpp.Declaration{"mouse": cfg.HIDs[0]}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	states := make(chan *state.ServerState, 1)
	go func() { _ = a.arrivalWatch(ctx, states) }()
	st := s.CurrentState()
	states <- &st

	waitFor(t, "the watcher polls", func() bool { return polls.Load() >= 3 })
	for _, h := range s.CurrentState().Assignments {
		if h.Arrived {
			t.Fatalf("%s arrived before the device did", h.Name)
		}
	}

	present.Store(true)
	waitFor(t, "the mouse is reported", func() bool {
		for _, h := range s.CurrentState().Assignments {
			if h.Name == "mouse" {
				return h.Arrived
			}
		}
		return false
	})
	// Reported once, then not polled again for that seq.
	n := polls.Load()
	time.Sleep(100 * time.Millisecond)
	if got := polls.Load(); got > n+1 {
		t.Errorf("polling went on after the arrival: %d -> %d", n, got)
	}
}

func TestArrivalWatcherGivesUp(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)
	client := newTestClient(t, testToken)
	if _, err := client.Claim(context.Background(), base, "linux", true, false); err != nil {
		t.Fatal(err)
	}
	if err := client.Take(context.Background(), base, "linux", 1, []state.Take{{Kind: state.KindHID, Name: "mouse", Channel: 1}}); err != nil {
		t.Fatal(err)
	}

	var polls atomic.Int32
	cfg := agentTestConfig(t, base, filepath.Join(t.TempDir(), "agent.json"), s, newFakeDetector(), &fakeGuard{ok: true}, (&recordingRunner{}).run)
	cfg.HIDPresent = func(context.Context, hidpp.Target) (bool, error) { polls.Add(1); return false, nil }
	cfg.ArrivalPoll, cfg.ArrivalFor = 10*time.Millisecond, 100*time.Millisecond
	a := &agent{cfg: cfg, hids: map[string]hidpp.Declaration{"mouse": {Name: "mouse"}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	states := make(chan *state.ServerState, 1)
	go func() { _ = a.arrivalWatch(ctx, states) }()
	st := s.CurrentState()
	states <- &st

	time.Sleep(400 * time.Millisecond)
	n := polls.Load()
	if n == 0 {
		t.Fatal("never polled")
	}
	time.Sleep(100 * time.Millisecond)
	if got := polls.Load(); got != n {
		t.Errorf("still polling %v after the window: %d -> %d", cfg.ArrivalFor, n, got)
	}
	// A new take of the name starts a new window.
	if err := client.Take(context.Background(), base, "linux", 1, []state.Take{{Kind: state.KindHID, Name: "mouse", Channel: 2}}); err != nil {
		t.Fatal(err)
	}
	st = s.CurrentState()
	states <- &st
	waitFor(t, "polling resumes for the new take", func() bool { return polls.Load() > n })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForRecord(t *testing.T, statePath string, ok func(state.AgentState) bool) {
	t.Helper()
	var as state.AgentState
	deadline := time.Now().Add(3 * time.Second)
	for {
		as = state.AgentState{}
		if err := state.Load(statePath, &as); err == nil && ok(as) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent state never reached the expected record (got %+v)", as)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForLastOwner(t *testing.T, statePath, want string) {
	t.Helper()
	waitForRecord(t, statePath, func(as state.AgentState) bool { return as.HandledOwner == want })
}

func TestAgentGuardsDown(t *testing.T) {
	s, srv := newAgentTestServer(t)
	defer srv.Close()
	base := agentTestBase(t, srv.URL)

	statePath := filepath.Join(t.TempDir(), "agent.json")
	seedOwner(t, base, "other", statePath, "linux")

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

	statePath := filepath.Join(t.TempDir(), "agent.json")
	seedOwner(t, base, "linux", statePath, "linux")

	det := newFakeDetector()
	runner := newHungSwitchRunner()
	cfg := agentTestConfig(t, base, statePath, s, det, &fakeGuard{ok: true}, runner.run)
	cfg.SwitchTimeout = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, t, cfg)
	waitForLive(t, s, "linux")

	if _, err := client.Claim(context.Background(), base, "other", true, false); err != nil {
		t.Fatalf("force claim other: %v", err)
	}

	// Wait for the first switch to be attempted.
	select {
	case <-runner.called:
	case <-time.After(2 * time.Second):
		t.Fatal("switch runner not called")
	}

	// The agent must stay responsive: the timeout produces a result and the
	// decider retries (SwitchRetries=1), so a second switch call arrives.
	waitFor(t, "a second switch call", func() bool { return runner.SwitchCalls() >= 2 })
	if !runner.firstCtxCanceled() {
		t.Error("first switch context was not canceled by timeout")
	}
}

func TestTimingOrdering(t *testing.T) {
	if server.DefaultWaitTimeout >= client.WaitClientTimeout || client.WaitClientTimeout >= waitTimeout {
		t.Fatalf("wait timeouts out of order: server=%v client=%v agent=%v",
			server.DefaultWaitTimeout, client.WaitClientTimeout, waitTimeout)
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

// A declaration without host=N is watched and given away, never taken:
// take-hid does not post it.
func TestHIDTakesSkipDevicesWithoutChannel(t *testing.T) {
	a := &agent{cfg: Config{HIDs: []hidpp.Declaration{
		{Name: "keyboard", Target: hidpp.Target{VID: 0x1532, PID: 0x0285}},
		{Name: "mouse", Target: hidpp.Target{VID: 0x046d, PID: 0xc548, Slot: 2}, Channel: 2},
	}}}
	got := a.hidTakes(context.Background())
	want := []state.Take{{Kind: state.KindHID, Name: "mouse", Channel: 2}}
	if !slices.Equal(got, want) {
		t.Errorf("hidTakes = %+v, want %+v", got, want)
	}
}
