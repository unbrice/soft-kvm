// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// machine.go: the pure decision state machine (SPEC §4.3, §11.3).

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
	Now    time.Time
	Attach bool               // receiver attach edge from the Detector
	State  *state.ServerState // from /state or a /wait wake
	Exit   *Exit              // result of the run in flight
}

// Exit is the result of the one run in flight: the -A or -D commands (Err), or
// a batch of takes (Takes, one outcome per hold).
type Exit struct {
	Err   error                     // -A or -D commands: nil when all exit 0
	Takes map[state.HoldKey]Outcome // a batch of takes; non-nil, whatever its size
}

// Outcome is what one take of a run did on this host.
type Outcome struct {
	NotHere bool  // nothing to move on this host
	Err     error // the write failed
}

// Action is the machine's only output. Glue executes it and feeds results back
// as the next Event (SPEC §11.3).
type Action struct {
	Claim      string            // non-empty: POST /claim/<id>
	Activate   bool              // run the --on-activation commands: this host just became the owner
	Activation int64             // with Activate: the activation its takes carry
	Deactivate bool              // run the --on-deactivation commands: this host just stopped being the owner
	Run        []state.Hold      // run these takes
	Retry      bool              // with Activate or Deactivate: re-run only the commands that failed
	Notify     bool              // run --notify-cmd
	WakeAt     time.Time         // emitted at most once per Step batch, as its own final action: the glue rearms its timer from it; a batch with no WakeAt action means no pending deadline
	Save       *state.AgentState // non-nil: persist this as agent.json
	Log        string            // non-empty: glue logs at Info
}

// MachineConfig is the policy knobs from the connect flags (SPEC §5.4).
type MachineConfig struct {
	ID            string
	Settle        time.Duration
	SwitchRetries int
	RetrySpacing  time.Duration
	// ArrivalWindow is how long a host that sent a device waits for the
	// holder to report it arrived (SPEC §4.3).
	ArrivalWindow time.Duration
	// Cooldown, BreakerWindow, BreakerMax and BreakerOpenFor implement the
	// §4.3 circuit breaker, counted per hold name.
	Cooldown       time.Duration
	BreakerWindow  time.Duration
	BreakerMax     int
	BreakerOpenFor time.Duration
}

// SwitchDeadline is the watchdog for an outstanding run, the only activity
// whose sole exit is an external event: a lost Exit would otherwise wedge the
// machine for the process lifetime. It is a watchdog, not a tuned timeout —
// well above the slowest legitimate run and above the glue's --switch-timeout
// default (30 s), so the child dies before the machine gives up.
const SwitchDeadline = 60 * time.Second

// status is where one take of a batch stands.
type status int

const (
	running    status = iota // in the run whose Exit is outstanding
	awaiting                 // a device sent: waiting for the holder to report it arrived
	failed                   // a write failed, or the device did not arrive in time
	done                     // moved, arrived, or nothing to move here
	superseded               // a newer take of the same name replaced it
)

// item is one take of a batch.
type item struct {
	hold     state.Hold
	status   status
	attempts int
	reason   string // why it last failed
	// attempted is set once a run reported anything but NotHere, or never
	// reported at all: only an attempted take counts for the cooldown and the
	// breaker.
	attempted bool
	deadline  time.Time // awaiting: when the arrival window closes
}

// takeBatch is the takes this host executes together: the holds that were
// pending at once, each tracked until it is done, superseded, or out of
// attempts.
type takeBatch struct {
	items       map[state.HoldKey]*item
	order       []state.HoldKey // the order /state listed them
	outstanding bool            // an Exit is awaited
	sentAt      time.Time       // when the latest run started
	retryAt     time.Time       // when failed takes may re-run; zero when none waits
}

// commandRun is an activation or a deactivation: this host's commands run in
// order and only those that failed re-run.
type commandRun struct {
	activating bool
	owner      string // the owner this run follows; this host when activating
	activation int64  // the activation an activation runs under
	attempt    int
	sentAt     time.Time
	retryAt    time.Time // waiting for the retry spacing; zero when running
	running    bool      // an Exit is awaited
}

// Machine is the decision state machine: no I/O, no goroutines, no clock. All
// time arrives via Event.Now (SPEC §11.3).
type Machine struct {
	cfg MachineConfig

	// The record agent.json persists (SPEC §4.3).
	hasRecord      bool
	lastOwner      string
	lastActivation int64
	seen           map[state.HoldKey]state.Seq

	// At most one of these runs at a time.
	cmd   *commandRun
	takes *takeBatch

	// latest is the newest state seen: a state that arrives during a command
	// run or a take batch is reconciled when it ends, and a deferred take is
	// reconciled from it when its gate opens.
	latest   *state.ServerState
	gateWake time.Time

	settleDeadline time.Time
	pendingAttach  bool

	cooldownUntil    map[state.HoldKey]time.Time
	breakerOpenUntil time.Time
	completed        map[state.HoldKey][]time.Time
}

// NewMachine creates a machine from the agent's persisted record. hasRecord
// false means no agent.json existed: the first State event adopts the server's
// owner, activation and holds and never switches (SPEC §4.3).
func NewMachine(cfg MachineConfig, rec state.AgentState, hasRecord bool) *Machine {
	m := &Machine{
		cfg:            cfg,
		hasRecord:      hasRecord,
		lastOwner:      rec.LastOwner,
		lastActivation: rec.LastActivation,
		seen:           make(map[state.HoldKey]state.Seq, len(rec.Seen)),
		cooldownUntil:  make(map[state.HoldKey]time.Time),
		completed:      make(map[state.HoldKey][]time.Time),
	}
	for k, seq := range rec.Seen {
		if key, ok := state.ParseHoldKey(k); ok {
			m.seen[key] = seq
		}
	}
	return m
}

// record is the state agent.json persists.
func (m *Machine) record() *state.AgentState {
	rec := &state.AgentState{
		LastOwner:      m.lastOwner,
		LastActivation: m.lastActivation,
		Seen:           make(map[string]state.Seq, len(m.seen)),
	}
	for k, seq := range m.seen {
		rec.Seen[k.String()] = seq
	}
	return rec
}

// Step processes one event and returns the actions to execute. After the
// event, deadlines that are due run in a fixed order: the command run or take
// batch in flight, the attach settle, the deferred-take gate.
func (m *Machine) Step(e Event) []Action {
	var actions []Action

	switch {
	case e.State != nil:
		actions = append(actions, m.handleState(e.State, e.Now)...)
	case e.Attach:
		m.handleAttach(e.Now)
	case e.Exit != nil:
		actions = append(actions, m.handleExit(e.Exit, e.Now)...)
	}

	m.processDeadlines(e.Now, &actions)

	// WakeAt is its own action, always the tail of the batch.
	if w := m.wakeAt(e.Now); !w.IsZero() {
		actions = append(actions, Action{WakeAt: w})
	}
	return actions
}

// busy reports whether a command run or a take batch is in progress.
func (m *Machine) busy() bool { return m.cmd != nil || m.takes != nil }

// handleState processes a State event.
func (m *Machine) handleState(s *state.ServerState, now time.Time) []Action {
	if !m.hasRecord {
		m.hasRecord = true
		m.lastOwner = s.Owner
		m.lastActivation = s.Activation
		for _, h := range s.Holds {
			m.seen[h.Key()] = h.Seq
		}
		return []Action{{
			Save: m.record(),
			Log: fmt.Sprintf("adopted owner %q, activation %d and %d holds from server; nothing runs on first reconcile",
				s.Owner, s.Activation, len(s.Holds)),
		}}
	}
	m.latest = s
	if m.busy() {
		m.inspectTakes(s)
		return nil
	}
	return m.reconcile(s, now)
}

// handleAttach processes a receiver attach edge.
func (m *Machine) handleAttach(now time.Time) {
	if m.busy() {
		m.pendingAttach = true
		return
	}
	m.settleDeadline = now.Add(m.cfg.Settle)
}

// handleExit handles the result of the run in flight. A result is accepted
// only while its run is outstanding (SPEC §4.3).
func (m *Machine) handleExit(x *Exit, now time.Time) []Action {
	switch {
	case m.cmd != nil && m.cmd.running && x.Takes == nil:
		m.cmd.running = false
		if x.Err != nil {
			return m.retryOrFail(now, fmt.Sprintf("%s attempt %d exit: %s", m.what(), m.cmd.attempt, x.Err))
		}
		return m.finishCmd(now, fmt.Sprintf("%s confirmed", m.what()))
	case m.takes != nil && m.takes.outstanding && x.Takes != nil:
		m.takes.outstanding = false
		m.takeOutcomes(x, now)
		return nil
	}
	return []Action{{Log: "ignoring late or mismatched Exit"}}
}

// reconcile decides what a state calls for while idle, in priority order: the
// activation edge, the deactivation edge, then pending takes (SPEC §4.3).
func (m *Machine) reconcile(s *state.ServerState, now time.Time) []Action {
	me := m.cfg.ID
	m.gateWake = time.Time{}

	switch {
	case s.Owner == me && (m.lastOwner != me || s.Activation != m.lastActivation):
		// Ownership came to this host, or was re-activated: run the activation
		// commands. Not a one-way door, so neither the cooldown nor the breaker
		// gates it, and the record is saved once the commands are done — a
		// restart mid-run runs them again.
		m.cmd = &commandRun{activating: true, owner: me, activation: s.Activation, attempt: 1, sentAt: now, running: true}
		return []Action{{
			Activate:   true,
			Activation: s.Activation,
			Log:        fmt.Sprintf("activating %q attempt 1: ownership came to me (activation %d)", me, s.Activation),
		}}
	case s.Owner != me && s.Owner != "" && m.lastOwner == me:
		m.cmd = &commandRun{owner: s.Owner, attempt: 1, sentAt: now, running: true}
		return []Action{{
			Deactivate: true,
			Log:        fmt.Sprintf("deactivating: ownership went to %q", s.Owner),
		}}
	}
	return m.reconcileTakes(s, now)
}

// reconcileTakes starts a batch of every pending take that passes the gates.
func (m *Machine) reconcileTakes(s *state.ServerState, now time.Time) []Action {
	var actions []Action
	var run []state.Hold
	changed := false
	for _, h := range s.Holds {
		key := h.Key()
		switch {
		case h.Holder == m.cfg.ID:
			continue
		case m.hasSeen(key, h.Seq):
			continue
		case h.Arrived:
			m.seen[key] = h.Seq
			changed = true
			actions = append(actions, Action{Log: fmt.Sprintf("no take of %s: already arrived on %q", key, h.Holder)})
			continue
		case !s.Live[h.Holder]:
			actions = append(actions, Action{Log: fmt.Sprintf("no take of %s: holder %q is not live; it stays pending", key, h.Holder)})
			continue
		}
		if until, reason := m.gated(key, now); !until.IsZero() {
			if m.gateWake.IsZero() || until.Before(m.gateWake) {
				m.gateWake = until
			}
			actions = append(actions, Action{Log: fmt.Sprintf("deferring take of %s: %s", key, reason)})
			continue
		}
		run = append(run, h)
	}
	if changed {
		actions = append(actions, Action{Save: m.record()})
	}
	if len(run) == 0 {
		return actions
	}

	t := &takeBatch{items: make(map[state.HoldKey]*item, len(run)), outstanding: true, sentAt: now}
	names := make([]string, len(run))
	for i, h := range run {
		t.items[h.Key()] = &item{hold: h, status: running, attempts: 1}
		t.order = append(t.order, h.Key())
		names[i] = h.Key().String()
	}
	m.takes = t
	return append(actions, Action{
		Run: run,
		Log: fmt.Sprintf("taking %s attempt 1: holders live, gates clear", strings.Join(names, ", ")),
	})
}

func (m *Machine) hasSeen(key state.HoldKey, seq state.Seq) bool {
	s, ok := m.seen[key]
	return ok && s == seq
}

// gated reports until when the cooldown of key or the circuit breaker forbids
// a take, and why; the zero time means neither does.
func (m *Machine) gated(key state.HoldKey, now time.Time) (time.Time, string) {
	var until time.Time
	var parts []string
	if t := m.cooldownUntil[key]; now.Before(t) {
		until = t
		parts = append(parts, fmt.Sprintf("cooldown until %v", t))
	}
	if now.Before(m.breakerOpenUntil) {
		if m.breakerOpenUntil.After(until) {
			until = m.breakerOpenUntil
		}
		parts = append(parts, fmt.Sprintf("breaker open until %v", m.breakerOpenUntil))
	}
	return until, strings.Join(parts, ", ")
}

// what names the command run in logs.
func (m *Machine) what() string {
	if m.cmd.activating {
		return fmt.Sprintf("activation of %q", m.cfg.ID)
	}
	return fmt.Sprintf("deactivation for %q", m.cmd.owner)
}

// runCmd is the Action that starts the command run's next attempt.
func (m *Machine) runCmd(log string) Action {
	c := m.cmd
	a := Action{Activate: c.activating, Deactivate: !c.activating, Retry: true, Log: log}
	if c.activating {
		a.Activation = c.activation
	}
	return a
}

// retryOrFail treats the current command attempt as failed: it retries while
// attempts remain (subject to retry spacing) and ends the run otherwise.
// reason names the failure class in the log.
func (m *Machine) retryOrFail(now time.Time, reason string) []Action {
	c := m.cmd
	if c.attempt < 1+m.cfg.SwitchRetries {
		next := c.sentAt.Add(m.cfg.RetrySpacing)
		if !now.Before(next) {
			c.attempt++
			c.sentAt = now
			c.running = true
			return []Action{m.runCmd(fmt.Sprintf("%s; retry %s attempt %d", reason, m.what(), c.attempt))}
		}
		c.retryAt = next
		return []Action{{Log: fmt.Sprintf("%s; waiting retry spacing until %v", reason, next)}}
	}
	return m.finishCmd(now, fmt.Sprintf("%s failed after %d attempts (%s)", m.what(), c.attempt, reason))
}

// finishCmd ends an activation or a deactivation, successful or not: the
// record moves on so the commands do not run again for the same transition. A
// failure is only logged — nothing in a command is a one-way door, and the
// user's fallback is the monitor's Input button. The stashed state is then
// reconciled against the new record, so a flip away during an activation
// deactivates right after it.
func (m *Machine) finishCmd(now time.Time, log string) []Action {
	c := m.cmd
	m.cmd = nil
	m.lastOwner = c.owner
	if c.activating {
		m.lastActivation = c.activation
	}
	actions := []Action{{Save: m.record(), Log: log}}
	return append(actions, m.afterWork(now)...)
}

// afterWork runs when a command run or a take batch ends: it reconciles the
// latest state and starts the settle for an attach that arrived meanwhile.
func (m *Machine) afterWork(now time.Time) []Action {
	var actions []Action
	if m.latest != nil {
		actions = m.reconcile(m.latest, now)
	}
	if !m.busy() && m.pendingAttach {
		m.pendingAttach = false
		m.settleDeadline = now.Add(m.cfg.Settle)
	}
	return actions
}

// takeOutcomes records the Exit of a run of takes. A take that reports neither
// an outcome nor a result counts as failed: its run never reported it.
func (m *Machine) takeOutcomes(x *Exit, now time.Time) {
	t := m.takes
	for _, key := range t.order {
		it := t.items[key]
		if it.status != running {
			continue
		}
		out, ok := x.Takes[key]
		it.attempted = it.attempted || !ok || !out.NotHere
		switch {
		case !ok:
			it.status, it.reason = failed, "no result for it"
		case out.NotHere:
			it.status = done
		case out.Err != nil:
			it.status, it.reason = failed, out.Err.Error()
		case key.Kind == state.KindScreen:
			// A screen has no receipt: the write was made, and that is all.
			it.status = done
		case m.arrived(it):
			it.status = done
		default:
			it.status, it.deadline = awaiting, now.Add(m.cfg.ArrivalWindow)
		}
	}
}

// arrived reports whether the latest state already shows the device of it
// arrived on its holder.
func (m *Machine) arrived(it *item) bool {
	if m.latest == nil {
		return false
	}
	for _, h := range m.latest.Holds {
		if h.Key() == it.hold.Key() {
			return h.Seq == it.hold.Seq && h.Arrived
		}
	}
	return false
}

// inspectTakes updates the takes of the batch in flight from a newer state: a
// hold that is gone or has a new seq was superseded and is dropped — the new
// take is pending on its own, not part of this run — and a device whose hold
// reads arrived is done.
func (m *Machine) inspectTakes(s *state.ServerState) {
	t := m.takes
	if t == nil {
		return
	}
	holds := make(map[state.HoldKey]state.Hold, len(s.Holds))
	for _, h := range s.Holds {
		holds[h.Key()] = h
	}
	for _, key := range t.order {
		it := t.items[key]
		if it.status == done || it.status == superseded {
			continue
		}
		h, ok := holds[key]
		switch {
		case !ok || h.Seq != it.hold.Seq:
			it.status = superseded
		case h.Arrived && key.Kind == state.KindHID && it.status != running:
			it.status = done
		}
	}
}

// startRetry re-runs the failed takes that have attempts left, and reports
// whether the run started or is waiting for the retry spacing.
func (m *Machine) startRetry(now time.Time, actions *[]Action) {
	t := m.takes
	var retry []state.Hold
	for _, key := range t.order {
		it := t.items[key]
		if it.status == failed && it.attempts < 1+m.cfg.SwitchRetries {
			retry = append(retry, it.hold)
		}
	}
	t.retryAt = time.Time{}
	if len(retry) == 0 {
		return
	}
	next := t.sentAt.Add(m.cfg.RetrySpacing)
	if now.Before(next) {
		t.retryAt = next
		return
	}
	names := make([]string, len(retry))
	for i, h := range retry {
		it := t.items[h.Key()]
		it.status = running
		it.attempts++
		names[i] = fmt.Sprintf("%s attempt %d after %s", h.Key(), it.attempts, it.reason)
	}
	t.outstanding, t.sentAt = true, now
	*actions = append(*actions, Action{Run: retry, Log: "retrying take of " + strings.Join(names, ", ")})
}

// advanceTakes applies the deadlines of the take batch: the watchdog on a
// lost Exit, the end of the arrival window, the retry of failed takes and the
// end of the batch. It reports whether anything changed.
func (m *Machine) advanceTakes(now time.Time, actions *[]Action) bool {
	t := m.takes
	progressed := false

	if t.outstanding && !now.Before(t.sentAt.Add(SwitchDeadline)) {
		// A lost Exit must not wedge the machine: count the attempt as failed
		// and feed the ordinary retry/failure path.
		t.outstanding = false
		for _, key := range t.order {
			if it := t.items[key]; it.status == running {
				it.attempted = true
				it.status = failed
				it.reason = fmt.Sprintf("no Exit within %v (lost event or hung child)", SwitchDeadline)
			}
		}
		progressed = true
	}
	for _, key := range t.order {
		if it := t.items[key]; it.status == awaiting && !now.Before(it.deadline) {
			it.status = failed
			it.reason = fmt.Sprintf("did not arrive within %v", m.cfg.ArrivalWindow)
			progressed = true
		}
	}

	before := t.outstanding
	if !t.outstanding {
		m.startRetry(now, actions)
	}
	if t.outstanding != before {
		return true
	}

	if t.outstanding || !t.retryAt.IsZero() {
		return progressed
	}
	for _, key := range t.order {
		if t.items[key].status == awaiting {
			return progressed
		}
	}
	*actions = append(*actions, m.finishTakes(now)...)
	return true
}

// finishTakes ends a take batch: every take is handled at the seq that ran,
// whatever happened, so a failure is final until a newer take arrives. The
// cooldown and the breaker count the names this host attempted to move: a
// take that was only ever nothing to move here is not a write, and the breaker
// exists to stop write loops. What still failed is
// notified when this host is the previous holder — the one the user is
// looking at — and only logged otherwise (SPEC §4.3).
func (m *Machine) finishTakes(now time.Time) []Action {
	t := m.takes
	m.takes = nil

	var actions []Action
	notify := false
	for _, key := range t.order {
		it := t.items[key]
		m.seen[key] = it.hold.Seq
		if it.attempted {
			m.cooldownUntil[key] = now.Add(m.cfg.Cooldown)
			m.recordBreaker(key, now)
		}
		switch it.status {
		case failed:
			who := "not notifying: I am not the previous holder"
			if it.hold.Prev == m.cfg.ID {
				notify = true
				who = "notifying: I am the previous holder"
			}
			actions = append(actions, Action{Log: fmt.Sprintf("take of %s failed after %d attempts (%s); %s", key, it.attempts, it.reason, who)})
		case superseded:
			actions = append(actions, Action{Log: fmt.Sprintf("take of %s dropped: a newer take replaced it", key)})
		}
	}
	if notify {
		actions = append(actions, Action{Notify: true})
	}
	actions = append(actions, Action{Save: m.record(), Log: "takes done"})
	return append(actions, m.afterWork(now)...)
}

// recordBreaker counts a take of key for the circuit breaker: BreakerMax of
// one name within the window open it for every name.
func (m *Machine) recordBreaker(key state.HoldKey, now time.Time) {
	cutoff := now.Add(-m.cfg.BreakerWindow)
	var kept []time.Time
	for _, at := range m.completed[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	m.completed[key] = kept
	if len(kept) >= m.cfg.BreakerMax {
		m.breakerOpenUntil = now.Add(m.cfg.BreakerOpenFor)
	}
}

// processDeadlines handles any deadlines that are due at now. It loops because
// handling one deadline can make another one due immediately.
func (m *Machine) processDeadlines(now time.Time, actions *[]Action) {
	for {
		switch {
		case m.cmd != nil:
			if !m.advanceCmd(now, actions) {
				return
			}
		case m.takes != nil:
			if !m.advanceTakes(now, actions) {
				return
			}
		case !m.settleDeadline.IsZero() && !m.settleDeadline.After(now):
			m.settleDeadline = time.Time{}
			*actions = append(*actions, Action{Claim: m.cfg.ID, Log: fmt.Sprintf("claim %q", m.cfg.ID)})
		case m.latest != nil && !m.gateWake.IsZero() && !m.gateWake.After(now):
			*actions = append(*actions, m.reconcile(m.latest, now)...)
		default:
			return
		}
	}
}

// advanceCmd applies the deadlines of the command run: the retry spacing
// and the watchdog on a lost Exit. It reports whether anything changed.
func (m *Machine) advanceCmd(now time.Time, actions *[]Action) bool {
	c := m.cmd
	switch {
	case !c.running && !c.retryAt.IsZero() && !now.Before(c.retryAt):
		c.running = true
		c.sentAt = now
		c.attempt++
		c.retryAt = time.Time{}
		*actions = append(*actions, m.runCmd(fmt.Sprintf("retry %s attempt %d", m.what(), c.attempt)))
		return true
	case c.running && !now.Before(c.sentAt.Add(SwitchDeadline)):
		// A lost Exit must not wedge the machine: count the attempt as failed
		// and feed the ordinary retry/failure path.
		c.running = false
		reason := fmt.Sprintf("no Exit within %v (lost event or hung child) after attempt %d", SwitchDeadline, c.attempt)
		*actions = append(*actions, m.retryOrFail(now, reason)...)
		return true
	}
	return false
}

// wakeAt returns the earliest future deadline the machine is tracking.
func (m *Machine) wakeAt(now time.Time) time.Time {
	candidates := []time.Time{m.settleDeadline}
	if m.cmd == nil && m.takes == nil {
		candidates = append(candidates, m.gateWake)
	}
	if c := m.cmd; c != nil {
		candidates = append(candidates, c.retryAt)
		if c.running {
			candidates = append(candidates, c.sentAt.Add(SwitchDeadline))
		}
	}
	if t := m.takes; t != nil {
		candidates = append(candidates, t.retryAt)
		if t.outstanding {
			candidates = append(candidates, t.sentAt.Add(SwitchDeadline))
		}
		for _, it := range t.items {
			if it.status == awaiting {
				candidates = append(candidates, it.deadline)
			}
		}
	}
	var wake time.Time
	for _, c := range candidates {
		if c.IsZero() || !c.After(now) {
			continue
		}
		if wake.IsZero() || c.Before(wake) {
			wake = c
		}
	}
	return wake
}
