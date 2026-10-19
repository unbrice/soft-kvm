// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// decider_test.go: table-driven tests of the pure decision state machine.

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

func testConfig(id string) DeciderConfig {
	return DeciderConfig{
		ID:             id,
		OnActivation:   [][]string{{TakeScreen}},
		OnDeactivation: [][]string{{"down"}},
		Settle:         2 * time.Second,
		SwitchRetries:  3,
		RetrySpacing:   1 * time.Second,
		ArrivalWindow:  10 * time.Second,
		Cooldown:       5 * time.Second,
		BreakerWindow:  30 * time.Second,
		BreakerMax:     3,
		BreakerPause:   60 * time.Second,
	}
}

var t0 = time.Unix(1000, 0)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// Assignment keys used throughout.
var (
	lgKey    = state.AssignmentKey{Kind: state.KindScreen, Name: "LG"}
	mouseKey = state.AssignmentKey{Kind: state.KindHID, Name: "mouse"}
)

func screenAssignment(name, assignee, prev string, seq state.Seq) state.Assignment {
	return state.Assignment{Kind: state.KindScreen, Name: name, Assignee: assignee, Prev: prev, Seq: seq, Input: "F4:50=D1"}
}

func hidAssignment(name, assignee, prev string, seq state.Seq, arrived bool) state.Assignment {
	return state.Assignment{Kind: state.KindHID, Name: name, Assignee: assignee, Prev: prev, Seq: seq, Channel: 2, Arrived: arrived}
}

// mk builds a state with every host live.
func mk(owner string, activation int64, assignments ...state.Assignment) *state.ServerState {
	return &state.ServerState{
		Owner:       owner,
		Activation:  activation,
		Epoch:       activation + int64(len(assignments)),
		Live:        map[string]bool{"linux": true, "mac": true},
		Assignments: assignments,
	}
}

func notLive(s *state.ServerState, id string) *state.ServerState {
	s.Live = map[string]bool{"linux": true, "mac": true}
	s.Live[id] = false
	return s
}

// agentAt is the record of an agent that follows owner at activation, having
// handled the given assignments.
func agentAt(owner string, activation int64, seen map[state.AssignmentKey]state.Seq) state.AgentState {
	rec := state.AgentState{HandledOwner: owner, HandledActivation: activation, HandledSeqs: map[string]state.Seq{}}
	for k, seq := range seen {
		rec.HandledSeqs[k.String()] = seq
	}
	return rec
}

func newDecider(id string, rec state.AgentState) *Decider {
	return NewDecider(testConfig(id), &rec)
}

// outcome is what one Decide returns.
type outcome struct {
	d    []Decision
	wake time.Time
}

// decide is Decide, as the tests read it.
func (m *Decider) decide(now time.Time, o Observation) outcome {
	ds, wake := m.Decide(now, o)
	return outcome{ds, wake}
}

// sum renders the decisions that do something as short strings, then the wake
// as "wake:N" when there is one, so a test reads as the sequence it expects.
// Log-only decisions are left out; logs() returns them.
func sum(o outcome) []string {
	var out []string
	for _, d := range o.d {
		var parts []string
		if d.Claim != "" {
			parts = append(parts, "claim:"+d.Claim)
		}
		if len(d.Run) > 0 {
			names := make([]string, len(d.Run))
			for i, c := range d.Run {
				names[i] = c.String()
				switch {
				case c.TakeScreen != 0:
					names[i] += fmt.Sprintf("@%d", c.TakeScreen)
				case c.TakeHID != 0:
					names[i] += fmt.Sprintf("@%d", c.TakeHID)
				}
			}
			parts = append(parts, "run:"+strings.Join(names, ","))
		}
		if len(d.GiveAway) > 0 {
			names := make([]string, len(d.GiveAway))
			for i, h := range d.GiveAway {
				names[i] = h.Key().String()
			}
			parts = append(parts, "give:"+strings.Join(names, ","))
		}
		if d.Notify {
			parts = append(parts, "notify")
		}
		if d.Save != nil {
			parts = append(parts, "save")
		}
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, "+"))
		}
	}
	if !o.wake.IsZero() {
		out = append(out, fmt.Sprintf("wake:%d", int(o.wake.Sub(t0)/time.Second)))
	}
	return out
}

func logs(o outcome) string {
	var out []string
	for _, d := range o.d {
		if d.Log != "" {
			out = append(out, d.Log)
		}
	}
	return strings.Join(out, "\n")
}

func want(t *testing.T, ds outcome, w ...string) {
	t.Helper()
	if got := sum(ds); !slices.Equal(got, w) {
		t.Fatalf("decisions = %q, want %q\nlogs:\n%s", got, w, logs(ds))
	}
}

func wantLog(t *testing.T, ds outcome, substr string) {
	t.Helper()
	if !strings.Contains(logs(ds), substr) {
		t.Fatalf("logs %q lack %q", logs(ds), substr)
	}
}

func saved(t *testing.T, ds outcome) *state.AgentState {
	t.Helper()
	var rec *state.AgentState
	for _, d := range ds.d {
		if d.Save != nil {
			rec = d.Save
		}
	}
	if rec == nil {
		t.Fatalf("no Save in %+v", ds)
	}
	return rec
}

func state_(s *state.ServerState) Observation { return Observation{State: s} }

// ran is the result of a run of commands, one error per command.
func ran(results ...error) *CommandsRun { return &CommandsRun{Results: results} }

func okAll(keys ...state.AssignmentKey) *AssignmentsGivenAway {
	results := map[state.AssignmentKey]error{}
	for _, k := range keys {
		results[k] = nil
	}
	return &AssignmentsGivenAway{Results: results}
}

func given(results map[state.AssignmentKey]error) *AssignmentsGivenAway {
	return &AssignmentsGivenAway{Results: results}
}

var errWrite = errors.New("ddcutil: no i2c-dev")

func TestFirstReconcileAdopts(t *testing.T) {
	m := NewDecider(testConfig("linux"), nil)
	s := mk("mac", 4, screenAssignment("LG", "mac", "linux", 3), hidAssignment("mouse", "mac", "linux", 4, false))

	ds := m.decide(at(0), state_(s))
	want(t, ds, "save")
	wantLog(t, ds, "adopted")
	rec := saved(t, ds)
	if rec.HandledOwner != "mac" || rec.HandledActivation != 4 ||
		rec.HandledSeqs["screen/LG"] != 3 || rec.HandledSeqs["hid/mouse"] != 4 {
		t.Fatalf("adopted record = %+v", rec)
	}

	// The adopted state is the baseline: the same state runs nothing, not an
	// activation (I am not the owner), not a deactivation (last_owner is mac),
	// not the takes it holds.
	want(t, m.decide(at(1), state_(s)))

	// A name the agent meets after the first reconcile is new, and so is a
	// new take of a name it adopted.
	dell := state.AssignmentKey{Kind: state.KindScreen, Name: "DELL"}
	next := mk("mac", 4, screenAssignment("DELL", "mac", "", 5), screenAssignment("LG", "mac", "linux", 3), hidAssignment("mouse", "mac", "linux", 4, false))
	ds = m.decide(at(2), state_(next))
	want(t, ds, "give:screen/DELL")
	want(t, m.decide(at(3), Observation{GaveAway: okAll(dell)}), "save")
	next = mk("mac", 4, screenAssignment("DELL", "mac", "", 5), screenAssignment("LG", "mac", "linux", 6), hidAssignment("mouse", "mac", "linux", 4, false))
	ds = m.decide(at(10), state_(next))
	want(t, ds, "give:screen/LG")
}

// A restarted agent builds its decider from agent.json: what it handled stays
// handled.
func TestRecordSurvivesARestart(t *testing.T) {
	m := newDecider("linux", agentAt("mac", 2, nil))
	s := mk("mac", 2, screenAssignment("LG", "mac", "linux", 3))
	ds := m.decide(at(0), state_(s))
	want(t, ds, "give:screen/LG")
	ds = m.decide(at(1), Observation{GaveAway: okAll(lgKey)})
	want(t, ds, "save")

	again := NewDecider(testConfig("linux"), saved(t, ds))
	want(t, again.decide(at(100), state_(s)))
}

func TestActivation(t *testing.T) {
	t.Run("ownership comes here: run, then save the record", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 1, nil))
		ds := m.decide(at(0), state_(mk("linux", 2)))
		want(t, ds, "run:take-screen@2")
		if c := ds.d[1].Run; len(c) != 1 || c[0].TakeScreen != 2 {
			t.Fatalf("run decision = %+v", ds.d[1])
		}
		ds = m.decide(at(2), Observation{Ran: ran(nil)})
		want(t, ds, "save")
		if rec := saved(t, ds); rec.HandledOwner != "linux" || rec.HandledActivation != 2 {
			t.Fatalf("record = %+v", rec)
		}
		// Nothing more for the same transition.
		want(t, m.decide(at(3), state_(mk("linux", 2))))
	})

	t.Run("a re-activation of the owner runs the commands again", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 2, nil))
		want(t, m.decide(at(0), state_(mk("linux", 2))))
		ds := m.decide(at(1), state_(mk("linux", 3)))
		want(t, ds, "run:take-screen@3")
		ds = m.decide(at(2), Observation{Ran: ran(nil)})
		if rec := saved(t, ds); rec.HandledActivation != 3 {
			t.Fatalf("record = %+v", rec)
		}
		want(t, m.decide(at(3), state_(mk("linux", 3))))
	})

	t.Run("an agent that never activated runs on an owner it already was", func(t *testing.T) {
		m := newDecider("linux", agentAt("", 0, nil))
		want(t, m.decide(at(0), state_(mk("linux", 1))), "run:take-screen@1")
	})

	t.Run("a failure retries only after the spacing, then is saved without a notification", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 1, nil))
		m.decide(at(0), state_(mk("linux", 2)))

		// Fast failure: the retry waits for 1 s after the previous start.
		ds := m.decide(at(0), Observation{Ran: ran(errors.New("exit 1"))})
		want(t, ds, "wake:1")
		ds = m.decide(at(1), Observation{})
		want(t, ds, "run:take-screen@2")
		if ds.d[0].Run[0].TakeScreen != 2 {
			t.Fatalf("retry carries activation %d, want 2", ds.d[0].Run[0].TakeScreen)
		}
		// A slow failure retries at once.
		want(t, m.decide(at(5), Observation{Ran: ran(errors.New("exit 1"))}), "run:take-screen@2")
		want(t, m.decide(at(9), Observation{Ran: ran(errors.New("exit 1"))}), "run:take-screen@2")

		// 1 + 3 retries are spent: the failure is logged, never notified, and
		// the record moves on so the same transition does not run again.
		ds = m.decide(at(12), Observation{Ran: ran(errors.New("exit 1"))})
		want(t, ds, "save")
		wantLog(t, ds, "failed after 4 attempts")
		if rec := saved(t, ds); rec.HandledOwner != "linux" || rec.HandledActivation != 2 {
			t.Fatalf("record = %+v", rec)
		}
		want(t, m.decide(at(13), state_(mk("linux", 2))))
	})

	t.Run("never waits behind the cooldown or the breaker", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 1, nil))
		m.limiter.cooldownUntil[lgKey] = at(100)
		m.limiter.pausedUntil = at(100)
		want(t, m.decide(at(0), state_(mk("linux", 2))), "run:take-screen@2")
	})

	t.Run("a flip away during the activation deactivates when it ends", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 1, nil))
		m.decide(at(0), state_(mk("linux", 2)))
		// Stashed: the activation is still running.
		want(t, m.decide(at(1), state_(mk("mac", 3))))
		// The activation spends its attempts first (a 409 each time), then the
		// deactivation runs right after.
		for _, sec := range []int{2, 3, 5} {
			want(t, m.decide(at(sec), Observation{Ran: ran(errors.New("409"))}), "run:take-screen@2")
		}
		ds := m.decide(at(7), Observation{Ran: ran(errors.New("409"))})
		want(t, ds, "save", "run:down")
		if rec := saved(t, ds); rec.HandledOwner != "linux" {
			t.Fatalf("record = %+v", rec)
		}
	})

	t.Run("a flip away during a successful activation deactivates right after", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 1, nil))
		m.decide(at(0), state_(mk("linux", 2)))
		want(t, m.decide(at(1), state_(mk("mac", 3))))
		ds := m.decide(at(2), Observation{Ran: ran(nil)})
		want(t, ds, "save", "run:down")
	})
}

func TestDeactivation(t *testing.T) {
	t.Run("ownership goes away: run, then save the new owner", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		ds := m.decide(at(0), state_(mk("mac", 2)))
		want(t, ds, "run:down")
		ds = m.decide(at(2), Observation{Ran: ran(nil)})
		want(t, ds, "save")
		if rec := saved(t, ds); rec.HandledOwner != "mac" {
			t.Fatalf("record = %+v", rec)
		}
		want(t, m.decide(at(3), state_(mk("mac", 2))))
	})

	t.Run("no liveness gate", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		want(t, m.decide(at(0), state_(notLive(mk("mac", 2), "mac"))), "run:down")
	})

	t.Run("no cooldown and no breaker", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.limiter.cooldownUntil[lgKey] = at(100)
		m.limiter.pausedUntil = at(100)
		want(t, m.decide(at(0), state_(mk("mac", 2))), "run:down")
	})

	t.Run("an empty owner deactivates nothing", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		want(t, m.decide(at(0), state_(mk("", 0))))
	})

	t.Run("a host that was not the owner has nothing to deactivate", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 1, nil))
		want(t, m.decide(at(0), state_(mk("third", 2))))
	})

	t.Run("a third host as owner deactivates the previous one", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		want(t, m.decide(at(0), state_(mk("third", 2))), "run:down")
	})

	t.Run("failure retries, is logged, never notified, and the record moves on", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.decide(at(0), state_(mk("mac", 2)))
		want(t, m.decide(at(0), Observation{Ran: ran(errors.New("x"))}), "wake:1")
		want(t, m.decide(at(1), Observation{}), "run:down")
		want(t, m.decide(at(3), Observation{Ran: ran(errors.New("x"))}), "run:down")
		want(t, m.decide(at(5), Observation{Ran: ran(errors.New("x"))}), "run:down")
		ds := m.decide(at(7), Observation{Ran: ran(errors.New("x"))})
		want(t, ds, "save")
		wantLog(t, ds, "failed after 4 attempts")
	})

	t.Run("a flip back during the deactivation activates when it ends", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.decide(at(0), state_(mk("mac", 2)))
		want(t, m.decide(at(1), state_(mk("linux", 3))))
		ds := m.decide(at(2), Observation{Ran: ran(nil)})
		want(t, ds, "save", "run:take-screen@3")
	})

	t.Run("a flip back during a failing deactivation activates when it ends", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.decide(at(0), state_(mk("mac", 2)))
		want(t, m.decide(at(1), state_(mk("linux", 3))))
		m.decide(at(2), Observation{Ran: ran(errors.New("x"))})
		m.decide(at(3), Observation{})
		m.decide(at(5), Observation{Ran: ran(errors.New("x"))})
		m.decide(at(7), Observation{Ran: ran(errors.New("x"))})
		ds := m.decide(at(9), Observation{Ran: ran(errors.New("x"))})
		want(t, ds, "save", "run:take-screen@3")
	})
}

// Each -A or -D command is a task of its own: a retry re-runs only those that
// failed, in their order (SPEC §4.3).
func TestCommands(t *testing.T) {
	two := func(id string, rec state.AgentState) *Decider {
		cfg := testConfig(id)
		cfg.OnActivation = [][]string{{"a"}, {"b"}, {"c"}}
		return NewDecider(cfg, &rec)
	}

	t.Run("a retry re-runs only the commands that failed", func(t *testing.T) {
		m := two("linux", agentAt("mac", 1, nil))
		want(t, m.decide(at(0), state_(mk("linux", 2))), "run:a,b,c")
		want(t, m.decide(at(0), Observation{Ran: ran(errors.New("x"), nil, errors.New("y"))}), "wake:1")
		want(t, m.decide(at(1), Observation{}), "run:a,c")
		ds := m.decide(at(2), Observation{Ran: ran(nil, nil)})
		want(t, ds, "save")
		wantLog(t, ds, `recording owner "linux" (activation 2)`)
	})

	t.Run("a command with no result counts as failed", func(t *testing.T) {
		m := two("linux", agentAt("mac", 1, nil))
		m.decide(at(0), state_(mk("linux", 2)))
		want(t, m.decide(at(5), Observation{Ran: ran(nil)}), "run:b,c")
	})

	t.Run("a command still failing is logged by name, and the record moves on", func(t *testing.T) {
		m := two("linux", agentAt("mac", 1, nil))
		m.decide(at(0), state_(mk("linux", 2)))
		fail := ran(nil, errors.New("exit 1"), nil)
		var ds outcome
		for i := range 4 {
			ds = m.decide(at(1+i), Observation{Ran: fail})
			fail = ran(errors.New("exit 1"))
		}
		want(t, ds, "save")
		wantLog(t, ds, "-A b failed after 4 attempts (exit 1)")
		if rec := saved(t, ds); rec.HandledOwner != "linux" || rec.HandledActivation != 2 {
			t.Fatalf("record = %+v", rec)
		}
	})

	t.Run("a side with no commands records the change at once and goes on", func(t *testing.T) {
		cfg := testConfig("linux")
		cfg.OnDeactivation = nil
		rec := agentAt("linux", 1, nil)
		m := NewDecider(cfg, &rec)
		ds := m.decide(at(0), state_(mk("mac", 2, screenAssignment("LG", "mac", "linux", 3))))
		want(t, ds, "save", "give:screen/LG")
		if strings.Contains(logs(ds), "running") {
			t.Fatalf("logs %q", logs(ds))
		}
		if rec := saved(t, ds); rec.HandledOwner != "mac" {
			t.Fatalf("record = %+v", rec)
		}
	})
}

func TestGiveAwayGates(t *testing.T) {
	// Every combination of the §4.3 gates a pending assignment passes.
	for _, seenSame := range []bool{false, true} {
		for _, arrived := range []bool{false, true} {
			for _, assigneeLive := range []bool{true, false} {
				for _, cooling := range []bool{false, true} {
					for _, breaker := range []bool{false, true} {
						for _, assigneeIsMe := range []bool{false, true} {
							name := fmt.Sprintf("seen=%v arrived=%v live=%v cooling=%v breaker=%v mine=%v",
								seenSame, arrived, assigneeLive, cooling, breaker, assigneeIsMe)
							t.Run(name, func(t *testing.T) {
								seen := map[state.AssignmentKey]state.Seq{}
								if seenSame {
									seen[mouseKey] = 7
								} else {
									seen[mouseKey] = 6
								}
								m := newDecider("linux", agentAt("mac", 2, seen))
								assignee := "mac"
								if assigneeIsMe {
									assignee = "linux"
								}
								s := mk("mac", 2, hidAssignment("mouse", assignee, "linux", 7, arrived))
								notLive(s, "mac")
								s.Live["mac"] = assigneeLive
								if cooling {
									m.limiter.cooldownUntil[mouseKey] = at(3)
								}
								if breaker {
									m.limiter.pausedUntil = at(30)
								}
								ds := m.decide(at(0), state_(s))

								switch {
								case assigneeIsMe, seenSame:
									want(t, ds)
								case arrived:
									want(t, ds, "save")
									if saved(t, ds).HandledSeqs["hid/mouse"] != 7 {
										t.Fatal("an arrived assignment is not marked seen")
									}
								case !assigneeLive:
									want(t, ds)
									wantLog(t, ds, "not live")
								case breaker:
									want(t, ds, "wake:30")
									wantLog(t, ds, "every give-away paused")
								case cooling:
									want(t, ds, "wake:3")
									wantLog(t, ds, "cooldown")
								default:
									want(t, ds, "give:hid/mouse")
								}
							})
						}
					}
				}
			}
		}
	}
}

// A reason for holding an assignment back is logged once, however often the
// decider reconciles while idle, and again only once it changed.
func TestHoldBackReasonsLogOnce(t *testing.T) {
	m := newDecider("linux", agentAt("mac", 2, nil))
	s := notLive(mk("mac", 2, screenAssignment("LG", "mac", "linux", 3)), "mac")
	ds := m.decide(at(0), state_(s))
	want(t, ds)
	wantLog(t, ds, "is not live")
	// An attach and its claim reconcile again: nothing new to say.
	for _, ds := range []outcome{m.decide(at(1), Observation{Attach: true}), m.decide(at(3), Observation{})} {
		if strings.Contains(logs(ds), "not live") {
			t.Fatalf("logs %q repeat the reason", logs(ds))
		}
	}
	// The assignee comes back: LG is given away, and the reason forgotten.
	want(t, m.decide(at(4), state_(mk("mac", 2, screenAssignment("LG", "mac", "linux", 3)))), "give:screen/LG")
	m.decide(at(5), Observation{GaveAway: okAll(lgKey)})
	// A newer take to a host that is not live is a new reason.
	wantLog(t, m.decide(at(6), state_(notLive(mk("mac", 2, screenAssignment("LG", "mac", "linux", 4)), "mac"))), "is not live")
}

func TestGiveAway(t *testing.T) {
	t.Run("a live assignee's new assignment is given away and handled once", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		s := mk("mac", 2, screenAssignment("LG", "mac", "linux", 3))
		ds := m.decide(at(0), state_(s))
		want(t, ds, "give:screen/LG")
		if ds.d[0].GiveAway[0].Input != "F4:50=D1" {
			t.Fatalf("give away = %+v", ds.d[0].GiveAway)
		}
		ds = m.decide(at(1), Observation{GaveAway: okAll(lgKey)})
		want(t, ds, "save")
		if rec := saved(t, ds); rec.HandledSeqs["screen/LG"] != 3 {
			t.Fatalf("record = %+v", rec)
		}
		want(t, m.decide(at(2), state_(s)))
	})

	t.Run("one run in the order /state lists them", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		s := mk("mac", 2, hidAssignment("keyboard", "mac", "linux", 3, false),
			hidAssignment("mouse", "mac", "linux", 3, false), screenAssignment("LG", "mac", "linux", 3))
		want(t, m.decide(at(0), state_(s)), "give:hid/keyboard,hid/mouse,screen/LG")
	})

	t.Run("nothing to move here is done, silently", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, screenAssignment("LG", "mac", "linux", 3))))
		ds := m.decide(at(1), Observation{GaveAway: given(map[state.AssignmentKey]error{lgKey: ErrNotHere})})
		want(t, ds, "save")
		if strings.Contains(logs(ds), "failed") {
			t.Fatalf("logs %q", logs(ds))
		}
	})

	t.Run("a failed write retries only that assignment, spaced by the retry spacing", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		s := mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, false), screenAssignment("LG", "mac", "linux", 3))
		m.decide(at(0), state_(s))
		ds := m.decide(at(0), Observation{GaveAway: given(map[state.AssignmentKey]error{
			mouseKey: ErrNotHere, lgKey: errWrite,
		})})
		want(t, ds, "wake:1")
		ds = m.decide(at(1), Observation{})
		want(t, ds, "give:screen/LG")

		// A slow failure retries at once.
		ds = m.decide(at(5), Observation{GaveAway: given(map[state.AssignmentKey]error{lgKey: errWrite})})
		want(t, ds, "give:screen/LG")
		ds = m.decide(at(8), Observation{GaveAway: given(map[state.AssignmentKey]error{lgKey: nil})})
		want(t, ds, "save")
	})

	t.Run("an assignment that reports nothing counts as failed", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, screenAssignment("LG", "mac", "linux", 3))))
		ds := m.decide(at(5), Observation{GaveAway: given(map[state.AssignmentKey]error{})})
		want(t, ds, "give:screen/LG")
	})

	t.Run("failing after the last retry notifies the previous assignee", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		s := mk("mac", 2, screenAssignment("LG", "mac", "linux", 3))
		m.decide(at(0), state_(s))
		fail := given(map[state.AssignmentKey]error{lgKey: errWrite})
		now := 0
		for i := 0; i < 3; i++ {
			now += 2
			ds := m.decide(at(now), Observation{GaveAway: fail})
			want(t, ds, "give:screen/LG")
		}
		now += 2
		ds := m.decide(at(now), Observation{GaveAway: fail})
		want(t, ds, "notify+save")
		wantLog(t, ds, "failed after 4 attempts")
		if rec := saved(t, ds); rec.HandledSeqs["screen/LG"] != 3 {
			t.Fatalf("a failure is final for that seq: %+v", rec)
		}
		want(t, m.decide(at(now+1), state_(s)))
	})

	t.Run("a host that is not the previous assignee only logs", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, screenAssignment("LG", "mac", "third", 3))))
		fail := given(map[state.AssignmentKey]error{lgKey: errWrite})
		var ds outcome
		for i := 1; i <= 4; i++ {
			ds = m.decide(at(2*i), Observation{GaveAway: fail})
		}
		want(t, ds, "save")
		wantLog(t, ds, "screen/LG failed after 4 attempts")
	})

	t.Run("a first assignment has no previous assignee and never notifies", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, screenAssignment("LG", "mac", "", 3))))
		fail := given(map[state.AssignmentKey]error{lgKey: errWrite})
		var ds outcome
		for i := 1; i <= 4; i++ {
			ds = m.decide(at(2*i), Observation{GaveAway: fail})
		}
		want(t, ds, "save")
	})

	t.Run("one failing assignment does not skip or fail the rest", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, false), screenAssignment("LG", "mac", "linux", 3))))
		m.decide(at(2), Observation{GaveAway: given(map[state.AssignmentKey]error{mouseKey: errWrite, lgKey: nil})})
		m.decide(at(4), Observation{GaveAway: given(map[state.AssignmentKey]error{mouseKey: errWrite})})
		m.decide(at(6), Observation{GaveAway: given(map[state.AssignmentKey]error{mouseKey: errWrite})})
		ds := m.decide(at(8), Observation{GaveAway: given(map[state.AssignmentKey]error{mouseKey: errWrite})})
		want(t, ds, "notify+save")
		rec := saved(t, ds)
		if rec.HandledSeqs["screen/LG"] != 3 || rec.HandledSeqs["hid/mouse"] != 3 {
			t.Fatalf("record = %+v", rec)
		}
	})

	t.Run("a state during the job is stashed, and reconciled when it ends", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, screenAssignment("LG", "mac", "linux", 3))))
		want(t, m.decide(at(1), state_(mk("mac", 2, screenAssignment("LG", "mac", "linux", 3), hidAssignment("mouse", "mac", "linux", 4, false)))))
		ds := m.decide(at(2), Observation{GaveAway: okAll(lgKey)})
		want(t, ds, "save", "give:hid/mouse")
	})

	t.Run("a flip back to me during a batch of assignments activates when it ends", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		third := func(owner string, activation int64) *state.ServerState {
			s := mk(owner, activation, screenAssignment("LG", "third", "linux", 3))
			s.Live["third"] = true
			return s
		}
		m.decide(at(0), state_(third("third", 2)))
		m.decide(at(1), Observation{Ran: ran(nil)}) // the deactivation ends, the assignment is given away
		want(t, m.decide(at(2), state_(third("linux", 3))))
		ds := m.decide(at(3), Observation{GaveAway: okAll(lgKey)})
		want(t, ds, "save", "run:take-screen@3")
	})
}

func TestArrival(t *testing.T) {
	sent := func(t *testing.T) *Decider {
		t.Helper()
		m := newDecider("linux", agentAt("mac", 2, nil))
		want(t, m.decide(at(0), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, false)))), "give:hid/mouse")
		return m
	}

	t.Run("a sent device waits for its arrival, then is done", func(t *testing.T) {
		m := sent(t)
		// Sent at 1 s: the window closes at 11 s.
		ds := m.decide(at(1), Observation{GaveAway: okAll(mouseKey)})
		want(t, ds, "wake:11")
		ds = m.decide(at(3), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, true))))
		want(t, ds, "save")
		if rec := saved(t, ds); rec.HandledSeqs["hid/mouse"] != 3 {
			t.Fatalf("record = %+v", rec)
		}
		// Nothing re-sends it, and no window fires later.
		want(t, m.decide(at(30), Observation{}))
	})

	t.Run("the device is not done on the exit alone", func(t *testing.T) {
		m := sent(t)
		m.decide(at(1), Observation{GaveAway: okAll(mouseKey)})
		// A state that does not show the arrival changes nothing.
		want(t, m.decide(at(2), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, false)))), "wake:11")
	})

	t.Run("an arrival already in the state at the exit is the receipt", func(t *testing.T) {
		m := sent(t)
		m.decide(at(1), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, true))))
		want(t, m.decide(at(2), Observation{GaveAway: okAll(mouseKey)}), "save")
	})

	t.Run("no arrival within the window re-sends, spaced and counted", func(t *testing.T) {
		m := sent(t)
		m.decide(at(1), Observation{GaveAway: okAll(mouseKey)})
		ds := m.decide(at(11), Observation{})
		want(t, ds, "give:hid/mouse")
		wantLog(t, ds, "did not arrive")
	})

	t.Run("never arriving notifies the previous assignee after the last retry", func(t *testing.T) {
		m := sent(t)
		now := 1
		m.decide(at(now), Observation{GaveAway: okAll(mouseKey)})
		for i := 0; i < 3; i++ {
			now += 10
			want(t, m.decide(at(now), Observation{}), "give:hid/mouse")
			now++
			want(t, m.decide(at(now), Observation{GaveAway: okAll(mouseKey)}), fmt.Sprintf("wake:%d", now+10))
		}
		now += 10
		ds := m.decide(at(now), Observation{})
		want(t, ds, "notify+save")
		wantLog(t, ds, "did not arrive")
	})

	t.Run("an assignment superseded while awaiting is dropped, and its successor is pending", func(t *testing.T) {
		m := sent(t)
		m.decide(at(1), Observation{GaveAway: okAll(mouseKey)})
		// The successor appears while the mouse awaits: the old assignment is
		// dropped and the batch ends at once, without marking the new seq seen.
		ds := m.decide(at(2), state_(mk("mac", 2, hidAssignment("mouse", "linux", "mac", 4, false))))
		want(t, ds, "save")
		wantLog(t, ds, "a newer take replaced its assignment")
		if rec := saved(t, ds); rec.HandledSeqs["hid/mouse"] != 3 {
			t.Fatalf("record = %+v, want the old seq handled and the new one not", rec)
		}
		// The new assignee is me: nothing to run for it, and no window fires.
		want(t, m.decide(at(12), Observation{}))
	})

	t.Run("a superseding take by another host is given away after the cooldown", func(t *testing.T) {
		m := sent(t)
		m.decide(at(1), Observation{GaveAway: okAll(mouseKey)})
		third := mk("mac", 2, hidAssignment("mouse", "third", "mac", 4, false))
		third.Live["third"] = true
		// The batch ends at 2 s; the mouse cooled down for 5 s since.
		ds := m.decide(at(2), state_(third))
		want(t, ds, "save", "wake:7")
		wantLog(t, ds, "cooldown")
		want(t, m.decide(at(6), Observation{}), "wake:7")
		want(t, m.decide(at(7), Observation{}), "give:hid/mouse")
	})

	t.Run("a failed assignment superseded before its retry is not retried", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, false))))
		want(t, m.decide(at(0), Observation{GaveAway: given(map[state.AssignmentKey]error{mouseKey: errWrite})}), "wake:1")
		ds := m.decide(at(0), state_(mk("mac", 2, hidAssignment("mouse", "linux", "mac", 4, false))))
		want(t, ds, "save")
		want(t, m.decide(at(1), Observation{}))
	})

	t.Run("an assignment that vanished is superseded", func(t *testing.T) {
		m := sent(t)
		m.decide(at(1), Observation{GaveAway: okAll(mouseKey)})
		want(t, m.decide(at(2), state_(mk("mac", 2))), "save")
	})

	t.Run("a screen failing while a device awaits retries without waiting for the window", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, false), screenAssignment("LG", "mac", "linux", 3))))
		ds := m.decide(at(0), Observation{GaveAway: given(map[state.AssignmentKey]error{mouseKey: nil, lgKey: errWrite})})
		want(t, ds, "wake:1")
		ds = m.decide(at(1), Observation{})
		want(t, ds, "give:screen/LG", "wake:10")
		ds = m.decide(at(2), Observation{GaveAway: given(map[state.AssignmentKey]error{lgKey: nil})})
		want(t, ds, "wake:10")
		want(t, m.decide(at(3), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, true), screenAssignment("LG", "mac", "linux", 3)))), "save")
	})

	t.Run("a device already on the assignee is never sent", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		ds := m.decide(at(0), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, true))))
		want(t, ds, "save")
	})

	t.Run("a device already arrived is recorded with the screen given away beside it", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		ds := m.decide(at(0), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, true), screenAssignment("LG", "mac", "linux", 3))))
		want(t, ds, "give:screen/LG")
		wantLog(t, ds, "already arrived")
		ds = m.decide(at(1), Observation{GaveAway: okAll(lgKey)})
		want(t, ds, "save")
		if rec := saved(t, ds); rec.HandledSeqs["hid/mouse"] != 3 || rec.HandledSeqs["screen/LG"] != 3 {
			t.Fatalf("record = %+v", rec)
		}
	})
}

func TestCooldownAndBreaker(t *testing.T) {
	handled := func(m *Decider, sec int, assignments ...state.Assignment) outcome {
		return m.decide(at(sec), state_(mk("mac", 2, assignments...)))
	}

	t.Run("cooldown is per name", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		handled(m, 0, screenAssignment("LG", "mac", "linux", 3))
		m.decide(at(1), Observation{GaveAway: okAll(lgKey)})
		// 2 s later: the screen cooled down until 6 s, the mouse did not.
		ds := handled(m, 3, screenAssignment("LG", "mac", "linux", 4), hidAssignment("mouse", "mac", "linux", 4, false))
		want(t, ds, "give:hid/mouse")
		m.decide(at(4), Observation{GaveAway: given(map[state.AssignmentKey]error{mouseKey: ErrNotHere})})
		// The cooled screen runs when the cooldown ends.
		want(t, m.decide(at(5), Observation{}), "wake:6")
		want(t, m.decide(at(6), Observation{}), "give:screen/LG")
	})

	t.Run("three give-aways of one name in 30 s pause every name", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		seq := state.Seq(3)
		for i, sec := range []int{0, 6, 12} {
			ds := handled(m, sec, screenAssignment("LG", "mac", "linux", seq+state.Seq(i)))
			want(t, ds, "give:screen/LG")
			m.decide(at(sec+1), Observation{GaveAway: okAll(lgKey)})
		}
		// The third ended at 13 s: the breaker is open until 73 s.
		ds := handled(m, 20, hidAssignment("mouse", "mac", "linux", 9, false))
		want(t, ds, "wake:73")
		wantLog(t, ds, "every give-away paused")
		want(t, m.decide(at(72), Observation{}), "wake:73")
		want(t, m.decide(at(73), Observation{}), "give:hid/mouse")
	})

	t.Run("give-aways spread beyond the window never trip it", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		for i, sec := range []int{0, 20, 40, 60, 80} {
			ds := handled(m, sec, screenAssignment("LG", "mac", "linux", state.Seq(3+i)))
			want(t, ds, "give:screen/LG")
			m.decide(at(sec+1), Observation{GaveAway: okAll(lgKey)})
		}
	})

	t.Run("the breaker never blocks an activation or a deactivation", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		for i, sec := range []int{0, 6, 12} {
			handled(m, sec, screenAssignment("LG", "mac", "linux", state.Seq(3+i)))
			m.decide(at(sec+1), Observation{GaveAway: okAll(lgKey)})
		}
		want(t, m.decide(at(14), state_(mk("linux", 3))), "run:take-screen@3")
	})
}

// Only assignments this host attempted to move count for the cooldown and the
// breaker: the peripheral that carried the gesture is ErrNotHere on every
// handover, and neither is a write.
func TestCooldownCountsOnlyAttempts(t *testing.T) {
	notHere := given(map[state.AssignmentKey]error{lgKey: ErrNotHere})
	assignment := func(seq state.Seq) *state.ServerState {
		return mk("mac", 2, screenAssignment("LG", "mac", "linux", seq))
	}

	t.Run("nothing to move starts no cooldown and never trips the breaker", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		for i, sec := range []int{0, 1, 2, 3} {
			// Each assignment follows the previous at once: no cooldown defers
			// it.
			want(t, m.decide(at(sec), state_(assignment(state.Seq(3+i)))), "give:screen/LG")
			ds := m.decide(at(sec), Observation{GaveAway: notHere})
			want(t, ds, "save")
			if rec := saved(t, ds); rec.HandledSeqs["screen/LG"] != state.Seq(3+i) {
				t.Fatalf("an ErrNotHere assignment must still be marked seen: %+v", rec)
			}
		}
		if !m.limiter.pausedUntil.IsZero() {
			t.Fatalf("paused until %v", m.limiter.pausedUntil)
		}
	})

	t.Run("a failed write starts the cooldown and counts", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(assignment(3)))
		fail := given(map[state.AssignmentKey]error{lgKey: errWrite})
		for _, sec := range []int{2, 4, 6, 8} {
			m.decide(at(sec), Observation{GaveAway: fail})
		}
		if got := m.limiter.cooldownUntil[lgKey]; !got.Equal(at(13)) {
			t.Fatalf("cooldown until %v, want %v", got, at(13))
		}
		if len(m.limiter.recent[lgKey]) != 1 {
			t.Fatalf("breaker count = %d, want 1", len(m.limiter.recent[lgKey]))
		}
		want(t, m.decide(at(9), state_(assignment(4))), "wake:13")
	})

	t.Run("an assignment missing from the result counts", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(assignment(3)))
		for _, sec := range []int{2, 4, 6, 8} {
			m.decide(at(sec), Observation{GaveAway: given(map[state.AssignmentKey]error{})})
		}
		if len(m.limiter.recent[lgKey]) != 1 {
			t.Fatalf("breaker count = %d, want 1", len(m.limiter.recent[lgKey]))
		}
	})

	t.Run("superseded before any run reported it as attempted does not count", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(assignment(3)))
		m.decide(at(1), state_(assignment(4))) // the job is still in flight
		want(t, m.decide(at(2), Observation{GaveAway: notHere}), "save", "give:screen/LG")
		if len(m.limiter.recent[lgKey]) != 0 || !m.limiter.cooldownUntil[lgKey].IsZero() {
			t.Fatalf("cooldown %v, count %d; want neither", m.limiter.cooldownUntil[lgKey], len(m.limiter.recent[lgKey]))
		}
	})

	t.Run("a mixed batch gates only the key that was written", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		s := func(lg, mouse state.Seq) *state.ServerState {
			return mk("mac", 2, hidAssignment("mouse", "mac", "linux", mouse, false), screenAssignment("LG", "mac", "linux", lg))
		}
		want(t, m.decide(at(0), state_(s(3, 3))), "give:hid/mouse,screen/LG")
		want(t, m.decide(at(1), Observation{GaveAway: given(map[state.AssignmentKey]error{
			mouseKey: ErrNotHere, lgKey: nil,
		})}), "save")
		// Both are taken again at once: the screen cools down, the mouse does not.
		ds := m.decide(at(2), state_(s(4, 4)))
		want(t, ds, "give:hid/mouse")
		wantLog(t, ds, "deferring screen/LG")
		if len(m.limiter.recent[mouseKey]) != 0 || len(m.limiter.recent[lgKey]) != 1 {
			t.Fatalf("breaker counts: mouse %d, LG %d; want 0 and 1", len(m.limiter.recent[mouseKey]), len(m.limiter.recent[lgKey]))
		}
	})
}

func TestLateResults(t *testing.T) {
	t.Run("a result with no run in flight is ignored", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		ds := m.decide(at(0), Observation{Ran: ran(nil)})
		want(t, ds)
		wantLog(t, ds, "ignoring a late")
	})

	t.Run("a result after its batch ended is ignored", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, screenAssignment("LG", "mac", "linux", 3))))
		m.decide(at(1), Observation{GaveAway: okAll(lgKey)})
		ds := m.decide(at(2), Observation{GaveAway: okAll(lgKey)})
		want(t, ds)
		wantLog(t, ds, "ignoring a late")
	})

	t.Run("a command result during a batch of assignments is ignored", func(t *testing.T) {
		m := newDecider("linux", agentAt("mac", 2, nil))
		m.decide(at(0), state_(mk("mac", 2, screenAssignment("LG", "mac", "linux", 3))))
		ds := m.decide(at(1), Observation{Ran: ran(nil)})
		wantLog(t, ds, "ignoring a late")
		want(t, m.decide(at(2), Observation{GaveAway: okAll(lgKey)}), "save")
	})

	t.Run("a give-away result during a command is ignored", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.decide(at(0), state_(mk("mac", 2)))
		ds := m.decide(at(1), Observation{GaveAway: okAll(lgKey)})
		wantLog(t, ds, "ignoring a late")
		want(t, m.decide(at(2), Observation{Ran: ran(nil)}), "save")
	})
}

func TestAttachSettle(t *testing.T) {
	t.Run("a stable attach claims after the settle", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		want(t, m.decide(at(0), Observation{Attach: true}), "wake:2")
		want(t, m.decide(at(1), Observation{}), "wake:2")
		want(t, m.decide(at(2), Observation{}), "claim:linux")
		want(t, m.decide(at(3), Observation{}))
	})

	t.Run("a flapping attach restarts the settle", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.decide(at(0), Observation{Attach: true})
		want(t, m.decide(at(1), Observation{Attach: true}), "wake:3")
		want(t, m.decide(at(2), Observation{}), "wake:3")
		want(t, m.decide(at(3), Observation{}), "claim:linux")
	})

	t.Run("an attach during a deactivation settles from the attach and claims once it ends", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.decide(at(0), state_(mk("mac", 2)))
		want(t, m.decide(at(1), Observation{Attach: true}), "wake:3")
		// Settled at 3, but the job is in flight: the claim waits for it.
		want(t, m.decide(at(3), Observation{}))
		want(t, m.decide(at(5), Observation{Ran: ran(nil)}), "save", "claim:linux")
	})

	t.Run("an attach late in a deactivation still waits out its settle", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.decide(at(0), state_(mk("mac", 2)))
		m.decide(at(4), Observation{Attach: true})
		want(t, m.decide(at(5), Observation{Ran: ran(nil)}), "save", "wake:6")
		want(t, m.decide(at(6), Observation{}), "claim:linux")
	})

	t.Run("a settle that elapsed during a deactivation fires once it ends", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.decide(at(0), Observation{Attach: true})
		m.decide(at(1), state_(mk("mac", 2)))
		// The settle is due at 2 s but a deactivation runs.
		want(t, m.decide(at(2), Observation{}))
		ds := m.decide(at(3), Observation{Ran: ran(nil)})
		want(t, ds, "save", "claim:linux")
	})

	t.Run("a claim is not blocked by the cooldown or the breaker", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		m.limiter.pausedUntil = at(100)
		m.decide(at(0), Observation{Attach: true})
		want(t, m.decide(at(2), Observation{}), "claim:linux")
	})

	t.Run("a stale epoch still reconciles", func(t *testing.T) {
		m := newDecider("linux", agentAt("linux", 1, nil))
		s := mk("linux", 1)
		s.Epoch = 1
		want(t, m.decide(at(0), state_(s)))
		s2 := mk("mac", 2)
		s2.Epoch = 0 // the server's state was restored from a backup
		want(t, m.decide(at(1), state_(s2)), "run:down")
	})
}

func TestWakeAt(t *testing.T) {
	m := newDecider("linux", agentAt("mac", 2, nil))
	m.decide(at(0), Observation{Attach: true})
	m.decide(at(1), state_(mk("mac", 2, hidAssignment("mouse", "mac", "linux", 3, false), screenAssignment("LG", "mac", "linux", 3))))
	// The earliest deadline wins: the settle at 2 s, not the arrival window at
	// 11 s.
	if _, wake := m.Decide(at(1), Observation{GaveAway: okAll(mouseKey, lgKey)}); !wake.Equal(at(2)) {
		t.Fatalf("wake = %v, want %v", wake, at(2))
	}

	// With nothing pending, nothing to do and no wake.
	if ds, wake := newDecider("linux", agentAt("mac", 2, nil)).Decide(at(0), Observation{}); len(ds) != 0 || !wake.IsZero() {
		t.Fatalf("idle decider returned %+v, wake %v", ds, wake)
	}
}

func ExampleDecider_Decide() {
	m := NewDecider(testConfig("linux"), &state.AgentState{HandledOwner: "linux"})
	ds, _ := m.Decide(time.Unix(0, 0), Observation{
		State: &state.ServerState{
			Owner:      "mac",
			Activation: 2,
		},
	})
	fmt.Println(ds[0].Log)
	fmt.Println("run:", ds[1].Run[0].Argv, ds[1].Log)
	// Output:
	// deactivating: ownership went to "mac"
	// run: [down] running -D down
}
