// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// machine_test.go: table-driven tests of the pure switch state machine.

package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/unbrice/soft-kvm/state"
)

func testConfig(id string) MachineConfig {
	return MachineConfig{
		ID:             id,
		Settle:         2 * time.Second,
		SwitchRetries:  3,
		RetrySpacing:   1 * time.Second,
		Cooldown:       5 * time.Second,
		BreakerWindow:  30 * time.Second,
		BreakerMax:     3,
		BreakerOpenFor: 60 * time.Second,
	}
}

func mkstate(owner string, epoch int64, live map[string]bool) *state.ServerState {
	return &state.ServerState{Owner: owner, Epoch: epoch, Live: live}
}

func exit(err error) *error { return &err }

func ptr(s string) *string { return &s }

func checkActions(t *testing.T, got, want []Action) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d actions, want %d; got=%+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if w.Claim != "" && g.Claim != w.Claim {
			t.Errorf("action %d Claim=%q, want %q", i, g.Claim, w.Claim)
		}
		if g.Switch != w.Switch {
			t.Errorf("action %d Switch=%v, want %v", i, g.Switch, w.Switch)
		}
		if g.Retry != w.Retry {
			t.Errorf("action %d Retry=%v, want %v", i, g.Retry, w.Retry)
		}
		if g.Notify != w.Notify {
			t.Errorf("action %d Notify=%v, want %v", i, g.Notify, w.Notify)
		}
		if !w.WakeAt.IsZero() && !g.WakeAt.Equal(w.WakeAt) {
			t.Errorf("action %d WakeAt=%v, want %v", i, g.WakeAt, w.WakeAt)
		}
		if w.SaveOwner != nil {
			if g.SaveOwner == nil || *g.SaveOwner != *w.SaveOwner {
				t.Errorf("action %d SaveOwner=%v, want %v", i, deref(g.SaveOwner), *w.SaveOwner)
			}
		} else if g.SaveOwner != nil {
			t.Errorf("action %d SaveOwner=%v, want nil", i, deref(g.SaveOwner))
		}
		if w.Log != "" && !strings.Contains(g.Log, w.Log) {
			t.Errorf("action %d Log=%q, want substring %q", i, g.Log, w.Log)
		}
	}
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func requireActionCount(t *testing.T, acts []Action, n int) {
	t.Helper()
	if len(acts) != n {
		t.Fatalf("got %d actions, want %d: %+v", len(acts), n, acts)
	}
}

func TestMachine(t *testing.T) {
	t0 := time.Unix(1000, 0)
	errStuck := errors.New("screen-switch: monitor still on this input")
	macLive := map[string]bool{"mac": true}

	t.Run("first reconcile adopts and never switches", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "", false)

		acts := m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})
		checkActions(t, acts, []Action{{SaveOwner: ptr("mac"), Log: "adopted"}})

		// The adopted owner is now treated as our own: no transition.
		acts = m.Step(Event{Now: t0, State: mkstate("mac", 2, macLive)})
		checkActions(t, acts, []Action{{Log: "last_owner"}})
	})

	t.Run("happy path", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		// The transition switches at once: nothing probes first.
		acts := m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "attempt 1"},
			{WakeAt: t0.Add(SwitchDeadline)},
		})

		// Exit 0 is the receipt.
		end := t0.Add(2 * time.Second)
		acts = m.Step(Event{Now: end, SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{{SaveOwner: ptr("mac"), Log: "confirmed"}})
		if !m.cooldownUntil.Equal(end.Add(cfg.Cooldown)) {
			t.Fatalf("cooldown set to %v", m.cooldownUntil)
		}
	})

	t.Run("winner not live", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		acts := m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": false})})
		requireActionCount(t, acts, 1)
		if acts[0].Switch {
			t.Fatalf("expected no switch, got %+v", acts[0])
		}
		if !strings.Contains(acts[0].Log, "not live") {
			t.Fatalf("expected log mentioning not live, got %q", acts[0].Log)
		}
	})

	t.Run("owner empty", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		acts := m.Step(Event{Now: t0, State: mkstate("", 1, map[string]bool{})})
		requireActionCount(t, acts, 1)
		if !strings.Contains(acts[0].Log, "empty") {
			t.Fatalf("expected log mentioning empty owner, got %q", acts[0].Log)
		}
	})

	t.Run("failed switch retries at once past the retry spacing", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})

		// The command spent its own confirm window before failing, so the
		// spacing has already passed.
		fail := t0.Add(5 * time.Second)
		acts := m.Step(Event{Now: fail, SwitchExit: exit(errStuck)})
		checkActions(t, acts, []Action{
			{Switch: true, Retry: true, Log: "attempt 2"},
			{WakeAt: fail.Add(SwitchDeadline)},
		})
		if !strings.Contains(acts[0].Log, errStuck.Error()) {
			t.Fatalf("retry log lost the command's error: %q", acts[0].Log)
		}

		// The second attempt lands.
		acts = m.Step(Event{Now: fail.Add(time.Second), SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{{SaveOwner: ptr("mac"), Log: "confirmed"}})
	})

	t.Run("fast failure waits for the retry spacing", func(t *testing.T) {
		cfg := testConfig("linux")
		cfg.SwitchRetries = 1
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})

		fail1 := t0.Add(200 * time.Millisecond)
		acts := m.Step(Event{Now: fail1, SwitchExit: exit(errors.New("switch error"))})
		retryAt := t0.Add(cfg.RetrySpacing)
		checkActions(t, acts, []Action{
			{Log: "waiting retry spacing"},
			{WakeAt: retryAt},
		})

		acts = m.Step(Event{Now: retryAt})
		checkActions(t, acts, []Action{
			{Switch: true, Retry: true, Log: "retry switch to \"mac\" attempt 2"},
			{WakeAt: retryAt.Add(SwitchDeadline)},
		})

		fail2 := retryAt.Add(200 * time.Millisecond)
		acts = m.Step(Event{Now: fail2, SwitchExit: exit(errors.New("switch error 2"))})
		checkActions(t, acts, []Action{
			{Notify: true, SaveOwner: ptr("mac"), Log: "failed"},
		})
	})

	t.Run("all retries exhausted then notify and stop", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})

		at := t0
		for i := 0; i < 1+cfg.SwitchRetries; i++ {
			at = at.Add(5 * time.Second)
			acts := m.Step(Event{Now: at, SwitchExit: exit(errStuck)})
			if i < cfg.SwitchRetries {
				if !acts[0].Switch || !acts[0].Retry {
					t.Fatalf("attempt %d: wanted retry switch, got %+v", i+1, acts)
				}
				continue
			}
			checkActions(t, acts, []Action{{Notify: true, SaveOwner: ptr("mac"), Log: "failed"}})
		}

		// Same transition afterwards is a no-op: last_owner is already mac.
		acts := m.Step(Event{Now: at.Add(1 * time.Second), State: mkstate("mac", 2, macLive)})
		requireActionCount(t, acts, 1)
		if acts[0].Switch || acts[0].Notify {
			t.Fatalf("expected no-op after failure, got %+v", acts[0])
		}
	})

	t.Run("cooldown defers then proceeds", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		// Complete a switch to mac at t0+3s.
		m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})
		m.Step(Event{Now: t0.Add(3 * time.Second), SwitchExit: exit(nil)})

		// Resync: the server now says we are the owner again.
		m.Step(Event{Now: t0.Add(4 * time.Second), State: mkstate("linux", 2, map[string]bool{"linux": true})})

		// A new transition inside the cooldown is deferred.
		cooldownEnd := t0.Add(3 * time.Second).Add(cfg.Cooldown)
		acts := m.Step(Event{Now: t0.Add(4 * time.Second), State: mkstate("win2", 3, map[string]bool{"win2": true})})
		checkActions(t, acts, []Action{
			{Log: "deferring"},
			{WakeAt: cooldownEnd},
		})

		// After the cooldown, the bare timer re-runs the reconcile and switches.
		acts = m.Step(Event{Now: cooldownEnd})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "attempt 1"},
			{WakeAt: cooldownEnd.Add(SwitchDeadline)},
		})
	})

	t.Run("circuit breaker trips and reopens", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		owners := []string{"mac", "win2", "win3", "win4"}
		base := t0
		// Three completed switch sequences inside the breaker window.
		for i := 0; i < 3; i++ {
			m.Step(Event{Now: base, State: mkstate(owners[i], int64(i), map[string]bool{owners[i]: true})})
			m.Step(Event{Now: base.Add(3 * time.Second), SwitchExit: exit(nil)})
			base = base.Add(3*time.Second + cfg.Cooldown)

			// Resync back to linux so the next transition is away from us.
			if i < 2 {
				m.Step(Event{Now: base, State: mkstate("linux", int64(i)+10, map[string]bool{"linux": true})})
			}
		}

		// Resync once more after the third success.
		m.Step(Event{Now: base, State: mkstate("linux", 20, map[string]bool{"linux": true})})

		// The fourth transition is refused while the breaker is open.
		// The breaker opened at the last success, which was one cooldown ago.
		breakerEnd := base.Add(-cfg.Cooldown).Add(cfg.BreakerOpenFor)
		acts := m.Step(Event{Now: base, State: mkstate(owners[3], 3, map[string]bool{owners[3]: true})})
		checkActions(t, acts, []Action{
			{Log: "breaker"},
			{WakeAt: breakerEnd},
		})

		// Once the breaker closes, the deferred reconcile proceeds.
		acts = m.Step(Event{Now: breakerEnd})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "attempt 1"},
			{WakeAt: breakerEnd.Add(SwitchDeadline)},
		})
	})

	t.Run("attach settle claim", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		acts := m.Step(Event{Now: t0, Attach: true})
		requireActionCount(t, acts, 1)
		if !acts[0].WakeAt.Equal(t0.Add(cfg.Settle)) {
			t.Fatalf("expected wake at %v, got %v", t0.Add(cfg.Settle), acts[0].WakeAt)
		}

		acts = m.Step(Event{Now: t0.Add(cfg.Settle)})
		checkActions(t, acts, []Action{{Claim: "linux", Log: "claim"}})
	})

	t.Run("attach flapping restarts settle", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, Attach: true})
		m.Step(Event{Now: t0.Add(1 * time.Second), Attach: true})
		m.Step(Event{Now: t0.Add(2 * time.Second), Attach: true})

		acts := m.Step(Event{Now: t0.Add(2*time.Second + cfg.Settle)})
		checkActions(t, acts, []Action{{Claim: "linux"}})
	})

	t.Run("settle survives a switch sequence", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, Attach: true}) // settle deadline at t0+2s

		// A state arrives mid-settle and starts a switch; the settle deadline
		// stays the earliest wake.
		acts := m.Step(Event{Now: t0.Add(1 * time.Second), State: mkstate("mac", 1, macLive)})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "attempt 1"},
			{WakeAt: t0.Add(cfg.Settle)},
		})

		// The settle deadline passes while switching: no claim mid-sequence.
		acts = m.Step(Event{Now: t0.Add(cfg.Settle)})
		checkActions(t, acts, []Action{{WakeAt: t0.Add(1*time.Second + SwitchDeadline)}})

		// The interrupted settle fires the claim once the sequence ends (SPEC
		// §5.4: attach→claim is an independent trigger).
		acts = m.Step(Event{Now: t0.Add(3 * time.Second), SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{
			{SaveOwner: ptr("mac"), Log: "confirmed"},
			{Claim: "linux"},
		})
	})

	t.Run("attach during sequence queued", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})

		// Attach while switching: queued, the watchdog stays the only wake.
		acts := m.Step(Event{Now: t0.Add(500 * time.Millisecond), Attach: true})
		checkActions(t, acts, []Action{{WakeAt: t0.Add(SwitchDeadline)}})

		// The switch lands; the pending attach starts its settle.
		end := t0.Add(1500 * time.Millisecond)
		acts = m.Step(Event{Now: end, SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{
			{SaveOwner: ptr("mac"), Log: "confirmed"},
			{WakeAt: end.Add(cfg.Settle)},
		})

		acts = m.Step(Event{Now: end.Add(cfg.Settle)})
		checkActions(t, acts, []Action{{Claim: "linux"}})
	})

	t.Run("state event during switching is stashed", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})

		acts := m.Step(Event{Now: t0.Add(2 * time.Second), State: mkstate("win2", 2, map[string]bool{"win2": true})})
		checkActions(t, acts, []Action{
			{WakeAt: t0.Add(SwitchDeadline)},
		})
		if m.latestState == nil || m.latestState.Owner != "win2" {
			t.Fatalf("expected latestState stashed to win2, got %+v", m.latestState)
		}

		// The sequence ends; the stashed state reconciles, behind the cooldown
		// the switch just started.
		end := t0.Add(3 * time.Second)
		acts = m.Step(Event{Now: end, SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{
			{SaveOwner: ptr("mac"), Log: "confirmed"},
			{Log: "deferring switch to \"win2\""},
			{WakeAt: end.Add(cfg.Cooldown)},
		})
	})

	t.Run("stale epoch still reconciles", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		// A state with a lower epoch but a different owner is still a valid
		// input: clients never compare epochs (SPEC §7).
		m.Step(Event{Now: t0, State: mkstate("linux", 6, map[string]bool{"linux": true})})
		acts := m.Step(Event{Now: t0.Add(1 * time.Second), State: mkstate("win2", 2, map[string]bool{"win2": true})})
		requireActionCount(t, acts, 2)
		if !acts[0].Switch {
			t.Fatalf("expected switch for lower-epoch win2, got %+v", acts[0])
		}
	})

	t.Run("switch watchdog fires and retries", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})

		watchdogTime := t0.Add(SwitchDeadline)
		acts := m.Step(Event{Now: watchdogTime})
		checkActions(t, acts, []Action{
			{Switch: true, Retry: true, Log: "no SwitchExit within 1m0s (lost event or hung child)"},
			{WakeAt: watchdogTime.Add(SwitchDeadline)},
		})
		if !strings.Contains(acts[0].Log, "attempt 1") {
			t.Fatalf("expected attempt 1 in log, got %q", acts[0].Log)
		}
		if !strings.Contains(acts[0].Log, "retry switch to \"mac\" attempt 2") {
			t.Fatalf("expected retry attempt 2 log, got %q", acts[0].Log)
		}
	})

	t.Run("switch watchdog waits for retry spacing", func(t *testing.T) {
		cfg := testConfig("linux")
		cfg.RetrySpacing = 70 * time.Second
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})

		watchdogTime := t0.Add(SwitchDeadline)
		acts := m.Step(Event{Now: watchdogTime})
		requireActionCount(t, acts, 2)
		if acts[0].Switch {
			t.Fatalf("expected no switch while waiting retry spacing, got %+v", acts[0])
		}
		if !strings.Contains(acts[0].Log, "no SwitchExit within 1m0s (lost event or hung child)") {
			t.Fatalf("expected watchdog log, got %q", acts[0].Log)
		}
		if !acts[1].WakeAt.Equal(t0.Add(cfg.RetrySpacing)) {
			t.Fatalf("expected wake at retry spacing, got %v", acts[1].WakeAt)
		}
	})

	t.Run("watchdog exhausts retries and trips breaker", func(t *testing.T) {
		cfg := testConfig("linux")
		cfg.SwitchRetries = 0
		// Watchdog failures are spaced by SwitchDeadline, so widen the breaker
		// window enough to keep all three failures in the count.
		cfg.BreakerWindow = 300 * time.Second
		m := NewMachine(cfg, "linux", true)

		owners := []string{"mac", "win2", "win3", "win4"}
		base := t0
		var lastFail time.Time
		for i := 0; i < 3; i++ {
			m.Step(Event{Now: base, State: mkstate(owners[i], int64(i), map[string]bool{owners[i]: true})})

			lastFail = base.Add(SwitchDeadline)
			acts := m.Step(Event{Now: lastFail})
			checkActions(t, acts, []Action{{Notify: true, SaveOwner: ptr(owners[i]), Log: "failed"}})
			if !strings.Contains(acts[0].Log, "no SwitchExit within 1m0s (lost event or hung child)") {
				t.Fatalf("attempt %d: expected watchdog log, got %q", i+1, acts[0].Log)
			}

			// Resync back to linux so the next transition is away from us.
			m.Step(Event{Now: lastFail.Add(1 * time.Second), State: mkstate("linux", int64(i)+10, map[string]bool{"linux": true})})
			base = lastFail.Add(1*time.Second + cfg.Cooldown + 1*time.Second)
		}

		// Resync once more after the third failure.
		m.Step(Event{Now: base, State: mkstate("linux", 20, map[string]bool{"linux": true})})

		// The fourth transition is refused while the breaker is open.
		breakerEnd := lastFail.Add(cfg.BreakerOpenFor)
		acts := m.Step(Event{Now: base, State: mkstate(owners[3], 3, map[string]bool{owners[3]: true})})
		checkActions(t, acts, []Action{
			{Log: "breaker"},
			{WakeAt: breakerEnd},
		})
	})

	t.Run("late SwitchExit ignored", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})
		m.Step(Event{Now: t0.Add(2 * time.Second), SwitchExit: exit(nil)})

		acts := m.Step(Event{Now: t0.Add(3 * time.Second), SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{{Log: "ignoring late SwitchExit"}})
		if acts[0].Switch || acts[0].Notify || acts[0].SaveOwner != nil {
			t.Fatalf("expected only log action, got %+v", acts[0])
		}
	})

	t.Run("WakeAt is a standalone tail action", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		// The switch batch ends with a WakeAt-only action: the watchdog.
		acts := m.Step(Event{Now: t0, State: mkstate("mac", 1, macLive)})
		requireActionCount(t, acts, 2)
		if !acts[0].Switch || !acts[0].WakeAt.IsZero() {
			t.Fatalf("expected a switch carrying no WakeAt, got %+v", acts[0])
		}
		wake := acts[1]
		if wake.WakeAt.IsZero() || !wake.WakeAt.Equal(t0.Add(SwitchDeadline)) {
			t.Fatalf("expected wake at switch deadline, got %+v", wake)
		}
		if wake.Claim != "" || wake.Switch || wake.Notify || wake.SaveOwner != nil || wake.Log != "" {
			t.Fatalf("expected WakeAt-only action, got %+v", wake)
		}

		// A bare timer at the switch deadline fires the watchdog; the batch again
		// ends with a standalone WakeAt.
		watchdogTime := t0.Add(SwitchDeadline)
		acts = m.Step(Event{Now: watchdogTime})
		requireActionCount(t, acts, 2)
		wake = acts[1]
		if wake.WakeAt.IsZero() || !wake.WakeAt.Equal(watchdogTime.Add(SwitchDeadline)) {
			t.Fatalf("expected wake at next switch deadline, got %+v", wake)
		}
		if wake.Claim != "" || wake.Switch || wake.Notify || wake.SaveOwner != nil || wake.Log != "" {
			t.Fatalf("expected WakeAt-only action, got %+v", wake)
		}

		// A sequence that ends with nothing pending emits no WakeAt at all.
		acts = m.Step(Event{Now: watchdogTime.Add(time.Second), SwitchExit: exit(nil)})
		for _, a := range acts {
			if !a.WakeAt.IsZero() {
				t.Fatalf("expected no WakeAt once idle, got %+v", a)
			}
		}
	})
}

// TestMachineSequenceEndLastOwner pins the fix for the stale lastOwner bug:
// when a sequence ends with an attach queued, postSequence starts a settle
// and lastOwner must still track the persisted SaveOwner. A state event for
// the same owner during the settle must not re-reconcile (no second switch).
func TestMachineSequenceEndLastOwner(t *testing.T) {
	t0 := time.Unix(1000, 0)

	// afterEnd checks the common tail: settle started, same-owner state is
	// not re-reconciled, and the settle still claims.
	afterEnd := func(t *testing.T, m *Machine, cfg MachineConfig, end time.Time) {
		t.Helper()
		acts := m.Step(Event{Now: end.Add(500 * time.Millisecond), State: mkstate("mac", 9, map[string]bool{"mac": true})})
		checkActions(t, acts, []Action{
			{Log: "not me"},
			{WakeAt: end.Add(cfg.Settle)},
		})
		acts = m.Step(Event{Now: end.Add(cfg.Settle)})
		checkActions(t, acts, []Action{{Claim: "linux"}})
	}

	t.Run("sequence success with queued attach", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(3 * time.Second), Attach: true}) // queued

		end := t0.Add(4 * time.Second)
		acts := m.Step(Event{Now: end, SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{
			{SaveOwner: ptr("mac"), Log: "confirmed"},
			{WakeAt: end.Add(cfg.Settle)},
		})
		afterEnd(t, m, cfg, end)
	})

	t.Run("sequence failure with queued attach", func(t *testing.T) {
		cfg := testConfig("linux")
		cfg.SwitchRetries = 0
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(3 * time.Second), Attach: true}) // queued

		// The only attempt fails: no retries left.
		end := t0.Add(6 * time.Second)
		acts := m.Step(Event{Now: end, SwitchExit: exit(errors.New("still here"))})
		checkActions(t, acts, []Action{
			{Notify: true, SaveOwner: ptr("mac"), Log: "failed"},
			{WakeAt: end.Add(cfg.Settle)},
		})
		afterEnd(t, m, cfg, end)
	})
}

func ExampleMachine_Step() {
	cfg := MachineConfig{
		ID:             "linux",
		Settle:         2 * time.Second,
		SwitchRetries:  3,
		RetrySpacing:   1 * time.Second,
		Cooldown:       5 * time.Second,
		BreakerWindow:  30 * time.Second,
		BreakerMax:     3,
		BreakerOpenFor: 60 * time.Second,
	}
	m := NewMachine(cfg, "linux", true)
	t0 := time.Unix(0, 0)

	acts := m.Step(Event{
		Now: t0,
		State: &state.ServerState{
			Owner: "mac",
			Live:  map[string]bool{"mac": true},
		},
	})
	fmt.Println("action:", acts[0].Switch, acts[0].Log)
	// Output: action: true switching to "mac" attempt 1: ownership transition, winner live, gates clear
}
