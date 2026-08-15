// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// machine.go: the pure switch-decision state machine (SPEC §4.3, §11.3).

package main

import (
	"fmt"
	"strings"
	"time"
)

// Event is the machine's only input. Exactly one semantic field is set, plus
// Now (SPEC §11.3).
type Event struct {
	Now        time.Time
	Attach     bool         // receiver attach edge from the Detector
	State      *ServerState // from /state or a /wait wake
	SwitchExit *error       // result of running SWITCH-CMD (nil = exit 0)
	ProbeExit  *error       // result of running --check-cmd (nil = exit 0 = my input IS active)
}

// Action is the machine's only output. Glue executes it and feeds results back
// as the next Event (SPEC §11.3).
type Action struct {
	Claim     string    // non-empty: POST /claim/<id>
	Switch    bool      // run SWITCH-CMD
	Probe     bool      // run --check-cmd
	Notify    bool      // run --notify-cmd
	WakeAt    time.Time // when the glue should deliver the next bare timer event; zero = no timer
	SaveOwner *string   // non-nil: persist this as last_owner (agent.json)
	Log       string    // non-empty: glue logs at Info
}

// MachineConfig is the policy knobs from the connect flags (SPEC §5.4).
type MachineConfig struct {
	ID            string
	Settle        time.Duration
	Confirm       time.Duration
	SwitchRetries int
	RetrySpacing  time.Duration
	// Cooldown, BreakerWindow, BreakerMax and BreakerOpenFor implement the
	// §4.3 circuit breaker.
	Cooldown       time.Duration
	BreakerWindow  time.Duration
	BreakerMax     int
	BreakerOpenFor time.Duration
}

// probeInterval paces --check-cmd runs across the --confirm window (SPEC
// §4.3: "poll", not a tight loop paced by check-cmd latency).
const probeInterval = 500 * time.Millisecond

// mode is the machine's current activity. Only one activity is in progress at
// a time.
type mode int

const (
	modeIdle mode = iota
	modeSettling
	modeReconcile
	modeSwitching
	modeConfirming
	modeRetryWait
)

// Machine is the switch-decision state machine: no I/O, no goroutines, no
// clock. All time arrives via Event.Now (SPEC §11.3).
type Machine struct {
	cfg       MachineConfig
	lastOwner string
	hasRecord bool

	mode           mode
	settleDeadline time.Time
	pendingAttach  bool

	deferred *ServerState
	gateWake time.Time

	reconState         *ServerState
	latestState        *ServerState
	switchSentAt       time.Time
	attemptCount       int
	confirmDeadline    time.Time
	nextProbeAt        time.Time
	allProbesSucceeded bool
	retryWaitDeadline  time.Time

	cooldownUntil    time.Time
	breakerOpenUntil time.Time
	completed        []time.Time
}

// NewMachine creates a machine. hasRecord=false means no agent.json existed:
// the first State event adopts the server's owner and never switches (SPEC §4.3).
func NewMachine(cfg MachineConfig, lastOwner string, hasRecord bool) *Machine {
	return &Machine{
		cfg:       cfg,
		lastOwner: lastOwner,
		hasRecord: hasRecord,
	}
}

// Step processes one event and returns the actions to execute. The fixed
// deadline-processing order is: attach settle, deferred reconcile gate,
// switch retry wait, confirm deadline.
func (m *Machine) Step(e Event) []Action {
	var actions []Action

	switch {
	case e.State != nil:
		actions = append(actions, m.handleState(e.State, e.Now)...)
	case e.Attach:
		actions = append(actions, m.handleAttach(e.Now)...)
	case e.SwitchExit != nil:
		actions = append(actions, m.handleSwitchExit(*e.SwitchExit, e.Now)...)
	case e.ProbeExit != nil:
		actions = append(actions, m.handleProbeExit(*e.ProbeExit, e.Now)...)
	}

	m.processDeadlines(e.Now, &actions)

	if w := m.wakeAt(e.Now); !w.IsZero() {
		if len(actions) == 0 {
			actions = append(actions, Action{WakeAt: w})
		} else {
			actions[len(actions)-1].WakeAt = w
		}
	}
	return actions
}

// handleState processes a State event.
func (m *Machine) handleState(s *ServerState, now time.Time) []Action {
	if !m.hasRecord {
		owner := s.Owner
		m.lastOwner = owner
		m.hasRecord = true
		return []Action{{
			SaveOwner: &owner,
			Log:       fmt.Sprintf("adopted owner %q from server; no switch on first reconcile", owner),
		}}
	}

	switch m.mode {
	case modeReconcile, modeSwitching, modeConfirming, modeRetryWait:
		m.latestState = s
		return nil
	default:
		return m.reconcile(s, now)
	}
}

// handleAttach processes a receiver attach edge.
func (m *Machine) handleAttach(now time.Time) []Action {
	switch m.mode {
	case modeSettling:
		m.settleDeadline = now.Add(m.cfg.Settle)
	case modeIdle:
		m.mode = modeSettling
		m.settleDeadline = now.Add(m.cfg.Settle)
	default:
		m.pendingAttach = true
	}
	return nil
}

// handleSwitchExit handles the result of a SWITCH-CMD run.
func (m *Machine) handleSwitchExit(err error, now time.Time) []Action {
	if m.mode != modeSwitching {
		return nil
	}
	status := "success"
	if err != nil {
		status = err.Error()
	}
	m.mode = modeConfirming
	m.confirmDeadline = now.Add(m.cfg.Confirm)
	m.nextProbeAt = time.Time{}
	m.allProbesSucceeded = true
	return []Action{{
		Probe:  true,
		Log:    fmt.Sprintf("switch to %q attempt %d exit: %s", m.reconState.Owner, m.attemptCount, status),
		WakeAt: m.confirmDeadline,
	}}
}

// handleProbeExit handles the result of a --check-cmd run.
func (m *Machine) handleProbeExit(err error, now time.Time) []Action {
	switch m.mode {
	case modeReconcile:
		if err != nil {
			return m.vetoFail(now)
		}
		m.mode = modeSwitching
		m.switchSentAt = now
		m.attemptCount = 1
		return []Action{{
			Switch: true,
			Log:    fmt.Sprintf("veto passed; switching to %q attempt 1", m.reconState.Owner),
		}}

	case modeConfirming:
		if err != nil {
			return m.sequenceSuccess(now)
		}
		if now.Before(m.confirmDeadline) {
			// Poll, don't spin: the next probe fires on the timer
			// (processDeadlines), never back-to-back (SPEC §4.3).
			m.nextProbeAt = now.Add(probeInterval)
			return nil
		}
		m.confirmDeadline = time.Time{}
		var actions []Action
		m.confirmElapsed(now, &actions)
		return actions

	default:
		return nil
	}
}

// reconcile evaluates the §4.3 gates for a state while idle or settling.
func (m *Machine) reconcile(s *ServerState, now time.Time) []Action {
	me := m.cfg.ID

	if s.Owner == me {
		if m.lastOwner != me {
			owner := me
			m.lastOwner = me
			return []Action{{SaveOwner: &owner, Log: fmt.Sprintf("resynced last_owner to me (%s)", me)}}
		}
		return []Action{{Log: fmt.Sprintf("no switch: owner is still me (%s)", me)}}
	}
	if m.lastOwner != me {
		return []Action{{Log: fmt.Sprintf("no switch: last_owner=%q is not me (%s)", m.lastOwner, me)}}
	}
	if s.Owner == "" {
		return []Action{{Log: "no switch: owner is empty"}}
	}
	if !s.Live[s.Owner] {
		return []Action{{Log: fmt.Sprintf("no switch: winner %q is not live", s.Owner)}}
	}
	if blocked, until, reason := m.gated(now); blocked {
		m.deferred = s
		m.gateWake = until
		return []Action{{
			Log:    fmt.Sprintf("deferring switch to %q: %s", s.Owner, reason),
			WakeAt: until,
		}}
	}

	m.mode = modeReconcile
	m.reconState = s
	m.latestState = s
	// A pending settle survives: attach→claim is an independent trigger
	// (SPEC §5.4) and the claim is what rescues a stale bit pointing at the
	// other host while the receiver is attached here. It fires once the
	// machine is idle again.
	return []Action{{
		Probe: true,
		Log:   fmt.Sprintf("probing before switch to %q: ownership transition, winner live, gates clear", s.Owner),
	}}
}

// gated reports whether the cooldown or circuit breaker forbids a new switch
// sequence right now.
func (m *Machine) gated(now time.Time) (bool, time.Time, string) {
	var until time.Time
	var parts []string
	if !m.cooldownUntil.IsZero() && now.Before(m.cooldownUntil) {
		until = m.cooldownUntil
		parts = append(parts, fmt.Sprintf("cooldown until %v", m.cooldownUntil))
	}
	if !m.breakerOpenUntil.IsZero() && now.Before(m.breakerOpenUntil) {
		if until.IsZero() || m.breakerOpenUntil.After(until) {
			until = m.breakerOpenUntil
		}
		parts = append(parts, fmt.Sprintf("breaker open until %v", m.breakerOpenUntil))
	}
	if len(parts) == 0 {
		return false, time.Time{}, ""
	}
	return true, until, strings.Join(parts, ", ")
}

// processDeadlines handles any deadlines that are due at now. It loops because
// handling one deadline can make another one due immediately.
func (m *Machine) processDeadlines(now time.Time, actions *[]Action) {
	for {
		switch {
		case !m.settleDeadline.IsZero() && !m.settleDeadline.After(now) &&
			(m.mode == modeIdle || m.mode == modeSettling):
			// A settle that was interrupted by a reconcile still fires once
			// the machine is idle again (SPEC §5.4: attach→claim is an
			// independent trigger).
			m.mode = modeIdle
			m.settleDeadline = time.Time{}
			*actions = append(*actions, Action{Claim: m.cfg.ID, Log: fmt.Sprintf("claim %q", m.cfg.ID)})
			continue

		case m.mode == modeIdle && m.deferred != nil && !m.gateWake.After(now):
			state := m.deferred
			m.deferred = nil
			m.gateWake = time.Time{}
			*actions = append(*actions, m.reconcile(state, now)...)
			continue

		case m.mode == modeRetryWait && !m.retryWaitDeadline.IsZero() && !m.retryWaitDeadline.After(now):
			m.mode = modeSwitching
			m.switchSentAt = now
			m.attemptCount++
			m.retryWaitDeadline = time.Time{}
			*actions = append(*actions, Action{
				Switch: true,
				Log:    fmt.Sprintf("retry switch to %q attempt %d", m.reconState.Owner, m.attemptCount),
			})
			continue

		case m.mode == modeConfirming && !m.nextProbeAt.IsZero() && !m.nextProbeAt.After(now) &&
			m.confirmDeadline.After(now):
			m.nextProbeAt = time.Time{}
			*actions = append(*actions, Action{Probe: true})
			continue

		case m.mode == modeConfirming && !m.confirmDeadline.IsZero() && !m.confirmDeadline.After(now) && m.allProbesSucceeded:
			m.confirmDeadline = time.Time{}
			m.confirmElapsed(now, actions)
			continue
		}
		break
	}
}

// confirmElapsed is reached when the confirm window closes with every probe
// still succeeding. It either retries the switch or gives up.
func (m *Machine) confirmElapsed(now time.Time, actions *[]Action) {
	maxAttempts := 1 + m.cfg.SwitchRetries
	if m.attemptCount < maxAttempts {
		nextSwitch := m.switchSentAt.Add(m.cfg.RetrySpacing)
		if !now.Before(nextSwitch) {
			m.mode = modeSwitching
			m.switchSentAt = now
			m.attemptCount++
			*actions = append(*actions, Action{
				Switch: true,
				Log:    fmt.Sprintf("confirm elapsed; retry switch to %q attempt %d", m.reconState.Owner, m.attemptCount),
			})
			return
		}
		m.mode = modeRetryWait
		m.retryWaitDeadline = nextSwitch
		*actions = append(*actions, Action{
			Log:    fmt.Sprintf("confirm elapsed; waiting retry spacing until %v", nextSwitch),
			WakeAt: nextSwitch,
		})
		return
	}
	*actions = append(*actions, m.sequenceFailure(now)...)
}

// sequenceSuccess completes a switch sequence because a confirm probe failed
// (the falling edge receipt).
func (m *Machine) sequenceSuccess(now time.Time) []Action {
	owner := m.reconState.Owner
	actions := []Action{{SaveOwner: &owner, Log: fmt.Sprintf("switch to %q confirmed", owner)}}
	m.hasRecord = true
	m.cooldownUntil = now.Add(m.cfg.Cooldown)
	m.recordBreaker(now)
	m.clearSequence()
	actions = append(actions, m.postSequence(now, owner)...)
	// A chained reconcile of a stashed newer state decides lastOwner when it
	// completes; in every other ending (idle, deferred, settle) memory must
	// match the persisted SaveOwner now.
	if m.mode == modeIdle || m.mode == modeSettling {
		m.lastOwner = owner
	}
	return actions
}

// sequenceFailure completes a switch sequence after the last retry without
// confirmation.
func (m *Machine) sequenceFailure(now time.Time) []Action {
	owner := m.reconState.Owner
	actions := []Action{{
		Notify:    true,
		SaveOwner: &owner,
		Log:       fmt.Sprintf("switch to %q failed after %d attempts; notifying", owner, m.attemptCount),
	}}
	m.hasRecord = true
	m.cooldownUntil = now.Add(m.cfg.Cooldown)
	m.recordBreaker(now)
	m.clearSequence()
	actions = append(actions, m.postSequence(now, owner)...)
	if m.mode == modeIdle || m.mode == modeSettling {
		m.lastOwner = owner
	}
	return actions
}

// vetoFail aborts a reconcile before the switch because the probe says the
// input is already inactive.
func (m *Machine) vetoFail(now time.Time) []Action {
	owner := m.reconState.Owner
	actions := []Action{{
		SaveOwner: &owner,
		Log:       fmt.Sprintf("probe veto: input already inactive; resync last_owner to %q", owner),
	}}
	m.hasRecord = true
	m.clearSequence()
	actions = append(actions, m.postSequence(now, owner)...)
	if m.mode == modeIdle || m.mode == modeSettling {
		m.lastOwner = owner
	}
	return actions
}

// recordBreaker counts a completed switch sequence for the circuit breaker.
func (m *Machine) recordBreaker(now time.Time) {
	m.completed = append(m.completed, now)
	cutoff := now.Add(-m.cfg.BreakerWindow)
	i := 0
	for i < len(m.completed) && !m.completed[i].After(cutoff) {
		i++
	}
	m.completed = m.completed[i:]
	if len(m.completed) >= m.cfg.BreakerMax {
		m.breakerOpenUntil = now.Add(m.cfg.BreakerOpenFor)
	}
}

// clearSequence resets the in-progress switch fields.
func (m *Machine) clearSequence() {
	m.mode = modeIdle
	m.reconState = nil
	m.attemptCount = 0
	m.switchSentAt = time.Time{}
	m.confirmDeadline = time.Time{}
	m.nextProbeAt = time.Time{}
	m.retryWaitDeadline = time.Time{}
	m.allProbesSucceeded = false
}

// postSequence runs after any sequence end: reconciles a stashed newer state
// and then starts settle for a pending attach.
func (m *Machine) postSequence(now time.Time, target string) []Action {
	var actions []Action
	if m.latestState != nil && m.latestState.Owner != target {
		s := m.latestState
		m.latestState = nil
		actions = append(actions, m.reconcile(s, now)...)
	} else {
		m.latestState = nil
	}
	if m.mode == modeIdle && m.pendingAttach {
		m.pendingAttach = false
		m.mode = modeSettling
		m.settleDeadline = now.Add(m.cfg.Settle)
	}
	return actions
}

// wakeAt returns the earliest future deadline the machine is tracking.
func (m *Machine) wakeAt(now time.Time) time.Time {
	var wake time.Time
	candidates := []time.Time{m.settleDeadline, m.gateWake, m.confirmDeadline, m.nextProbeAt, m.retryWaitDeadline}
	for _, t := range candidates {
		if t.IsZero() || !t.After(now) {
			continue
		}
		if wake.IsZero() || t.Before(wake) {
			wake = t
		}
	}
	return wake
}
