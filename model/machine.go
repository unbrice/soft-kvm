// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// machine.go: the pure switch-decision state machine (SPEC §4.3, §11.3).

package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/unbrice/soft-kvm/state"
)

// Event is the machine's only input. Exactly one semantic field is set, plus
// Now (SPEC §11.3).
type Event struct {
	Now        time.Time
	Attach     bool               // receiver attach edge from the Detector
	State      *state.ServerState // from /state or a /wait wake
	SwitchExit *error             // result of the activation or deactivation commands (nil = all exit 0)
}

// Action is the machine's only output. Glue executes it and feeds results back
// as the next Event (SPEC §11.3).
type Action struct {
	Claim      string    // non-empty: POST /claim/<id>
	Activate   bool      // run the --on-activation commands: this host just became the owner
	Deactivate bool      // run the --on-deactivation commands: this host just stopped being the owner
	Retry      bool      // with Activate or Deactivate: a re-run within one sequence, so commands that already succeeded are skipped
	Notify     bool      // run --notify-cmd
	WakeAt     time.Time // emitted at most once per Step batch, as its own final action: the glue rearms its timer from it; a batch with no WakeAt action means no pending deadline
	SaveOwner  *string   // non-nil: persist this as last_owner (agent.json)
	Log        string    // non-empty: glue logs at Info
}

// MachineConfig is the policy knobs from the connect flags (SPEC §5.4).
type MachineConfig struct {
	ID            string
	Settle        time.Duration
	SwitchRetries int
	RetrySpacing  time.Duration
	// Cooldown, BreakerWindow, BreakerMax and BreakerOpenFor implement the
	// §4.3 circuit breaker.
	Cooldown       time.Duration
	BreakerWindow  time.Duration
	BreakerMax     int
	BreakerOpenFor time.Duration
}

// SwitchDeadline is the watchdog for modeSwitching, the only mode whose sole
// exit is an external event: a lost SwitchExit would otherwise wedge the
// machine for the process lifetime. It is a watchdog, not a tuned timeout —
// well above the slowest legitimate switch and above the glue's
// --switch-timeout default (30 s), so the child dies before the machine gives
// up.
const SwitchDeadline = 60 * time.Second

// mode is the machine's current activity. Only one activity is in progress at
// a time.
type mode int

const (
	modeIdle mode = iota
	modeSettling
	modeSwitching
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

	deferred *state.ServerState
	gateWake time.Time

	reconState        *state.ServerState
	activating        bool // the sequence in progress is an activation, not a deactivation
	latestState       *state.ServerState
	switchSentAt      time.Time
	attemptCount      int
	retryWaitDeadline time.Time

	// Outstanding-effect bookkeeping: results pair by mode alone, so under
	// concurrent glue a stale result can mispair; a result is accepted only
	// while its effect is outstanding.
	switchOutstanding bool

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
// switch retry wait, switch watchdog.
func (m *Machine) Step(e Event) []Action {
	var actions []Action

	switch {
	case e.State != nil:
		actions = append(actions, m.handleState(e.State, e.Now)...)
	case e.Attach:
		actions = append(actions, m.handleAttach(e.Now)...)
	case e.SwitchExit != nil:
		actions = append(actions, m.handleSwitchExit(*e.SwitchExit, e.Now)...)
	}

	m.processDeadlines(e.Now, &actions)

	// WakeAt is its own action, always the tail of the batch.
	if w := m.wakeAt(e.Now); !w.IsZero() {
		actions = append(actions, Action{WakeAt: w})
	}
	return actions
}

// handleState processes a State event.
func (m *Machine) handleState(s *state.ServerState, now time.Time) []Action {
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
	case modeSwitching, modeRetryWait:
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

// handleSwitchExit handles the result of one run of the switch commands. Exit
// 0 is the receipt: a switch command reports success only once the switch
// landed (SPEC §4.3), so there is nothing left to confirm here.
func (m *Machine) handleSwitchExit(err error, now time.Time) []Action {
	if !m.switchOutstanding {
		// The result arrived after its sequence ended (or was never
		// requested): a plumbing bug class worth grepping.
		return []Action{{Log: "ignoring late SwitchExit"}}
	}
	if m.mode != modeSwitching {
		return nil
	}
	m.switchOutstanding = false
	if err != nil {
		var actions []Action
		m.retryOrFail(now, &actions, fmt.Sprintf("%s attempt %d exit: %s", m.what(), m.attemptCount, err))
		return actions
	}
	return m.sequenceSuccess(now)
}

// reconcile evaluates the §4.3 gates for a state while idle or settling.
func (m *Machine) reconcile(s *state.ServerState, now time.Time) []Action {
	me := m.cfg.ID

	if s.Owner == me {
		if m.lastOwner != me {
			// Ownership came to this host: run the activation commands. Not a
			// one-way door, so neither the cooldown nor the breaker gates it,
			// and last_owner is saved once the commands are done — a restart
			// mid-run runs them again.
			m.startSequence(s, now, true)
			return []Action{{
				Activate: true,
				Log:      fmt.Sprintf("activating %q attempt 1: ownership came to me", me),
			}}
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
			Log: fmt.Sprintf("deferring switch to %q: %s", s.Owner, reason),
		}}
	}

	// A pending settle survives: attach→claim is an independent trigger
	// (SPEC §5.4) and the claim is what rescues a stale bit pointing at the
	// other host while the receiver is attached here. It fires once the
	// machine is idle again.
	m.startSequence(s, now, false)
	return []Action{{
		Deactivate: true,
		Log:        fmt.Sprintf("switching to %q attempt 1: ownership transition, winner live, gates clear", s.Owner),
	}}
}

// startSequence enters modeSwitching for the first attempt of an activation or
// deactivation sequence against s.
func (m *Machine) startSequence(s *state.ServerState, now time.Time, activating bool) {
	m.mode = modeSwitching
	m.activating = activating
	m.reconState = s
	m.latestState = s
	m.switchSentAt = now
	m.attemptCount = 1
	m.switchOutstanding = true
}

// run is the Action that starts the current sequence's next attempt.
func (m *Machine) run(log string) Action {
	return Action{Activate: m.activating, Deactivate: !m.activating, Retry: true, Log: log}
}

// what names the current sequence in logs.
func (m *Machine) what() string {
	if m.activating {
		return fmt.Sprintf("activation of %q", m.cfg.ID)
	}
	return fmt.Sprintf("switch to %q", m.reconState.Owner)
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
			m.switchOutstanding = true
			*actions = append(*actions, m.run(fmt.Sprintf("retry %s attempt %d", m.what(), m.attemptCount)))
			continue

		case m.mode == modeSwitching && m.switchOutstanding && !now.Before(m.switchSentAt.Add(SwitchDeadline)):
			// A lost SwitchExit must not wedge the machine: count the attempt
			// as failed and feed the ordinary retry/failure path.
			m.switchOutstanding = false
			reason := fmt.Sprintf("no SwitchExit within %v (lost event or hung child) after attempt %d", SwitchDeadline, m.attemptCount)
			m.retryOrFail(now, actions, reason)
			continue
		}
		break
	}
}

// retryOrFail treats the current attempt as failed: it retries while attempts
// remain (subject to retry spacing) and fails the sequence otherwise. reason
// names the failure class in the log — a watchdog-fired attempt counts toward
// the circuit breaker exactly like a non-zero exit (via sequenceFailure).
func (m *Machine) retryOrFail(now time.Time, actions *[]Action, reason string) {
	maxAttempts := 1 + m.cfg.SwitchRetries
	if m.attemptCount < maxAttempts {
		nextSwitch := m.switchSentAt.Add(m.cfg.RetrySpacing)
		if !now.Before(nextSwitch) {
			m.mode = modeSwitching
			m.switchSentAt = now
			m.attemptCount++
			m.switchOutstanding = true
			*actions = append(*actions, m.run(fmt.Sprintf("%s; retry %s attempt %d", reason, m.what(), m.attemptCount)))
			return
		}
		m.mode = modeRetryWait
		m.retryWaitDeadline = nextSwitch
		*actions = append(*actions, Action{
			Log: fmt.Sprintf("%s; waiting retry spacing until %v", reason, nextSwitch),
		})
		return
	}
	*actions = append(*actions, m.sequenceFailure(now, reason)...)
}

// sequenceSuccess completes a sequence because its commands all exited 0.
func (m *Machine) sequenceSuccess(now time.Time) []Action {
	owner := m.reconState.Owner
	if m.activating {
		return m.activationDone(now, fmt.Sprintf("activation of %q confirmed", owner))
	}
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
// a successful run. reason names the failure class in the log so a
// watchdog-fired attempt is greppable alongside ordinary non-zero exits.
func (m *Machine) sequenceFailure(now time.Time, reason string) []Action {
	owner := m.reconState.Owner
	if m.activating {
		// No notification: the user's fallback is the monitor's Input button,
		// which an activation command does not stand in for.
		return m.activationDone(now, fmt.Sprintf("activation of %q failed after %d attempts (%s)", owner, m.attemptCount, reason))
	}
	actions := []Action{{
		Notify:    true,
		SaveOwner: &owner,
		Log:       fmt.Sprintf("switch to %q failed after %d attempts (%s); notifying", owner, m.attemptCount, reason),
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

// activationDone ends an activation sequence, successful or not: last_owner
// becomes this host so the commands do not run again for the same transition.
// It leaves the cooldown and the breaker alone — they guard the one-way door,
// and a quick flip away must not wait behind a hook. last_owner is set before
// the chained reconcile of a stashed state, which must see this host as the
// previous owner to switch away from it.
func (m *Machine) activationDone(now time.Time, log string) []Action {
	owner := m.cfg.ID
	m.hasRecord = true
	m.lastOwner = owner
	m.clearSequence()
	actions := []Action{{SaveOwner: &owner, Log: log}}
	return append(actions, m.postSequence(now, owner)...)
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
	m.activating = false
	m.reconState = nil
	m.attemptCount = 0
	m.switchSentAt = time.Time{}
	m.retryWaitDeadline = time.Time{}
	m.switchOutstanding = false
}

// postSequence runs after any sequence end: reconciles a stashed newer state
// and then starts settle for a pending attach.
func (m *Machine) postSequence(now time.Time, target string) []Action {
	var actions []Action
	if m.latestState != nil && m.latestState.Owner != target {
		s := m.latestState
		m.latestState = nil
		// Ownership came back to this host while a deactivation ran. lastOwner
		// still says "me" — which a stashed state for a third host relies on to
		// switch again — so judge the flip back against the owner this host
		// just left, or reconcile reads it as "still me" and never activates.
		if s.Owner == m.cfg.ID && target != m.cfg.ID {
			m.lastOwner = target
		}
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
	candidates := []time.Time{m.settleDeadline, m.gateWake, m.retryWaitDeadline}
	if m.mode == modeSwitching && m.switchOutstanding {
		candidates = append(candidates, m.switchSentAt.Add(SwitchDeadline))
	}
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
