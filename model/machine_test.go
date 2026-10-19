// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// machine_test.go: table-driven tests of the pure decision state machine.

package model

import (
	"errors"
	"fmt"
	"slices"
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
		ArrivalWindow:  10 * time.Second,
		Cooldown:       5 * time.Second,
		BreakerWindow:  30 * time.Second,
		BreakerMax:     3,
		BreakerOpenFor: 60 * time.Second,
	}
}

var t0 = time.Unix(1000, 0)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// Hold keys used throughout.
var (
	lgKey    = state.HoldKey{Kind: state.KindScreen, Name: "LG"}
	mouseKey = state.HoldKey{Kind: state.KindHID, Name: "mouse"}
)

func screenHold(name, holder, prev string, seq state.Seq) state.Hold {
	return state.Hold{Kind: state.KindScreen, Name: name, Holder: holder, Prev: prev, Seq: seq, Input: "F4:50=D1"}
}

func hidHold(name, holder, prev string, seq state.Seq, arrived bool) state.Hold {
	return state.Hold{Kind: state.KindHID, Name: name, Holder: holder, Prev: prev, Seq: seq, Channel: 2, Arrived: arrived}
}

// mk builds a state with every host live.
func mk(owner string, activation int64, holds ...state.Hold) *state.ServerState {
	return &state.ServerState{
		Owner:      owner,
		Activation: activation,
		Epoch:      activation + int64(len(holds)),
		Live:       map[string]bool{"linux": true, "mac": true},
		Holds:      holds,
	}
}

func notLive(s *state.ServerState, id string) *state.ServerState {
	s.Live = map[string]bool{"linux": true, "mac": true}
	s.Live[id] = false
	return s
}

// agentAt is the record of an agent that follows owner at activation, having
// handled the given holds.
func agentAt(owner string, activation int64, seen map[state.HoldKey]state.Seq) state.AgentState {
	rec := state.AgentState{LastOwner: owner, LastActivation: activation, Seen: map[string]state.Seq{}}
	for k, seq := range seen {
		rec.Seen[k.String()] = seq
	}
	return rec
}

func newMachine(id string, rec state.AgentState) *Machine {
	return NewMachine(testConfig(id), rec, true)
}

// sum renders the actions that do something as short strings, so a test reads
// as the sequence it expects. Log-only actions are left out; logs() returns
// them.
func sum(acts []Action) []string {
	var out []string
	for _, a := range acts {
		var parts []string
		if a.Claim != "" {
			parts = append(parts, "claim:"+a.Claim)
		}
		if a.Activate {
			parts = append(parts, fmt.Sprintf("activate:%d", a.Activation))
		}
		if a.Deactivate {
			parts = append(parts, "deactivate")
		}
		if (a.Activate || a.Deactivate) && a.Retry {
			parts = append(parts, "retry")
		}
		if len(a.Run) > 0 {
			names := make([]string, len(a.Run))
			for i, h := range a.Run {
				names[i] = h.Key().String()
			}
			parts = append(parts, "run:"+strings.Join(names, ","))
		}
		if a.Notify {
			parts = append(parts, "notify")
		}
		if a.Save != nil {
			parts = append(parts, "save")
		}
		if !a.WakeAt.IsZero() {
			parts = append(parts, fmt.Sprintf("wake:%d", int(a.WakeAt.Sub(t0)/time.Second)))
		}
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, "+"))
		}
	}
	return out
}

func logs(acts []Action) string {
	var out []string
	for _, a := range acts {
		if a.Log != "" {
			out = append(out, a.Log)
		}
	}
	return strings.Join(out, "\n")
}

func want(t *testing.T, acts []Action, w ...string) {
	t.Helper()
	if got := sum(acts); !slices.Equal(got, w) {
		t.Fatalf("actions = %q, want %q\nlogs:\n%s", got, w, logs(acts))
	}
}

func wantLog(t *testing.T, acts []Action, substr string) {
	t.Helper()
	if !strings.Contains(logs(acts), substr) {
		t.Fatalf("logs %q lack %q", logs(acts), substr)
	}
}

func saved(t *testing.T, acts []Action) *state.AgentState {
	t.Helper()
	var rec *state.AgentState
	for _, a := range acts {
		if a.Save != nil {
			rec = a.Save
		}
	}
	if rec == nil {
		t.Fatalf("no Save in %+v", acts)
	}
	return rec
}

func state_(s *state.ServerState, now time.Time) Event { return Event{Now: now, State: s} }

func okAll(keys ...state.HoldKey) *Exit {
	out := map[state.HoldKey]Outcome{}
	for _, k := range keys {
		out[k] = Outcome{}
	}
	return &Exit{Takes: out}
}

func takesExit(o map[state.HoldKey]Outcome) *Exit { return &Exit{Takes: o} }

var errWrite = errors.New("ddcutil: no i2c-dev")

func TestFirstReconcileAdopts(t *testing.T) {
	m := NewMachine(testConfig("linux"), state.AgentState{}, false)
	s := mk("mac", 4, screenHold("LG", "mac", "linux", 3), hidHold("mouse", "mac", "linux", 4, false))

	acts := m.Step(state_(s, at(0)))
	want(t, acts, "save")
	wantLog(t, acts, "adopted")
	rec := saved(t, acts)
	if rec.LastOwner != "mac" || rec.LastActivation != 4 ||
		rec.Seen["screen/LG"] != 3 || rec.Seen["hid/mouse"] != 4 {
		t.Fatalf("adopted record = %+v", rec)
	}

	// The adopted state is the baseline: the same state runs nothing, not an
	// activation (I am not the owner), not a deactivation (last_owner is mac),
	// not the takes it holds.
	want(t, m.Step(state_(s, at(1))))

	// A name the agent meets after the first reconcile is new, and so is a
	// new take of a name it adopted.
	dell := state.HoldKey{Kind: state.KindScreen, Name: "DELL"}
	next := mk("mac", 4, screenHold("DELL", "mac", "", 5), screenHold("LG", "mac", "linux", 3), hidHold("mouse", "mac", "linux", 4, false))
	acts = m.Step(state_(next, at(2)))
	want(t, acts, "run:screen/DELL", "wake:62")
	want(t, m.Step(Event{Now: at(3), Exit: okAll(dell)}), "save")
	next = mk("mac", 4, screenHold("DELL", "mac", "", 5), screenHold("LG", "mac", "linux", 6), hidHold("mouse", "mac", "linux", 4, false))
	acts = m.Step(state_(next, at(10)))
	want(t, acts, "run:screen/LG", "wake:70")
}

// A restarted agent builds its machine from agent.json: what it handled stays
// handled.
func TestRecordSurvivesARestart(t *testing.T) {
	m := newMachine("linux", agentAt("mac", 2, nil))
	s := mk("mac", 2, screenHold("LG", "mac", "linux", 3))
	acts := m.Step(state_(s, at(0)))
	want(t, acts, "run:screen/LG", "wake:60")
	acts = m.Step(Event{Now: at(1), Exit: okAll(lgKey)})
	want(t, acts, "save")

	again := NewMachine(testConfig("linux"), *saved(t, acts), true)
	want(t, again.Step(state_(s, at(100))))
}

func TestActivation(t *testing.T) {
	t.Run("ownership comes here: run, then save the record", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 1, nil))
		acts := m.Step(state_(mk("linux", 2), at(0)))
		want(t, acts, "activate:2", "wake:60")
		if !acts[0].Activate || acts[0].Retry || acts[0].Activation != 2 {
			t.Fatalf("first action = %+v", acts[0])
		}
		acts = m.Step(Event{Now: at(2), Exit: &Exit{}})
		want(t, acts, "save")
		if rec := saved(t, acts); rec.LastOwner != "linux" || rec.LastActivation != 2 {
			t.Fatalf("record = %+v", rec)
		}
		// Nothing more for the same transition.
		want(t, m.Step(state_(mk("linux", 2), at(3))))
	})

	t.Run("a re-activation of the owner runs the commands again", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 2, nil))
		want(t, m.Step(state_(mk("linux", 2), at(0))))
		acts := m.Step(state_(mk("linux", 3), at(1)))
		want(t, acts, "activate:3", "wake:61")
		acts = m.Step(Event{Now: at(2), Exit: &Exit{}})
		if rec := saved(t, acts); rec.LastActivation != 3 {
			t.Fatalf("record = %+v", rec)
		}
		want(t, m.Step(state_(mk("linux", 3), at(3))))
	})

	t.Run("an agent that never activated runs on an owner it already was", func(t *testing.T) {
		m := newMachine("linux", agentAt("", 0, nil))
		want(t, m.Step(state_(mk("linux", 1), at(0))), "activate:1", "wake:60")
	})

	t.Run("a failure retries only after the spacing, then is saved without a notification", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 1, nil))
		m.Step(state_(mk("linux", 2), at(0)))

		// Fast failure: the retry waits for 1 s after the previous start.
		acts := m.Step(Event{Now: at(0), Exit: &Exit{Err: errors.New("exit 1")}})
		want(t, acts, "wake:1")
		acts = m.Step(Event{Now: at(1)})
		want(t, acts, "activate:2+retry", "wake:61")
		if acts[0].Activation != 2 {
			t.Fatalf("retry carries activation %d, want 2", acts[0].Activation)
		}
		// A slow failure retries at once.
		want(t, m.Step(Event{Now: at(5), Exit: &Exit{Err: errors.New("exit 1")}}), "activate:2+retry", "wake:65")
		want(t, m.Step(Event{Now: at(9), Exit: &Exit{Err: errors.New("exit 1")}}), "activate:2+retry", "wake:69")

		// 1 + 3 retries are spent: the failure is logged, never notified, and
		// the record moves on so the same transition does not run again.
		acts = m.Step(Event{Now: at(12), Exit: &Exit{Err: errors.New("exit 1")}})
		want(t, acts, "save")
		wantLog(t, acts, "failed after 4 attempts")
		if rec := saved(t, acts); rec.LastOwner != "linux" || rec.LastActivation != 2 {
			t.Fatalf("record = %+v", rec)
		}
		want(t, m.Step(state_(mk("linux", 2), at(13))))
	})

	t.Run("never waits behind the cooldown or the breaker", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 1, nil))
		m.cooldownUntil[lgKey] = at(100)
		m.breakerOpenUntil = at(100)
		want(t, m.Step(state_(mk("linux", 2), at(0))), "activate:2", "wake:60")
	})

	t.Run("a flip away during the activation deactivates when it ends", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 1, nil))
		m.Step(state_(mk("linux", 2), at(0)))
		// Stashed: the activation is still running.
		want(t, m.Step(state_(mk("mac", 3), at(1))), "wake:60")
		// The activation spends its attempts first (a 409 each time), then the
		// deactivation runs right after.
		for _, sec := range []int{2, 3, 5} {
			want(t, m.Step(Event{Now: at(sec), Exit: &Exit{Err: errors.New("409")}}), "activate:2+retry", fmt.Sprintf("wake:%d", sec+60))
		}
		acts := m.Step(Event{Now: at(7), Exit: &Exit{Err: errors.New("409")}})
		want(t, acts, "save", "deactivate", "wake:67")
		if rec := saved(t, acts); rec.LastOwner != "linux" {
			t.Fatalf("record = %+v", rec)
		}
	})

	t.Run("a flip away during a successful activation deactivates right after", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 1, nil))
		m.Step(state_(mk("linux", 2), at(0)))
		want(t, m.Step(state_(mk("mac", 3), at(1))), "wake:60")
		acts := m.Step(Event{Now: at(2), Exit: &Exit{}})
		want(t, acts, "save", "deactivate", "wake:62")
	})
}

func TestDeactivation(t *testing.T) {
	t.Run("ownership goes away: run, then save the new owner", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		acts := m.Step(state_(mk("mac", 2), at(0)))
		want(t, acts, "deactivate", "wake:60")
		if acts[0].Retry {
			t.Fatal("first run flagged Retry")
		}
		acts = m.Step(Event{Now: at(2), Exit: &Exit{}})
		want(t, acts, "save")
		if rec := saved(t, acts); rec.LastOwner != "mac" {
			t.Fatalf("record = %+v", rec)
		}
		want(t, m.Step(state_(mk("mac", 2), at(3))))
	})

	t.Run("no liveness gate", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		want(t, m.Step(state_(notLive(mk("mac", 2), "mac"), at(0))), "deactivate", "wake:60")
	})

	t.Run("no cooldown and no breaker", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.cooldownUntil[lgKey] = at(100)
		m.breakerOpenUntil = at(100)
		want(t, m.Step(state_(mk("mac", 2), at(0))), "deactivate", "wake:60")
	})

	t.Run("an empty owner deactivates nothing", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		want(t, m.Step(state_(mk("", 0), at(0))))
	})

	t.Run("a host that was not the owner has nothing to deactivate", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 1, nil))
		want(t, m.Step(state_(mk("third", 2), at(0))))
	})

	t.Run("a third host as owner deactivates the previous one", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		want(t, m.Step(state_(mk("third", 2), at(0))), "deactivate", "wake:60")
	})

	t.Run("failure retries, is logged, never notified, and the record moves on", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.Step(state_(mk("mac", 2), at(0)))
		want(t, m.Step(Event{Now: at(0), Exit: &Exit{Err: errors.New("x")}}), "wake:1")
		want(t, m.Step(Event{Now: at(1)}), "deactivate+retry", "wake:61")
		want(t, m.Step(Event{Now: at(3), Exit: &Exit{Err: errors.New("x")}}), "deactivate+retry", "wake:63")
		want(t, m.Step(Event{Now: at(5), Exit: &Exit{Err: errors.New("x")}}), "deactivate+retry", "wake:65")
		acts := m.Step(Event{Now: at(7), Exit: &Exit{Err: errors.New("x")}})
		want(t, acts, "save")
		wantLog(t, acts, "failed after 4 attempts")
	})

	t.Run("a flip back during the deactivation activates when it ends", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.Step(state_(mk("mac", 2), at(0)))
		want(t, m.Step(state_(mk("linux", 3), at(1))), "wake:60")
		acts := m.Step(Event{Now: at(2), Exit: &Exit{}})
		want(t, acts, "save", "activate:3", "wake:62")
	})

	t.Run("a flip back during a failing deactivation activates when it ends", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.Step(state_(mk("mac", 2), at(0)))
		want(t, m.Step(state_(mk("linux", 3), at(1))), "wake:60")
		m.Step(Event{Now: at(2), Exit: &Exit{Err: errors.New("x")}})
		m.Step(Event{Now: at(3)})
		m.Step(Event{Now: at(5), Exit: &Exit{Err: errors.New("x")}})
		m.Step(Event{Now: at(7), Exit: &Exit{Err: errors.New("x")}})
		acts := m.Step(Event{Now: at(9), Exit: &Exit{Err: errors.New("x")}})
		want(t, acts, "save", "activate:3", "wake:69")
	})
}

func TestTakeGates(t *testing.T) {
	// Every combination of the §4.3 gates a pending take passes.
	for _, seenSame := range []bool{false, true} {
		for _, arrived := range []bool{false, true} {
			for _, holderLive := range []bool{true, false} {
				for _, cooling := range []bool{false, true} {
					for _, breaker := range []bool{false, true} {
						for _, holderIsMe := range []bool{false, true} {
							name := fmt.Sprintf("seen=%v arrived=%v live=%v cooling=%v breaker=%v mine=%v",
								seenSame, arrived, holderLive, cooling, breaker, holderIsMe)
							t.Run(name, func(t *testing.T) {
								seen := map[state.HoldKey]state.Seq{}
								if seenSame {
									seen[mouseKey] = 7
								} else {
									seen[mouseKey] = 6
								}
								m := newMachine("linux", agentAt("mac", 2, seen))
								holder := "mac"
								if holderIsMe {
									holder = "linux"
								}
								s := mk("mac", 2, hidHold("mouse", holder, "linux", 7, arrived))
								notLive(s, "mac")
								s.Live["mac"] = holderLive
								if cooling {
									m.cooldownUntil[mouseKey] = at(3)
								}
								if breaker {
									m.breakerOpenUntil = at(30)
								}
								acts := m.Step(state_(s, at(0)))

								switch {
								case holderIsMe, seenSame:
									want(t, acts)
								case arrived:
									want(t, acts, "save")
									if saved(t, acts).Seen["hid/mouse"] != 7 {
										t.Fatal("an arrived hold is not marked seen")
									}
								case !holderLive:
									want(t, acts)
									wantLog(t, acts, "not live")
								case breaker:
									want(t, acts, "wake:30")
									wantLog(t, acts, "breaker open")
								case cooling:
									want(t, acts, "wake:3")
									wantLog(t, acts, "cooldown")
								default:
									want(t, acts, "run:hid/mouse", "wake:60")
								}
							})
						}
					}
				}
			}
		}
	}
}

func TestTakeRun(t *testing.T) {
	t.Run("a live holder's new take runs and is handled once", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		s := mk("mac", 2, screenHold("LG", "mac", "linux", 3))
		acts := m.Step(state_(s, at(0)))
		want(t, acts, "run:screen/LG", "wake:60")
		if acts[0].Run[0].Input != "F4:50=D1" {
			t.Fatalf("run = %+v", acts[0].Run)
		}
		acts = m.Step(Event{Now: at(1), Exit: okAll(lgKey)})
		want(t, acts, "save")
		if rec := saved(t, acts); rec.Seen["screen/LG"] != 3 {
			t.Fatalf("record = %+v", rec)
		}
		want(t, m.Step(state_(s, at(2))))
	})

	t.Run("one run in the order /state lists them", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		s := mk("mac", 2, hidHold("keyboard", "mac", "linux", 3, false),
			hidHold("mouse", "mac", "linux", 3, false), screenHold("LG", "mac", "linux", 3))
		want(t, m.Step(state_(s, at(0))), "run:hid/keyboard,hid/mouse,screen/LG", "wake:60")
	})

	t.Run("nothing to move here is done, silently", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "linux", 3)), at(0)))
		acts := m.Step(Event{Now: at(1), Exit: takesExit(map[state.HoldKey]Outcome{lgKey: {NotHere: true}})})
		want(t, acts, "save")
		if strings.Contains(logs(acts), "failed") {
			t.Fatalf("logs %q", logs(acts))
		}
	})

	t.Run("a failed write re-runs only that take, spaced by the retry spacing", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		s := mk("mac", 2, hidHold("mouse", "mac", "linux", 3, false), screenHold("LG", "mac", "linux", 3))
		m.Step(state_(s, at(0)))
		acts := m.Step(Event{Now: at(0), Exit: takesExit(map[state.HoldKey]Outcome{
			mouseKey: {NotHere: true}, lgKey: {Err: errWrite},
		})})
		want(t, acts, "wake:1")
		acts = m.Step(Event{Now: at(1)})
		want(t, acts, "run:screen/LG", "wake:61")

		// A slow failure retries at once.
		acts = m.Step(Event{Now: at(5), Exit: takesExit(map[state.HoldKey]Outcome{lgKey: {Err: errWrite}})})
		want(t, acts, "run:screen/LG", "wake:65")
		acts = m.Step(Event{Now: at(8), Exit: takesExit(map[state.HoldKey]Outcome{lgKey: {}})})
		want(t, acts, "save")
	})

	t.Run("a take that reports nothing counts as failed", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "linux", 3)), at(0)))
		acts := m.Step(Event{Now: at(5), Exit: takesExit(map[state.HoldKey]Outcome{})})
		want(t, acts, "run:screen/LG", "wake:65")
	})

	t.Run("failing after the last retry notifies the previous holder", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		s := mk("mac", 2, screenHold("LG", "mac", "linux", 3))
		m.Step(state_(s, at(0)))
		fail := takesExit(map[state.HoldKey]Outcome{lgKey: {Err: errWrite}})
		now := 0
		for i := 0; i < 3; i++ {
			now += 2
			acts := m.Step(Event{Now: at(now), Exit: fail})
			want(t, acts, "run:screen/LG", fmt.Sprintf("wake:%d", now+60))
		}
		now += 2
		acts := m.Step(Event{Now: at(now), Exit: fail})
		want(t, acts, "notify", "save")
		wantLog(t, acts, "failed after 4 attempts")
		if rec := saved(t, acts); rec.Seen["screen/LG"] != 3 {
			t.Fatalf("a failure is final for that seq: %+v", rec)
		}
		want(t, m.Step(state_(s, at(now+1))))
	})

	t.Run("a host that is not the previous holder only logs", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "third", 3)), at(0)))
		fail := takesExit(map[state.HoldKey]Outcome{lgKey: {Err: errWrite}})
		var acts []Action
		for i := 1; i <= 4; i++ {
			acts = m.Step(Event{Now: at(2 * i), Exit: fail})
		}
		want(t, acts, "save")
		wantLog(t, acts, "I am not the previous holder")
	})

	t.Run("a first take has no previous holder and never notifies", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "", 3)), at(0)))
		fail := takesExit(map[state.HoldKey]Outcome{lgKey: {Err: errWrite}})
		var acts []Action
		for i := 1; i <= 4; i++ {
			acts = m.Step(Event{Now: at(2 * i), Exit: fail})
		}
		want(t, acts, "save")
	})

	t.Run("one failing take does not skip or fail the rest", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, false), screenHold("LG", "mac", "linux", 3)), at(0)))
		m.Step(Event{Now: at(2), Exit: takesExit(map[state.HoldKey]Outcome{mouseKey: {Err: errWrite}, lgKey: {}})})
		m.Step(Event{Now: at(4), Exit: takesExit(map[state.HoldKey]Outcome{mouseKey: {Err: errWrite}})})
		m.Step(Event{Now: at(6), Exit: takesExit(map[state.HoldKey]Outcome{mouseKey: {Err: errWrite}})})
		acts := m.Step(Event{Now: at(8), Exit: takesExit(map[state.HoldKey]Outcome{mouseKey: {Err: errWrite}})})
		want(t, acts, "notify", "save")
		rec := saved(t, acts)
		if rec.Seen["screen/LG"] != 3 || rec.Seen["hid/mouse"] != 3 {
			t.Fatalf("record = %+v", rec)
		}
	})

	t.Run("a state during the run is stashed, and reconciled when it ends", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "linux", 3)), at(0)))
		want(t, m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "linux", 3), hidHold("mouse", "mac", "linux", 4, false)), at(1))), "wake:60")
		acts := m.Step(Event{Now: at(2), Exit: okAll(lgKey)})
		want(t, acts, "save", "run:hid/mouse", "wake:62")
	})

	t.Run("a flip back to me during a run of takes activates when it ends", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		third := func(owner string, activation int64) *state.ServerState {
			s := mk(owner, activation, screenHold("LG", "third", "linux", 3))
			s.Live["third"] = true
			return s
		}
		m.Step(state_(third("third", 2), at(0)))
		m.Step(Event{Now: at(1), Exit: &Exit{}}) // the deactivation ends, the take starts
		want(t, m.Step(state_(third("linux", 3), at(2))), "wake:61")
		acts := m.Step(Event{Now: at(3), Exit: okAll(lgKey)})
		want(t, acts, "save", "activate:3", "wake:63")
	})
}

func TestArrival(t *testing.T) {
	sent := func(t *testing.T) *Machine {
		t.Helper()
		m := newMachine("linux", agentAt("mac", 2, nil))
		want(t, m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, false)), at(0))), "run:hid/mouse", "wake:60")
		return m
	}

	t.Run("a sent device waits for its arrival, then is done", func(t *testing.T) {
		m := sent(t)
		// Sent at 1 s: the window closes at 11 s.
		acts := m.Step(Event{Now: at(1), Exit: okAll(mouseKey)})
		want(t, acts, "wake:11")
		acts = m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, true)), at(3)))
		want(t, acts, "save")
		if rec := saved(t, acts); rec.Seen["hid/mouse"] != 3 {
			t.Fatalf("record = %+v", rec)
		}
		// Nothing re-sends it, and no window fires later.
		want(t, m.Step(Event{Now: at(30)}))
	})

	t.Run("the device is not done on the exit alone", func(t *testing.T) {
		m := sent(t)
		m.Step(Event{Now: at(1), Exit: okAll(mouseKey)})
		// A state that does not show the arrival changes nothing.
		want(t, m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, false)), at(2))), "wake:11")
	})

	t.Run("an arrival already in the state at the exit is the receipt", func(t *testing.T) {
		m := sent(t)
		m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, true)), at(1)))
		want(t, m.Step(Event{Now: at(2), Exit: okAll(mouseKey)}), "save")
	})

	t.Run("no arrival within the window re-sends, spaced and counted", func(t *testing.T) {
		m := sent(t)
		m.Step(Event{Now: at(1), Exit: okAll(mouseKey)})
		acts := m.Step(Event{Now: at(11)})
		want(t, acts, "run:hid/mouse", "wake:71")
		wantLog(t, acts, "did not arrive")
	})

	t.Run("never arriving notifies the previous holder after the last retry", func(t *testing.T) {
		m := sent(t)
		now := 1
		m.Step(Event{Now: at(now), Exit: okAll(mouseKey)})
		for i := 0; i < 3; i++ {
			now += 10
			want(t, m.Step(Event{Now: at(now)}), "run:hid/mouse", fmt.Sprintf("wake:%d", now+60))
			now++
			want(t, m.Step(Event{Now: at(now), Exit: okAll(mouseKey)}), fmt.Sprintf("wake:%d", now+10))
		}
		now += 10
		acts := m.Step(Event{Now: at(now)})
		want(t, acts, "notify", "save")
		wantLog(t, acts, "did not arrive")
	})

	t.Run("a take superseded while awaiting is dropped, and its successor is pending", func(t *testing.T) {
		m := sent(t)
		m.Step(Event{Now: at(1), Exit: okAll(mouseKey)})
		// The successor appears while the mouse awaits: the old take is dropped
		// and the batch ends at once, without marking the new seq seen.
		acts := m.Step(state_(mk("mac", 2, hidHold("mouse", "linux", "mac", 4, false)), at(2)))
		want(t, acts, "save")
		wantLog(t, acts, "a newer take replaced it")
		if rec := saved(t, acts); rec.Seen["hid/mouse"] != 3 {
			t.Fatalf("record = %+v, want the old seq handled and the new one not", rec)
		}
		// The new holder is me: nothing to run for it, and no window fires.
		want(t, m.Step(Event{Now: at(12)}))
	})

	t.Run("a superseding take by another host runs after the cooldown", func(t *testing.T) {
		m := sent(t)
		m.Step(Event{Now: at(1), Exit: okAll(mouseKey)})
		third := mk("mac", 2, hidHold("mouse", "third", "mac", 4, false))
		third.Live["third"] = true
		// The batch ends at 2 s; the mouse cooled down for 5 s since.
		acts := m.Step(state_(third, at(2)))
		want(t, acts, "save", "wake:7")
		wantLog(t, acts, "cooldown")
		want(t, m.Step(Event{Now: at(6)}), "wake:7")
		want(t, m.Step(Event{Now: at(7)}), "run:hid/mouse", "wake:67")
	})

	t.Run("a failed take superseded before its retry is not re-run", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, false)), at(0)))
		want(t, m.Step(Event{Now: at(0), Exit: takesExit(map[state.HoldKey]Outcome{mouseKey: {Err: errWrite}})}), "wake:1")
		acts := m.Step(state_(mk("mac", 2, hidHold("mouse", "linux", "mac", 4, false)), at(0)))
		want(t, acts, "save")
		want(t, m.Step(Event{Now: at(1)}))
	})

	t.Run("a hold that vanished is superseded", func(t *testing.T) {
		m := sent(t)
		m.Step(Event{Now: at(1), Exit: okAll(mouseKey)})
		want(t, m.Step(state_(mk("mac", 2), at(2))), "save")
	})

	t.Run("a screen failing while a device awaits retries without waiting for the window", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, false), screenHold("LG", "mac", "linux", 3)), at(0)))
		acts := m.Step(Event{Now: at(0), Exit: takesExit(map[state.HoldKey]Outcome{mouseKey: {}, lgKey: {Err: errWrite}})})
		want(t, acts, "wake:1")
		acts = m.Step(Event{Now: at(1)})
		want(t, acts, "run:screen/LG", "wake:10")
		acts = m.Step(Event{Now: at(2), Exit: takesExit(map[state.HoldKey]Outcome{lgKey: {}})})
		want(t, acts, "wake:10")
		want(t, m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, true), screenHold("LG", "mac", "linux", 3)), at(3))), "save")
	})

	t.Run("a device already on the holder is never sent", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		acts := m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, true)), at(0)))
		want(t, acts, "save")
	})
}

func TestCooldownAndBreaker(t *testing.T) {
	handled := func(m *Machine, sec int, holds ...state.Hold) []Action {
		return m.Step(state_(mk("mac", 2, holds...), at(sec)))
	}

	t.Run("cooldown is per name", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		handled(m, 0, screenHold("LG", "mac", "linux", 3))
		m.Step(Event{Now: at(1), Exit: okAll(lgKey)})
		// 2 s later: the screen cooled down until 6 s, the mouse did not.
		acts := handled(m, 3, screenHold("LG", "mac", "linux", 4), hidHold("mouse", "mac", "linux", 4, false))
		want(t, acts, "run:hid/mouse", "wake:63")
		m.Step(Event{Now: at(4), Exit: takesExit(map[state.HoldKey]Outcome{mouseKey: {NotHere: true}})})
		// The cooled screen runs when the cooldown ends.
		want(t, m.Step(Event{Now: at(5)}), "wake:6")
		want(t, m.Step(Event{Now: at(6)}), "run:screen/LG", "wake:66")
	})

	t.Run("three takes of one name in 30 s open the breaker for every name", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		seq := state.Seq(3)
		for i, sec := range []int{0, 6, 12} {
			acts := handled(m, sec, screenHold("LG", "mac", "linux", seq+state.Seq(i)))
			want(t, acts, "run:screen/LG", fmt.Sprintf("wake:%d", sec+60))
			m.Step(Event{Now: at(sec + 1), Exit: okAll(lgKey)})
		}
		// The third ended at 13 s: the breaker is open until 73 s.
		acts := handled(m, 20, hidHold("mouse", "mac", "linux", 9, false))
		want(t, acts, "wake:73")
		wantLog(t, acts, "breaker open")
		want(t, m.Step(Event{Now: at(72)}), "wake:73")
		want(t, m.Step(Event{Now: at(73)}), "run:hid/mouse", "wake:133")
	})

	t.Run("takes spread beyond the window never trip it", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		for i, sec := range []int{0, 20, 40, 60, 80} {
			acts := handled(m, sec, screenHold("LG", "mac", "linux", state.Seq(3+i)))
			want(t, acts, "run:screen/LG", fmt.Sprintf("wake:%d", sec+60))
			m.Step(Event{Now: at(sec + 1), Exit: okAll(lgKey)})
		}
	})

	t.Run("the breaker never blocks an activation or a deactivation", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		for i, sec := range []int{0, 6, 12} {
			handled(m, sec, screenHold("LG", "mac", "linux", state.Seq(3+i)))
			m.Step(Event{Now: at(sec + 1), Exit: okAll(lgKey)})
		}
		want(t, m.Step(state_(mk("linux", 3), at(14))), "activate:3", "wake:74")
	})
}

// Only takes this host attempted to move count for the cooldown and the
// breaker: the peripheral that carried the gesture is NotHere on every
// handover, and neither is a write.
func TestCooldownCountsOnlyAttempts(t *testing.T) {
	notHere := takesExit(map[state.HoldKey]Outcome{lgKey: {NotHere: true}})
	hold := func(seq state.Seq) *state.ServerState { return mk("mac", 2, screenHold("LG", "mac", "linux", seq)) }

	t.Run("nothing to move starts no cooldown and never trips the breaker", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		for i, sec := range []int{0, 1, 2, 3} {
			// Each take follows the previous at once: no cooldown defers it.
			want(t, m.Step(state_(hold(state.Seq(3+i)), at(sec))), "run:screen/LG", fmt.Sprintf("wake:%d", sec+60))
			acts := m.Step(Event{Now: at(sec), Exit: notHere})
			want(t, acts, "save")
			if rec := saved(t, acts); rec.Seen["screen/LG"] != state.Seq(3+i) {
				t.Fatalf("a NotHere take must still be marked seen: %+v", rec)
			}
		}
		if !m.breakerOpenUntil.IsZero() {
			t.Fatalf("breaker open until %v", m.breakerOpenUntil)
		}
	})

	t.Run("a failed write starts the cooldown and counts", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(hold(3), at(0)))
		fail := takesExit(map[state.HoldKey]Outcome{lgKey: {Err: errWrite}})
		for _, sec := range []int{2, 4, 6, 8} {
			m.Step(Event{Now: at(sec), Exit: fail})
		}
		if got := m.cooldownUntil[lgKey]; !got.Equal(at(13)) {
			t.Fatalf("cooldown until %v, want %v", got, at(13))
		}
		if len(m.completed[lgKey]) != 1 {
			t.Fatalf("breaker count = %d, want 1", len(m.completed[lgKey]))
		}
		want(t, m.Step(state_(hold(4), at(9))), "wake:13")
	})

	t.Run("a take whose run never reported counts", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(hold(3), at(0)))
		m.Step(Event{Now: at(60)})
		m.Step(Event{Now: at(120)})
		m.Step(Event{Now: at(180)})
		m.Step(Event{Now: at(240)})
		if got := m.cooldownUntil[lgKey]; !got.Equal(at(245)) || len(m.completed[lgKey]) != 1 {
			t.Fatalf("cooldown %v, count %d; want a counted take", got, len(m.completed[lgKey]))
		}
	})

	t.Run("a take missing from the exit counts", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(hold(3), at(0)))
		for _, sec := range []int{2, 4, 6, 8} {
			m.Step(Event{Now: at(sec), Exit: takesExit(map[state.HoldKey]Outcome{})})
		}
		if len(m.completed[lgKey]) != 1 {
			t.Fatalf("breaker count = %d, want 1", len(m.completed[lgKey]))
		}
	})

	t.Run("superseded before any run reported it as attempted does not count", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(hold(3), at(0)))
		m.Step(state_(hold(4), at(1))) // the run is still outstanding
		want(t, m.Step(Event{Now: at(2), Exit: notHere}), "save", "run:screen/LG", "wake:62")
		if len(m.completed[lgKey]) != 0 || !m.cooldownUntil[lgKey].IsZero() {
			t.Fatalf("cooldown %v, count %d; want neither", m.cooldownUntil[lgKey], len(m.completed[lgKey]))
		}
	})

	t.Run("a mixed batch gates only the key that was written", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		s := func(lg, mouse state.Seq) *state.ServerState {
			return mk("mac", 2, hidHold("mouse", "mac", "linux", mouse, false), screenHold("LG", "mac", "linux", lg))
		}
		want(t, m.Step(state_(s(3, 3), at(0))), "run:hid/mouse,screen/LG", "wake:60")
		want(t, m.Step(Event{Now: at(1), Exit: takesExit(map[state.HoldKey]Outcome{
			mouseKey: {NotHere: true}, lgKey: {},
		})}), "save")
		// Both are taken again at once: the screen cools down, the mouse does not.
		acts := m.Step(state_(s(4, 4), at(2)))
		want(t, acts, "run:hid/mouse", "wake:62")
		wantLog(t, acts, "deferring take of screen/LG")
		if len(m.completed[mouseKey]) != 0 || len(m.completed[lgKey]) != 1 {
			t.Fatalf("breaker counts: mouse %d, LG %d; want 0 and 1", len(m.completed[mouseKey]), len(m.completed[lgKey]))
		}
	})
}

func TestWatchdogAndLateExits(t *testing.T) {
	t.Run("a lost Exit of a take counts as a failed attempt", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "linux", 3)), at(0)))
		want(t, m.Step(Event{Now: at(59)}), "wake:60")
		acts := m.Step(Event{Now: at(60)})
		want(t, acts, "run:screen/LG", "wake:120")
		wantLog(t, acts, "no Exit within 1m0s")
	})

	t.Run("a lost Exit of a take exhausts the attempts and notifies", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "linux", 3)), at(0)))
		want(t, m.Step(Event{Now: at(60)}), "run:screen/LG", "wake:120")
		want(t, m.Step(Event{Now: at(120)}), "run:screen/LG", "wake:180")
		want(t, m.Step(Event{Now: at(180)}), "run:screen/LG", "wake:240")
		want(t, m.Step(Event{Now: at(240)}), "notify", "save")
	})

	t.Run("a lost Exit of a command is retried", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.Step(state_(mk("mac", 2), at(0)))
		acts := m.Step(Event{Now: at(60)})
		want(t, acts, "deactivate+retry", "wake:120")
	})

	t.Run("an Exit with no run outstanding is ignored", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		acts := m.Step(Event{Now: at(0), Exit: &Exit{}})
		want(t, acts)
		wantLog(t, acts, "ignoring late")
	})

	t.Run("an Exit after its batch ended is ignored", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "linux", 3)), at(0)))
		m.Step(Event{Now: at(1), Exit: okAll(lgKey)})
		acts := m.Step(Event{Now: at(2), Exit: okAll(lgKey)})
		want(t, acts)
		wantLog(t, acts, "ignoring late")
	})

	t.Run("a command Exit during a run of takes is ignored", func(t *testing.T) {
		m := newMachine("linux", agentAt("mac", 2, nil))
		m.Step(state_(mk("mac", 2, screenHold("LG", "mac", "linux", 3)), at(0)))
		acts := m.Step(Event{Now: at(1), Exit: &Exit{}})
		wantLog(t, acts, "ignoring late or mismatched")
		want(t, m.Step(Event{Now: at(2), Exit: okAll(lgKey)}), "save")
	})

	t.Run("a takes Exit during a command is ignored", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.Step(state_(mk("mac", 2), at(0)))
		acts := m.Step(Event{Now: at(1), Exit: okAll(lgKey)})
		wantLog(t, acts, "ignoring late or mismatched")
		want(t, m.Step(Event{Now: at(2), Exit: &Exit{}}), "save")
	})
}

func TestAttachSettle(t *testing.T) {
	t.Run("a stable attach claims after the settle", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		want(t, m.Step(Event{Now: at(0), Attach: true}), "wake:2")
		want(t, m.Step(Event{Now: at(1)}), "wake:2")
		want(t, m.Step(Event{Now: at(2)}), "claim:linux")
		want(t, m.Step(Event{Now: at(3)}))
	})

	t.Run("a flapping attach restarts the settle", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.Step(Event{Now: at(0), Attach: true})
		want(t, m.Step(Event{Now: at(1), Attach: true}), "wake:3")
		want(t, m.Step(Event{Now: at(2)}), "wake:3")
		want(t, m.Step(Event{Now: at(3)}), "claim:linux")
	})

	t.Run("an attach during a deactivation settles when it ends", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.Step(state_(mk("mac", 2), at(0)))
		want(t, m.Step(Event{Now: at(1), Attach: true}), "wake:60")
		want(t, m.Step(Event{Now: at(5), Exit: &Exit{}}), "save", "wake:7")
		want(t, m.Step(Event{Now: at(7)}), "claim:linux")
	})

	t.Run("a settle that elapsed during a deactivation fires once it ends", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.Step(Event{Now: at(0), Attach: true})
		m.Step(state_(mk("mac", 2), at(1)))
		// The settle is due at 2 s but a deactivation runs.
		want(t, m.Step(Event{Now: at(2)}), "wake:61")
		acts := m.Step(Event{Now: at(3), Exit: &Exit{}})
		want(t, acts, "save", "claim:linux")
	})

	t.Run("a claim is not blocked by the cooldown or the breaker", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		m.breakerOpenUntil = at(100)
		m.Step(Event{Now: at(0), Attach: true})
		want(t, m.Step(Event{Now: at(2)}), "claim:linux")
	})

	t.Run("a stale epoch still reconciles", func(t *testing.T) {
		m := newMachine("linux", agentAt("linux", 1, nil))
		s := mk("linux", 1)
		s.Epoch = 1
		want(t, m.Step(state_(s, at(0))))
		s2 := mk("mac", 2)
		s2.Epoch = 0 // the server's state was restored from a backup
		want(t, m.Step(state_(s2, at(1))), "deactivate", "wake:61")
	})
}

func TestWakeAt(t *testing.T) {
	// WakeAt is its own action, the tail of the batch, at most once.
	m := newMachine("linux", agentAt("mac", 2, nil))
	m.Step(Event{Now: at(0), Attach: true})
	acts := m.Step(state_(mk("mac", 2, hidHold("mouse", "mac", "linux", 3, false), screenHold("LG", "mac", "linux", 3)), at(1)))
	n := 0
	for i, a := range acts {
		if !a.WakeAt.IsZero() {
			n++
			if i != len(acts)-1 {
				t.Errorf("WakeAt at %d of %d, want the tail", i, len(acts))
			}
			if a.Claim != "" || a.Run != nil || a.Save != nil || a.Log != "" {
				t.Errorf("WakeAt shares an action: %+v", a)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d WakeAt actions, want 1: %+v", n, acts)
	}
	// The earliest deadline wins: the settle at 2 s, not the watchdog.
	if !acts[len(acts)-1].WakeAt.Equal(at(2)) {
		t.Fatalf("WakeAt = %v, want %v", acts[len(acts)-1].WakeAt, at(2))
	}

	// With nothing pending there is no WakeAt at all.
	if got := newMachine("linux", agentAt("mac", 2, nil)).Step(Event{Now: at(0)}); len(got) != 0 {
		t.Fatalf("idle machine returned %+v", got)
	}
}

func ExampleMachine_Step() {
	m := NewMachine(testConfig("linux"), state.AgentState{LastOwner: "linux"}, true)
	acts := m.Step(Event{
		Now: time.Unix(0, 0),
		State: &state.ServerState{
			Owner:      "mac",
			Activation: 2,
		},
	})
	fmt.Println("action:", acts[0].Deactivate, acts[0].Log)
	// Output: action: true deactivating: ownership went to "mac"
}
