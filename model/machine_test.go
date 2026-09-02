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
		Confirm:        4 * time.Second,
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
		if g.Probe != w.Probe {
			t.Errorf("action %d Probe=%v, want %v", i, g.Probe, w.Probe)
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
	errInactive := errors.New("check-cmd failed")

	t.Run("first reconcile adopts and never switches", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "", false)

		acts := m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		checkActions(t, acts, []Action{{SaveOwner: ptr("mac"), Log: "adopted"}})

		// The adopted owner is now treated as our own: no transition.
		acts = m.Step(Event{Now: t0, State: mkstate("mac", 2, map[string]bool{"mac": true})})
		checkActions(t, acts, []Action{{Log: "last_owner"}})
	})

	t.Run("second reconcile with transition switches", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		acts := m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		checkActions(t, acts, []Action{{Probe: true, Log: "probing"}})
	})

	t.Run("happy path", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		acts := m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		requireActionCount(t, acts, 1)
		if !acts[0].Probe {
			t.Fatalf("wanted probe, got %+v", acts[0])
		}

		acts = m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "attempt 1"},
			{WakeAt: t0.Add(1*time.Second + SwitchDeadline)},
		})

		confirmUntil := t0.Add(1 * time.Second).Add(cfg.Confirm)
		acts = m.Step(Event{Now: t0.Add(1 * time.Second), SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{
			{Probe: true, Log: "switch"},
			{WakeAt: confirmUntil},
		})

		// A successful probe inside the window does not re-probe immediately:
		// it schedules the next poll one probeInterval out.
		acts = m.Step(Event{Now: t0.Add(2 * time.Second), ProbeExit: exit(nil)})
		checkActions(t, acts, []Action{{WakeAt: t0.Add(2500 * time.Millisecond)}})

		// The probe timer fires the next --check-cmd run.
		acts = m.Step(Event{Now: t0.Add(2500 * time.Millisecond)})
		checkActions(t, acts, []Action{
			{Probe: true},
			{WakeAt: confirmUntil},
		})

		end := t0.Add(3 * time.Second)
		acts = m.Step(Event{Now: end, ProbeExit: exit(errInactive)})
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
		if acts[0].Probe {
			t.Fatalf("expected no probe, got %+v", acts[0])
		}
		if !strings.Contains(acts[0].Log, "not live") {
			t.Fatalf("expected log mentioning not live, got %q", acts[0].Log)
		}
	})

	t.Run("probe veto fails", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		acts := m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(errInactive)})
		checkActions(t, acts, []Action{{SaveOwner: ptr("mac"), Log: "veto"}})
		if acts[0].Switch {
			t.Fatal("expected no switch")
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

	t.Run("retries succeed on later retry", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		// First attempt: probes succeed through the confirm window.
		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
		m.Step(Event{Now: t0.Add(2 * time.Second), SwitchExit: exit(nil)})

		// Poll succeeds a few times.
		m.Step(Event{Now: t0.Add(2500 * time.Millisecond), ProbeExit: exit(nil)})
		m.Step(Event{Now: t0.Add(3 * time.Second), ProbeExit: exit(nil)})

		// Confirm window elapses; retry immediately because retry spacing has passed.
		acts := m.Step(Event{Now: t0.Add(6 * time.Second), ProbeExit: exit(nil)})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "attempt 2"},
			{WakeAt: t0.Add(6*time.Second + SwitchDeadline)},
		})

		// Second attempt lands.
		confirmUntil := t0.Add(7 * time.Second).Add(cfg.Confirm)
		acts = m.Step(Event{Now: t0.Add(7 * time.Second), SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{
			{Probe: true},
			{WakeAt: confirmUntil},
		})

		acts = m.Step(Event{Now: t0.Add(8 * time.Second), ProbeExit: exit(errInactive)})
		checkActions(t, acts, []Action{{SaveOwner: ptr("mac"), Log: "confirmed"}})
	})

	t.Run("all retries exhausted then notify and stop", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})

		switchAt := t0.Add(1 * time.Second)
		for i := 0; i < 1+cfg.SwitchRetries; i++ {
			acts := m.Step(Event{Now: switchAt, SwitchExit: exit(nil)})
			if i == 0 {
				if !acts[0].Probe {
					t.Fatalf("attempt %d: wanted probe after switch, got %+v", i+1, acts)
				}
			}
			// Confirm window closes with all probes succeeding.
			switchAt = switchAt.Add(cfg.Confirm)
			acts = m.Step(Event{Now: switchAt, ProbeExit: exit(nil)})
			if i < cfg.SwitchRetries {
				if !acts[0].Switch {
					t.Fatalf("attempt %d: wanted retry switch, got %+v", i+1, acts)
				}
			} else {
				// Final action must be notify + save owner.
				checkActions(t, acts, []Action{{Notify: true, SaveOwner: ptr("mac"), Log: "failed"}})
			}
		}

		// Same transition afterwards is a no-op: last_owner is already mac.
		acts := m.Step(Event{Now: switchAt.Add(1 * time.Second), State: mkstate("mac", 2, map[string]bool{"mac": true})})
		requireActionCount(t, acts, 1)
		if acts[0].Switch || acts[0].Probe || acts[0].Notify {
			t.Fatalf("expected no-op after failure, got %+v", acts[0])
		}
	})

	t.Run("cooldown defers then proceeds", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		// Complete a switch to mac at t0+3s.
		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
		m.Step(Event{Now: t0.Add(2 * time.Second), SwitchExit: exit(nil)})
		m.Step(Event{Now: t0.Add(3 * time.Second), ProbeExit: exit(errInactive)})

		// Resync: the server now says we are the owner again.
		m.Step(Event{Now: t0.Add(4 * time.Second), State: mkstate("linux", 2, map[string]bool{"linux": true})})

		// A new transition inside the cooldown is deferred.
		cooldownEnd := t0.Add(3 * time.Second).Add(cfg.Cooldown)
		acts := m.Step(Event{Now: t0.Add(4 * time.Second), State: mkstate("win2", 3, map[string]bool{"win2": true})})
		checkActions(t, acts, []Action{
			{Log: "deferring"},
			{WakeAt: cooldownEnd},
		})

		// After the cooldown, the bare timer re-runs the reconcile and probes.
		acts = m.Step(Event{Now: cooldownEnd})
		requireActionCount(t, acts, 1)
		if !acts[0].Probe {
			t.Fatalf("wanted deferred probe, got %+v", acts[0])
		}
	})

	t.Run("circuit breaker trips and reopens", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		owners := []string{"mac", "win2", "win3", "win4"}
		base := t0
		// Three completed switch sequences inside the breaker window.
		for i := 0; i < 3; i++ {
			m.Step(Event{Now: base, State: mkstate(owners[i], int64(i), map[string]bool{owners[i]: true})})
			m.Step(Event{Now: base.Add(1 * time.Second), ProbeExit: exit(nil)})
			m.Step(Event{Now: base.Add(2 * time.Second), SwitchExit: exit(nil)})
			m.Step(Event{Now: base.Add(3 * time.Second), ProbeExit: exit(errInactive)})
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
		requireActionCount(t, acts, 1)
		if !acts[0].Probe {
			t.Fatalf("wanted probe after breaker reopened, got %+v", acts[0])
		}
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

	t.Run("settle survives a reconcile", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, Attach: true}) // settle deadline at t0+2s

		// A state arrives mid-settle and starts a reconcile; the veto fails.
		acts := m.Step(Event{Now: t0.Add(1 * time.Second), State: mkstate("mac", 1, map[string]bool{"mac": true})})
		checkActions(t, acts, []Action{
			{Probe: true, Log: "probing"},
			{WakeAt: t0.Add(cfg.Settle)},
		})
		acts = m.Step(Event{Now: t0.Add(1500 * time.Millisecond), ProbeExit: exit(errInactive)})
		checkActions(t, acts, []Action{
			{SaveOwner: ptr("mac"), Log: "veto"},
			{WakeAt: t0.Add(cfg.Settle)},
		})

		// The interrupted settle still fires the claim (SPEC §5.4: attach→claim
		// is an independent trigger).
		acts = m.Step(Event{Now: t0.Add(cfg.Settle)})
		checkActions(t, acts, []Action{{Claim: "linux"}})
	})

	t.Run("attach during sequence queued", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
		m.Step(Event{Now: t0.Add(2 * time.Second), SwitchExit: exit(nil)})

		// Attach while confirming. The machine keeps the confirm deadline.
		confirmUntil := t0.Add(2 * time.Second).Add(cfg.Confirm)
		acts := m.Step(Event{Now: t0.Add(2500 * time.Millisecond), Attach: true})
		requireActionCount(t, acts, 1)
		if !acts[0].WakeAt.Equal(confirmUntil) {
			t.Fatalf("expected wake at confirm deadline %v, got %v", confirmUntil, acts[0].WakeAt)
		}

		// Confirm succeeds; pending attach should start settle.
		end := t0.Add(3500 * time.Millisecond)
		acts = m.Step(Event{Now: end, ProbeExit: exit(errInactive)})
		checkActions(t, acts, []Action{
			{SaveOwner: ptr("mac"), Log: "confirmed"},
			{WakeAt: end.Add(cfg.Settle)},
		})

		acts = m.Step(Event{Now: end.Add(cfg.Settle)})
		checkActions(t, acts, []Action{{Claim: "linux"}})
	})

	t.Run("deferred reconcile stashing", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		// A newer state arrives while we are awaiting the veto probe.
		m.Step(Event{Now: t0.Add(1 * time.Second), State: mkstate("win2", 2, map[string]bool{"win2": true})})

		// Veto fails for the original target: we resync to mac, then reconcile win2.
		acts := m.Step(Event{Now: t0.Add(2 * time.Second), ProbeExit: exit(errInactive)})
		requireActionCount(t, acts, 2)
		if acts[0].SaveOwner == nil || *acts[0].SaveOwner != "mac" {
			t.Fatalf("action 0 expected save mac, got %+v", acts[0])
		}
		if !acts[1].Probe {
			t.Fatalf("action 1 expected probe for win2, got %+v", acts[1])
		}
	})

	t.Run("stale epoch still reconciles", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		acts := m.Step(Event{Now: t0, State: mkstate("mac", 5, map[string]bool{"mac": true})})
		if !acts[0].Probe {
			t.Fatalf("expected probe for mac, got %+v", acts[0])
		}

		// A state with a lower epoch but a different owner is still a valid input.
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(errInactive)})
		m.Step(Event{Now: t0.Add(2 * time.Second), State: mkstate("linux", 6, map[string]bool{"linux": true})})

		lower := mkstate("win2", 2, map[string]bool{"win2": true})
		acts = m.Step(Event{Now: t0.Add(3 * time.Second), State: lower})
		requireActionCount(t, acts, 1)
		if !acts[0].Probe {
			t.Fatalf("expected probe for lower-epoch win2, got %+v", acts[0])
		}
	})

	t.Run("switch watchdog fires and retries", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})

		watchdogTime := t0.Add(1*time.Second + SwitchDeadline)
		acts := m.Step(Event{Now: watchdogTime})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "no SwitchExit within 1m0s (lost event or hung child)"},
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

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})

		watchdogTime := t0.Add(1*time.Second + SwitchDeadline)
		acts := m.Step(Event{Now: watchdogTime})
		requireActionCount(t, acts, 2)
		if acts[0].Switch {
			t.Fatalf("expected no switch while waiting retry spacing, got %+v", acts[0])
		}
		if !strings.Contains(acts[0].Log, "no SwitchExit within 1m0s (lost event or hung child)") {
			t.Fatalf("expected watchdog log, got %q", acts[0].Log)
		}
		if !acts[1].WakeAt.Equal(t0.Add(1 * time.Second).Add(cfg.RetrySpacing)) {
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
			switchAt := base.Add(1 * time.Second)
			m.Step(Event{Now: switchAt, ProbeExit: exit(nil)})

			lastFail = switchAt.Add(SwitchDeadline)
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

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
		m.Step(Event{Now: t0.Add(2 * time.Second), SwitchExit: exit(nil)})

		acts := m.Step(Event{Now: t0.Add(3 * time.Second), SwitchExit: exit(nil)})
		confirmUntil := t0.Add(2 * time.Second).Add(cfg.Confirm)
		checkActions(t, acts, []Action{
			{Log: "ignoring late SwitchExit"},
			{WakeAt: confirmUntil},
		})
		if acts[0].Switch || acts[0].Probe || acts[0].Notify || acts[0].SaveOwner != nil {
			t.Fatalf("expected only log action, got %+v", acts[0])
		}
	})

	t.Run("late ProbeExit ignored", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})

		acts := m.Step(Event{Now: t0.Add(2 * time.Second), ProbeExit: exit(nil)})
		checkActions(t, acts, []Action{
			{Log: "ignoring late ProbeExit"},
			{WakeAt: t0.Add(1*time.Second + SwitchDeadline)},
		})
		if acts[0].Switch || acts[0].Probe || acts[0].Notify || acts[0].SaveOwner != nil {
			t.Fatalf("expected only log action, got %+v", acts[0])
		}
	})

	t.Run("confirm deadline defers while probe outstanding", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
		m.Step(Event{Now: t0.Add(2 * time.Second), SwitchExit: exit(nil)})

		confirmDeadline := t0.Add(2 * time.Second).Add(cfg.Confirm)
		acts := m.Step(Event{Now: confirmDeadline})
		requireActionCount(t, acts, 0)

		acts = m.Step(Event{Now: confirmDeadline, ProbeExit: exit(nil)})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "confirm elapsed; retry switch to \"mac\" attempt 2"},
			{WakeAt: confirmDeadline.Add(SwitchDeadline)},
		})
	})

	t.Run("state event during switching is stashed", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})

		acts := m.Step(Event{Now: t0.Add(2 * time.Second), State: mkstate("win2", 2, map[string]bool{"win2": true})})
		checkActions(t, acts, []Action{
			{WakeAt: t0.Add(1*time.Second + SwitchDeadline)},
		})
		if m.latestState == nil || m.latestState.Owner != "win2" {
			t.Fatalf("expected latestState stashed to win2, got %+v", m.latestState)
		}
	})

	t.Run("WakeAt is a standalone tail action", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		// Reconcile emits a probe but no WakeAt: there is no pending deadline yet.
		acts := m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		requireActionCount(t, acts, 1)
		if !acts[0].Probe {
			t.Fatalf("expected probe, got %+v", acts[0])
		}
		for _, a := range acts {
			if !a.WakeAt.IsZero() {
				t.Fatalf("expected no WakeAt in reconcile batch, got %+v", a)
			}
		}

		// The probe result triggers a switch; the batch ends with a WakeAt-only action.
		acts = m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
		requireActionCount(t, acts, 2)
		if !acts[0].Switch {
			t.Fatalf("expected switch, got %+v", acts[0])
		}
		wake := acts[1]
		if wake.WakeAt.IsZero() || !wake.WakeAt.Equal(t0.Add(1*time.Second+SwitchDeadline)) {
			t.Fatalf("expected wake at switch deadline, got %+v", wake)
		}
		if wake.Claim != "" || wake.Switch || wake.Probe || wake.Notify || wake.SaveOwner != nil || wake.Log != "" {
			t.Fatalf("expected WakeAt-only action, got %+v", wake)
		}

		// A bare timer at the switch deadline fires the watchdog; the batch again
		// ends with a standalone WakeAt.
		watchdogTime := t0.Add(1*time.Second + SwitchDeadline)
		acts = m.Step(Event{Now: watchdogTime})
		requireActionCount(t, acts, 2)
		wake = acts[1]
		if wake.WakeAt.IsZero() || !wake.WakeAt.Equal(watchdogTime.Add(SwitchDeadline)) {
			t.Fatalf("expected wake at next switch deadline, got %+v", wake)
		}
		if wake.Claim != "" || wake.Switch || wake.Probe || wake.Notify || wake.SaveOwner != nil || wake.Log != "" {
			t.Fatalf("expected WakeAt-only action, got %+v", wake)
		}
	})
}

func TestMachineDeadlineOrder(t *testing.T) {
	// Confirm that when several deadlines are due at the same Step they are
	// processed in the documented order: settle, deferred gate, retry wait,
	// confirm deadline.
	cfg := testConfig("linux")
	t0 := time.Unix(2000, 0)
	errInactive := errors.New("check-cmd failed")
	m := NewMachine(cfg, "linux", true)

	// Put the machine into confirming with the confirm deadline at t0+10.
	m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
	m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
	m.Step(Event{Now: t0.Add(2 * time.Second), SwitchExit: exit(nil)})

	// Attach while confirming; it must be queued, not claim.
	acts := m.Step(Event{Now: t0.Add(3 * time.Second), Attach: true})
	requireActionCount(t, acts, 1)
	if !acts[0].WakeAt.Equal(t0.Add(2 * time.Second).Add(cfg.Confirm)) {
		t.Fatalf("expected wake at confirm deadline, got %v", acts[0].WakeAt)
	}

	// When the confirm deadline arrives the sequence ends (success here), and
	// only then does the queued attach start settle.
	end := t0.Add(2 * time.Second).Add(cfg.Confirm)
	acts = m.Step(Event{Now: end, ProbeExit: exit(errInactive)})
	checkActions(t, acts, []Action{
		{SaveOwner: ptr("mac"), Log: "confirmed"},
		{WakeAt: end.Add(cfg.Settle)},
	})

	// The next timer at end+Settle should emit the claim.
	acts = m.Step(Event{Now: end.Add(cfg.Settle)})
	checkActions(t, acts, []Action{{Claim: "linux"}})
}

// TestMachineSequenceEndLastOwner pins the fix for the stale lastOwner bug:
// when a sequence ends with an attach queued, postSequence starts a settle
// and lastOwner must still track the persisted SaveOwner. A state event for
// the same owner during the settle must not re-reconcile (no veto probe).
func TestMachineSequenceEndLastOwner(t *testing.T) {
	t0 := time.Unix(1000, 0)
	errInactive := errors.New("check-cmd failed")

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
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
		m.Step(Event{Now: t0.Add(2 * time.Second), SwitchExit: exit(nil)})
		m.Step(Event{Now: t0.Add(3 * time.Second), Attach: true}) // queued

		end := t0.Add(4 * time.Second)
		acts := m.Step(Event{Now: end, ProbeExit: exit(errInactive)})
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
		m.Step(Event{Now: t0.Add(1 * time.Second), ProbeExit: exit(nil)})
		m.Step(Event{Now: t0.Add(2 * time.Second), SwitchExit: exit(nil)})
		m.Step(Event{Now: t0.Add(3 * time.Second), Attach: true}) // queued

		// Confirm window closes with all probes succeeding: no retries left.
		end := t0.Add(2 * time.Second).Add(cfg.Confirm)
		acts := m.Step(Event{Now: end, ProbeExit: exit(nil)})
		checkActions(t, acts, []Action{
			{Notify: true, SaveOwner: ptr("mac"), Log: "failed"},
			{WakeAt: end.Add(cfg.Settle)},
		})
		afterEnd(t, m, cfg, end)
	})

	t.Run("veto fail with queued attach", func(t *testing.T) {
		cfg := testConfig("linux")
		m := NewMachine(cfg, "linux", true)

		m.Step(Event{Now: t0, State: mkstate("mac", 1, map[string]bool{"mac": true})})
		m.Step(Event{Now: t0.Add(1 * time.Second), Attach: true}) // queued

		end := t0.Add(2 * time.Second)
		acts := m.Step(Event{Now: end, ProbeExit: exit(errInactive)})
		checkActions(t, acts, []Action{
			{SaveOwner: ptr("mac"), Log: "veto"},
			{WakeAt: end.Add(cfg.Settle)},
		})
		afterEnd(t, m, cfg, end)
	})
}

func TestNoCheck(t *testing.T) {
	t0 := time.Unix(0, 0)

	t.Run("clean switch directly switches and confirms on exit 0", func(t *testing.T) {
		cfg := testConfig("mac")
		cfg.NoCheck = true
		m := NewMachine(cfg, "mac", true)

		acts := m.Step(Event{Now: t0, State: mkstate("linux", 1, map[string]bool{"linux": true})})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "switching to \"linux\" attempt 1"},
			{WakeAt: t0.Add(SwitchDeadline)},
		})

		switchDone := t0.Add(500 * time.Millisecond)
		acts = m.Step(Event{Now: switchDone, SwitchExit: exit(nil)})
		checkActions(t, acts, []Action{
			{SaveOwner: ptr("linux"), Log: "confirmed"},
		})
		if !m.cooldownUntil.Equal(switchDone.Add(cfg.Cooldown)) {
			t.Fatalf("cooldown = %v, want %v", m.cooldownUntil, switchDone.Add(cfg.Cooldown))
		}
	})

	t.Run("switch failure retries and notifies on exhaustion", func(t *testing.T) {
		cfg := testConfig("mac")
		cfg.NoCheck = true
		cfg.SwitchRetries = 1
		m := NewMachine(cfg, "mac", true)

		m.Step(Event{Now: t0, State: mkstate("linux", 1, map[string]bool{"linux": true})})

		fail1 := t0.Add(200 * time.Millisecond)
		acts := m.Step(Event{Now: fail1, SwitchExit: exit(errors.New("switch error"))})
		retryAt := t0.Add(cfg.RetrySpacing)
		checkActions(t, acts, []Action{
			{Log: "waiting retry spacing"},
			{WakeAt: retryAt},
		})

		acts = m.Step(Event{Now: retryAt})
		checkActions(t, acts, []Action{
			{Switch: true, Log: "retry switch to \"linux\" attempt 2"},
			{WakeAt: retryAt.Add(SwitchDeadline)},
		})

		fail2 := retryAt.Add(200 * time.Millisecond)
		acts = m.Step(Event{Now: fail2, SwitchExit: exit(errors.New("switch error 2"))})
		checkActions(t, acts, []Action{
			{Notify: true, SaveOwner: ptr("linux"), Log: "failed"},
		})
	})
}

func ExampleMachine_Step() {
	cfg := MachineConfig{
		ID:             "linux",
		Settle:         2 * time.Second,
		Confirm:        4 * time.Second,
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
	fmt.Println("action:", acts[0].Probe, acts[0].Log)
	// Output: action: true probing before switch to "mac": ownership transition, winner live, gates clear
}
