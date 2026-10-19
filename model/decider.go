// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// decider.go: the pure decision state machine (SPEC §4.3, §11.3).

package model

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/unbrice/soft-kvm/state"
)

// Observation is what the agent saw, the decider's only input besides the
// time. At most one field is set: none when only time passed (SPEC §11.3).
type Observation struct {
	Attach   bool                  // receiver attach edge from the Detector
	State    *state.ServerState    // from /state or a /wait wake
	Ran      *CommandsRun          // the commands in flight finished
	GaveAway *AssignmentsGivenAway // the assignments in flight were given away
}

// CommandsRun ends a run of commands: one result per command, in the order
// they ran, nil when it exited 0.
type CommandsRun struct {
	Results []error
}

// AssignmentsGivenAway ends a batch of assignments given away: one result per
// assignment, nil when its screen or device moved, ErrNotHere when there was
// nothing to move.
type AssignmentsGivenAway struct {
	Results map[state.AssignmentKey]error
}

// ErrNotHere is the result of giving away an assignment with nothing to move on
// this host: no monitor or device of that name, or the device not linked here.
// It is not a failure.
var ErrNotHere = errors.New("nothing to move on this host")

// Decision is the decider's only output. The agent carries it out and feeds
// results back as the next Observation (SPEC §11.3).
type Decision struct {
	Claim    string             // non-empty: POST /claim/<id>
	Run      []Command          // run these commands, in order
	GiveAway []state.Assignment // give away what these assignments name: move each screen or device to its assignee
	Notify   bool               // run --notify-cmd
	Save     *state.AgentState  // non-nil: persist this as agent.json
	Log      string             // non-empty: the decision loop logs it at Info
}

// Built-in -A commands: the agent runs them in-process instead of exec'ing
// them (SPEC §5.4).
const (
	TakeScreen = "take-screen"
	TakeHID    = "take-hid"
)

// Command is one -A or -D command to run. Exactly one field is set.
type Command struct {
	Argv       []string // exec this argv
	TakeScreen int64    // non-zero: post this host's screen declarations as takes, under this activation
	TakeHID    int64    // non-zero: post this host's device declarations as takes, under this activation
}

// command turns one argv of the -A or -D list into the Command it runs. Only an
// activation takes, under its activation: connect refuses take-screen and
// take-hid in -D.
func command(argv []string, activating bool, activation int64) Command {
	switch {
	case activating && argv[0] == TakeScreen:
		return Command{TakeScreen: activation}
	case activating && argv[0] == TakeHID:
		return Command{TakeHID: activation}
	}
	return Command{Argv: argv}
}

// String is the command as the flag spelled it.
func (c Command) String() string {
	switch {
	case c.TakeScreen != 0:
		return TakeScreen
	case c.TakeHID != 0:
		return TakeHID
	}
	return strings.Join(c.Argv, " ")
}

// DeciderConfig is the policy knobs from the connect flags (SPEC §5.4).
type DeciderConfig struct {
	ID string
	// OnActivation runs when this host becomes the owner, OnDeactivation when
	// it stops being the owner; each is an ordered list of argv slices
	// (SPEC §4.3, §5.4). Either may be empty. In OnActivation the argv
	// take-screen and take-hid run in-process.
	OnActivation   [][]string
	OnDeactivation [][]string
	Settle         time.Duration
	SwitchRetries  int
	RetrySpacing   time.Duration
	// ArrivalWindow is how long a host that sent a device waits for the
	// assignee to report it arrived (SPEC §4.3).
	ArrivalWindow time.Duration
	// Cooldown, BreakerWindow, BreakerMax and BreakerPause tune the §4.3
	// rate limit, counted per assignment name.
	Cooldown      time.Duration
	BreakerWindow time.Duration
	BreakerMax    int
	BreakerPause  time.Duration
}

// status is where one task stands.
type status int

const (
	pending         status = iota // not issued yet
	issued                        // its result is awaited
	awaitingArrival               // a device sent: waiting for the assignee to report it arrived
	failed                        // the issue failed, or the device did not arrive in time
	done                          // succeeded, arrived, or nothing to move here
	superseded                    // a newer take of the same name replaced its assignment
)

// ownership is who owns, under which activation: what /state says, and what
// agent.json records as handled.
type ownership struct {
	owner      string
	activation int64
}

// handled is what this host has handled, as agent.json persists it: the
// ownership whose commands last ran, and the seq of each assignment last
// handled (SPEC §4.3).
type handled struct {
	ownership ownership
	seqs      map[state.AssignmentKey]state.Seq
}

func (h handled) has(key state.AssignmentKey, seq state.Seq) bool {
	s, ok := h.seqs[key]
	return ok && s == seq
}

// add adds what a job handled.
func (h *handled) add(r record) {
	if r.ownership != nil {
		h.ownership = *r.ownership
	}
	maps.Copy(h.seqs, r.seqs)
}

// agentState is what agent.json persists.
func (h handled) agentState() *state.AgentState {
	rec := &state.AgentState{
		HandledOwner:      h.ownership.owner,
		HandledActivation: h.ownership.activation,
		HandledSeqs:       make(map[string]state.Seq, len(h.seqs)),
	}
	for k, seq := range h.seqs {
		rec.HandledSeqs[k.String()] = seq
	}
	return rec
}

// rateLimiter holds back giving an assignment away, never commands (SPEC
// §4.3), two ways: a cooldown per name after each attempt, and a circuit
// breaker: BreakerMax attempts of one name within BreakerWindow pause every
// name for BreakerPause.
type rateLimiter struct {
	cfg           DeciderConfig
	cooldownUntil map[state.AssignmentKey]time.Time
	pausedUntil   time.Time                           // every name, by the breaker
	recent        map[state.AssignmentKey][]time.Time // attempts per name, within the breaker window
}

func newRateLimiter(cfg DeciderConfig) rateLimiter {
	return rateLimiter{
		cfg:           cfg,
		cooldownUntil: make(map[state.AssignmentKey]time.Time),
		recent:        make(map[state.AssignmentKey][]time.Time),
	}
}

// limitedUntil reports until when key may not be given away, and why; the zero
// time means it may be now.
func (l *rateLimiter) limitedUntil(now time.Time, key state.AssignmentKey) (time.Time, string) {
	var until time.Time
	var parts []string
	if t := l.cooldownUntil[key]; now.Before(t) {
		until = t
		parts = append(parts, fmt.Sprintf("cooldown until %v", t))
	}
	if now.Before(l.pausedUntil) {
		if l.pausedUntil.After(until) {
			until = l.pausedUntil
		}
		parts = append(parts, fmt.Sprintf("every give-away paused until %v", l.pausedUntil))
	}
	return until, strings.Join(parts, ", ")
}

// count records an attempt to give key away: its cooldown starts, and it counts
// toward the breaker.
func (l *rateLimiter) count(now time.Time, key state.AssignmentKey) {
	l.cooldownUntil[key] = now.Add(l.cfg.Cooldown)
	cutoff := now.Add(-l.cfg.BreakerWindow)
	var kept []time.Time
	for _, at := range l.recent[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	l.recent[key] = kept
	if len(kept) >= l.cfg.BreakerMax {
		l.pausedUntil = now.Add(l.cfg.BreakerPause)
	}
}

// taskHeader is what every task carries: its name, where it stands and its
// attempts. A failed task is issued again RetrySpacing after its last issue,
// until it succeeds or runs out of attempts.
type taskHeader struct {
	name        string // names the task in logs
	status      status
	attempts    int
	lastFailure string    // why it last failed
	retryAt     time.Time // failed: not issued again before this
}

func (h *taskHeader) retriable(maxAttempts int) bool {
	return h.status == failed && h.attempts < maxAttempts
}

// due reports whether the task is to be issued at now: it never was, or it
// failed with attempts left and its retryAt has passed.
func (h *taskHeader) due(now time.Time, maxAttempts int) bool {
	return h.status == pending || h.retriable(maxAttempts) && !now.Before(h.retryAt)
}

// issueLog names the task in the log of its issue: by name the first time,
// with the attempt and the last failure on a retry.
func (h *taskHeader) issueLog() string {
	if h.status == pending {
		return h.name
	}
	return fmt.Sprintf("%s attempt %d after %s", h.name, h.attempts+1, h.lastFailure)
}

// issue marks the task issued at now.
func (h *taskHeader) issue(now time.Time, spacing time.Duration) {
	h.status = issued
	h.attempts++
	h.retryAt = now.Add(spacing)
}

// commandTask runs one -A or -D command. Each is retried on its own, so a
// take-screen or take-hid the server already accepted is not posted again.
type commandTask struct {
	taskHeader
	cmd Command
}

// assignmentTask gives one assignment away.
type assignmentTask struct {
	taskHeader
	assignment state.Assignment
	// triedToMove is set once giving the assignment away reported anything but
	// ErrNotHere, or nothing at all: only an attempt counts for the cooldown
	// and the breaker. Nothing to move here is not a write, and the breaker
	// exists to stop write loops.
	triedToMove bool
	// obsolete is set when a newer take replaces the assignment of an issued
	// task: it is superseded once its result arrives, whatever the result.
	obsolete        bool
	arrivalDeadline time.Time // awaitingArrival: when the arrival window closes
}

// record is what a job adds to handled once its tasks are settled, whether or
// not they succeeded: a failed give-away is final until a newer take replaces
// the assignment, and the commands of an ownership change do not run again for
// it.
type record struct {
	ownership *ownership                        // the ownership to record, if any
	seqs      map[state.AssignmentKey]state.Seq // the seq of each assignment to record
}

// String names the record in logs.
func (r record) String() string {
	var parts []string
	if r.ownership != nil {
		parts = append(parts, fmt.Sprintf("owner %q (activation %d)", r.ownership.owner, r.ownership.activation))
	}
	if len(r.seqs) > 0 {
		parts = append(parts, fmt.Sprintf("%d assignments", len(r.seqs)))
	}
	return strings.Join(parts, ", ")
}

// job is the work in flight: the -A or -D commands an ownership change runs,
// or the assignments a reconcile gives away, issued together; then, once every
// task is settled, the record of what it handled — a restart before the
// record runs the tasks again. A job is built whole, its tasks pending.
type job struct {
	commands    []*commandTask    // in the order the flags list them
	assignments []*assignmentTask // in the order /state lists them
	record      record
}

// headers yields the header of every task of the job.
func (j *job) headers() iter.Seq[*taskHeader] {
	return func(yield func(*taskHeader) bool) {
		for _, t := range j.commands {
			if !yield(&t.taskHeader) {
				return
			}
		}
		for _, t := range j.assignments {
			if !yield(&t.taskHeader) {
				return
			}
		}
	}
}

// settled reports whether nothing is left to issue, in flight or left to
// retry: every task is done, superseded or out of attempts.
func (j *job) settled(maxAttempts int) bool {
	for h := range j.headers() {
		if h.status == pending || h.status == issued || h.status == awaitingArrival || h.retriable(maxAttempts) {
			return false
		}
	}
	return true
}

// busy reports whether an issue awaits its result: the worker is still on it,
// even if every task in it has since been made obsolete.
func (j *job) busy() bool {
	for h := range j.headers() {
		if h.status == issued {
			return true
		}
	}
	return false
}

// issue marks the tasks due at now issued and returns the Decision that
// carries them out; ok is false when none is due.
func (j *job) issue(now time.Time, maxAttempts int, spacing time.Duration) (d Decision, ok bool) {
	starting := false
	for h := range j.headers() {
		starting = starting || h.status == pending
	}
	var commands, assignments []string
	for _, t := range j.commands {
		if t.due(now, maxAttempts) {
			commands = append(commands, t.issueLog())
			t.issue(now, spacing)
			d.Run = append(d.Run, t.cmd)
		}
	}
	for _, t := range j.assignments {
		if t.due(now, maxAttempts) {
			assignments = append(assignments, t.issueLog())
			t.issue(now, spacing)
			d.GiveAway = append(d.GiveAway, t.assignment)
		}
	}
	if len(commands) == 0 && len(assignments) == 0 {
		return Decision{}, false
	}
	if !starting {
		d.Log = "retrying " + strings.Join(slices.Concat(commands, assignments), ", ")
		return d, true
	}
	var parts []string
	if len(commands) > 0 {
		parts = append(parts, "running "+strings.Join(commands, ", "))
	}
	if len(assignments) > 0 {
		parts = append(parts, "giving away "+strings.Join(assignments, ", "))
	}
	d.Log = strings.Join(parts, "; ")
	return d, true
}

// expire fails the assignments awaiting arrival whose window closed by now.
func (j *job) expire(now time.Time, window time.Duration) {
	for _, t := range j.assignments {
		if t.status == awaitingArrival && !now.Before(t.arrivalDeadline) {
			t.status = failed
			t.lastFailure = fmt.Sprintf("did not arrive within %v", window)
		}
	}
}

// deadlines lists when the job next needs attention: the arrival windows, and
// — once no issue is outstanding — the retries.
func (j *job) deadlines(maxAttempts int) []time.Time {
	var ds []time.Time
	for _, t := range j.assignments {
		if t.status == awaitingArrival {
			ds = append(ds, t.arrivalDeadline)
		}
	}
	if !j.busy() {
		for h := range j.headers() {
			if h.retriable(maxAttempts) {
				ds = append(ds, h.retryAt)
			}
		}
	}
	return ds
}

// reportRun records the results of the commands issued, matched to them in
// order. A command with no result counts as failed: its run never reported it.
// ok is false when no command is issued: the result is late.
func (j *job) reportRun(results []error) (ok bool) {
	i := 0
	for _, t := range j.commands {
		if t.status != issued {
			continue
		}
		ok = true
		err := errors.New("no result for it")
		if i < len(results) {
			err = results[i]
		}
		i++
		if err != nil {
			t.status, t.lastFailure = failed, err.Error()
		} else {
			t.status = done
		}
	}
	return ok
}

// reportGiveAway records the results of the assignments issued. An assignment
// with no result counts as failed: its batch never reported it. A device sent
// away awaits its arrival until now+window. ok is false when no assignment is
// issued: the result is late.
func (j *job) reportGiveAway(now time.Time, results map[state.AssignmentKey]error, window time.Duration) (ok bool) {
	for _, t := range j.assignments {
		if t.status != issued {
			continue
		}
		ok = true
		if t.obsolete {
			t.status = superseded
			continue
		}
		key := t.assignment.Key()
		err, reported := results[key]
		notHere := errors.Is(err, ErrNotHere)
		t.triedToMove = t.triedToMove || !reported || !notHere
		switch {
		case !reported:
			t.status, t.lastFailure = failed, "no result for it"
		case notHere:
			t.status = done
		case err != nil:
			t.status, t.lastFailure = failed, err.Error()
		case key.Kind == state.KindScreen:
			// A screen has no receipt: the write was made, and that is all.
			t.status = done
		default:
			t.status, t.arrivalDeadline = awaitingArrival, now.Add(window)
		}
	}
	return ok
}

// applyState brings the assignment tasks up to date with a newer state s:
// those it made obsolete are superseded, and the devices it shows arrived are
// done.
func (j *job) applyState(s *state.ServerState) {
	if s == nil {
		return
	}
	current := assignmentsByKey(s)
	j.supersedeObsoleteAssignments(current)
	j.markArrived(current)
}

// supersedeObsoleteAssignments drops the assignment tasks a newer state made
// obsolete: an assignment that is gone or has a new seq was superseded, and the
// new one is pending on its own, not part of this job. Only unfinished work is
// superseded: a done task already moved its screen or device. An issued task is
// only marked: it is superseded once its result arrives.
func (j *job) supersedeObsoleteAssignments(current map[state.AssignmentKey]state.Assignment) {
	for _, t := range j.assignments {
		if t.status == done {
			continue
		}
		if a, ok := current[t.assignment.Key()]; ok && a.Seq == t.assignment.Seq {
			continue
		}
		if t.status == issued {
			t.obsolete = true
		} else {
			t.status = superseded
		}
	}
}

// markArrived finishes the devices given away whose current assignment reads
// arrived: the arrival is their receipt, even after the window closed.
func (j *job) markArrived(current map[state.AssignmentKey]state.Assignment) {
	for _, t := range j.assignments {
		if t.status != awaitingArrival && t.status != failed {
			continue
		}
		a, ok := current[t.assignment.Key()]
		if ok && a.Seq == t.assignment.Seq && a.Kind == state.KindHID && a.Arrived {
			t.status = done
		}
	}
}

// tried yields the keys of the assignments the job tried to move.
func (j *job) tried() iter.Seq[state.AssignmentKey] {
	return func(yield func(state.AssignmentKey) bool) {
		for _, t := range j.assignments {
			if t.triedToMove && !yield(t.assignment.Key()) {
				return
			}
		}
	}
}

// failureLogs logs, once the job is settled, what did not succeed in it. A
// failed command is only logged: nothing in a command is a one-way door, and
// the user's fallback is the monitor's Input button.
func (j *job) failureLogs() []Decision {
	var ds []Decision
	for h := range j.headers() {
		switch h.status {
		case failed:
			ds = append(ds, Decision{Log: fmt.Sprintf("%s failed after %d attempts (%s)", h.name, h.attempts, h.lastFailure)})
		case superseded:
			ds = append(ds, Decision{Log: fmt.Sprintf("%s dropped: a newer take replaced its assignment", h.name)})
		}
	}
	return ds
}

// notifyFor names, once the job is settled, the assignments host me notifies
// for: given away and still failed after the last attempt, with me as their
// previous assignee — the one the user is looking at. Any other host only logs
// the failure (SPEC §4.3).
func (j *job) notifyFor(me string) []string {
	var names []string
	for _, t := range j.assignments {
		if t.status == failed && t.assignment.Prev == me {
			names = append(names, t.name)
		}
	}
	return names
}

// assignmentsByKey indexes the assignments of s by key.
func assignmentsByKey(s *state.ServerState) map[state.AssignmentKey]state.Assignment {
	m := make(map[state.AssignmentKey]state.Assignment, len(s.Assignments))
	for _, a := range s.Assignments {
		m[a.Key()] = a
	}
	return m
}

// Decider is the decision state machine: no I/O, no goroutines, no clock. All
// time arrives as Decide's now (SPEC §11.3).
type Decider struct {
	cfg DeciderConfig

	// handled is what agent.json says this host handled; nil when there was
	// none, until the first state is adopted as handled.
	handled *handled

	job     *job // the work in flight, nil when idle
	limiter rateLimiter

	// latest is the newest state seen, which the decider reconciles whenever it
	// is idle.
	latest *state.ServerState
	// reasons is the last reason logged for holding back each assignment not
	// given away: a reason is logged again only once it changed.
	reasons map[state.AssignmentKey]string

	// claimAt is when to claim the ownership, once no job is in flight: Settle
	// after the last attach. Zero when no claim waits.
	claimAt time.Time
}

// NewDecider creates a decider from what agent.json says this host handled.
// rec is nil when there is no agent.json: the first State observation adopts
// the server's owner, activation and assignments as handled and never switches
// (SPEC §4.3).
func NewDecider(cfg DeciderConfig, rec *state.AgentState) (*Decider, error) {
	m := &Decider{
		cfg:     cfg,
		limiter: newRateLimiter(cfg),
		reasons: make(map[state.AssignmentKey]string),
	}
	if rec == nil {
		return m, nil
	}
	m.handled = &handled{
		ownership: ownership{owner: rec.HandledOwner, activation: rec.HandledActivation},
		seqs:      make(map[state.AssignmentKey]state.Seq, len(rec.HandledSeqs)),
	}
	for k, seq := range rec.HandledSeqs {
		if key, ok := state.ParseAssignmentKey(k); ok {
			m.handled.seqs[key] = seq
		} else {
			return nil, fmt.Errorf("invalid assignment key: %q", k)
		}
	}
	return m, nil
}

// Decide records one observation made at now and returns what to do about it,
// and when to call it again with nothing observed: the earliest deadline it
// tracks, zero when there is none. After the observation, the job in flight
// moves on; once idle, a due claim is made, then the latest state reconciled.
func (m *Decider) Decide(now time.Time, o Observation) (ds []Decision, wake time.Time) {
	switch {
	case o.State != nil:
		ds = append(ds, m.handleState(o.State)...)
	case o.Attach:
		m.handleAttach(now)
	case o.Ran != nil:
		ds = append(ds, m.handleRan(o.Ran)...)
	case o.GaveAway != nil:
		ds = append(ds, m.handleGaveAway(now, o.GaveAway)...)
	}

	m.advanceTime(now, &ds)
	return ds, m.wakeAt(now)
}

// handleState processes a State observation.
func (m *Decider) handleState(s *state.ServerState) []Decision {
	if m.handled == nil {
		m.handled = &handled{
			ownership: ownership{owner: s.Owner, activation: s.Activation},
			seqs:      make(map[state.AssignmentKey]state.Seq, len(s.Assignments)),
		}
		for _, a := range s.Assignments {
			m.handled.seqs[a.Key()] = a.Seq
		}
		return []Decision{{
			Save: m.handled.agentState(),
			Log: fmt.Sprintf("adopted owner %q, activation %d and %d assignments from server; nothing runs on first reconcile",
				s.Owner, s.Activation, len(s.Assignments)),
		}}
	}
	m.latest = s
	if m.job != nil {
		m.job.applyState(s)
	}
	return nil
}

// handleAttach processes a receiver attach edge: the claim waits Settle after
// it, so a flapping receiver claims once it is stable.
func (m *Decider) handleAttach(now time.Time) {
	m.claimAt = now.Add(m.cfg.Settle)
}

// handleRan records the results of the commands in flight. A result is
// accepted only while its issue is outstanding (SPEC §4.3).
func (m *Decider) handleRan(d *CommandsRun) []Decision {
	if m.job == nil || !m.job.reportRun(d.Results) {
		return []Decision{{Log: "ignoring a late command result"}}
	}
	return nil
}

// handleGaveAway records the results of the assignments in flight. A result is
// accepted only while its issue is outstanding (SPEC §4.3).
func (m *Decider) handleGaveAway(now time.Time, d *AssignmentsGivenAway) []Decision {
	if m.job == nil || !m.job.reportGiveAway(now, d.Results, m.cfg.ArrivalWindow) {
		return []Decision{{Log: "ignoring a late give-away result"}}
	}
	// The latest state may already show a sent device arrived.
	m.job.applyState(m.latest)
	return nil
}

// reconcile returns the job a state calls for while idle, nil when none, and
// why: in priority order, the activation edge, the deactivation edge, then
// pending assignments (SPEC §4.3). Reconciling a state again changes nothing
// once its job is over: every job records what it handled.
func (m *Decider) reconcile(now time.Time, s *state.ServerState) (*job, []Decision) {
	me := m.cfg.ID
	last := m.handled.ownership
	switch {
	case s.Owner == me && (last.owner != me || s.Activation != last.activation):
		// Ownership came to this host, or was re-activated: run the activation
		// commands. Not a one-way door, so the rate limiter does not hold it
		// back.
		return ownershipChangeJob(s, m.cfg.OnActivation, true), []Decision{{
			Log: fmt.Sprintf("activating %q: ownership came to me (activation %d)", me, s.Activation)}}
	case s.Owner != me && s.Owner != "" && last.owner == me:
		return ownershipChangeJob(s, m.cfg.OnDeactivation, false), []Decision{{
			Log: fmt.Sprintf("deactivating: ownership went to %q", s.Owner)}}
	}
	ps := pendingAssignments(now, s, me, *m.handled, &m.limiter)
	return giveAwayJob(ps), m.explain(ps)
}

// ownershipChangeJob is the job an ownership change runs: the -A or -D
// commands argvs, then the record of the ownership s shows — a restart before
// it runs them again. With no command on that side, the record is the whole
// job.
func ownershipChangeJob(s *state.ServerState, argvs [][]string, activating bool) *job {
	flag := "-D "
	if activating {
		flag = "-A "
	}
	j := &job{record: record{ownership: &ownership{owner: s.Owner, activation: s.Activation}}}
	for _, argv := range argvs {
		c := command(argv, activating, s.Activation)
		j.commands = append(j.commands, &commandTask{taskHeader: taskHeader{name: flag + c.String()}, cmd: c})
	}
	return j
}

// verdict is what host me does now with an assignment it has not handled.
type verdict int

const (
	giveNow         verdict = iota
	alreadyArrived          // on its assignee already: recorded, not given away
	assigneeNotLive         // waits for its assignee to be live
	rateLimited             // waits for the rate limiter to let it go
)

// pendingAssignment is an assignment host me has not handled, and what it does
// with it now.
type pendingAssignment struct {
	assignment state.Assignment
	verdict    verdict
	reason     string    // not giveNow: why, as logged
	until      time.Time // rateLimited: when the rate limiter lets it go
}

// pendingAssignments assesses the assignments of s host me has not handled:
// someone else's, at a seq it has not handled yet. It is the one place the
// gates and the rate limiter are applied.
func pendingAssignments(now time.Time, s *state.ServerState, me string, h handled, l *rateLimiter) []pendingAssignment {
	var ps []pendingAssignment
	for _, a := range s.Assignments {
		key := a.Key()
		if a.Assignee == me || h.has(key, a.Seq) {
			continue
		}
		p := pendingAssignment{assignment: a}
		switch {
		case a.Arrived:
			p.verdict = alreadyArrived
			p.reason = fmt.Sprintf("not giving away %s: already arrived on %q", key, a.Assignee)
		case !s.Live[a.Assignee]:
			p.verdict = assigneeNotLive
			p.reason = fmt.Sprintf("not giving away %s: assignee %q is not live; it stays pending", key, a.Assignee)
		default:
			if until, why := l.limitedUntil(now, key); !until.IsZero() {
				p.verdict, p.until = rateLimited, until
				p.reason = fmt.Sprintf("deferring %s: %s", key, why)
			}
		}
		ps = append(ps, p)
	}
	return ps
}

// giveAwayJob is the job of the pending assignments ps: giving away those to
// give now, then recording their seqs along with those already arrived on
// their assignee; nil when nothing is to give away or record.
func giveAwayJob(ps []pendingAssignment) *job {
	j := &job{record: record{seqs: make(map[state.AssignmentKey]state.Seq)}}
	for _, p := range ps {
		key := p.assignment.Key()
		switch p.verdict {
		case giveNow:
			j.assignments = append(j.assignments, &assignmentTask{taskHeader: taskHeader{name: key.String()}, assignment: p.assignment})
			j.record.seqs[key] = p.assignment.Seq
		case alreadyArrived:
			j.record.seqs[key] = p.assignment.Seq
		}
	}
	if len(j.record.seqs) == 0 {
		return nil
	}
	return j
}

// limitLifts is when the rate limiter first lets one of the pending
// assignments ps go; zero when it holds none back.
func limitLifts(ps []pendingAssignment) time.Time {
	var first time.Time
	for _, p := range ps {
		if p.verdict == rateLimited && (first.IsZero() || p.until.Before(first)) {
			first = p.until
		}
	}
	return first
}

// explain logs why each pending assignment of ps is held back, each reason
// once: it is logged again only after it changed, and forgotten once the
// assignment is given away or handled.
func (m *Decider) explain(ps []pendingAssignment) []Decision {
	var ds []Decision
	held := make(map[state.AssignmentKey]bool, len(ps))
	for _, p := range ps {
		if p.verdict == giveNow {
			continue
		}
		key := p.assignment.Key()
		held[key] = true
		if m.reasons[key] != p.reason {
			m.reasons[key] = p.reason
			ds = append(ds, Decision{Log: p.reason})
		}
	}
	for key := range m.reasons {
		if !held[key] {
			delete(m.reasons, key)
		}
	}
	return ds
}

// maxAttempts is how many times a task may be issued: once, then the retries.
func (m *Decider) maxAttempts() int { return 1 + m.cfg.SwitchRetries }

// advanceTime moves the decider on to now: the job in flight first; once idle,
// a due claim, then the latest state reconciled. It loops because ending a job
// lets a due claim through and calls for the next job.
func (m *Decider) advanceTime(now time.Time, ds *[]Decision) {
	for {
		switch {
		case m.job != nil:
			m.advanceJob(now, ds)
			if m.job != nil {
				return
			}
		case !m.claimAt.IsZero() && !m.claimAt.After(now):
			m.claimAt = time.Time{}
			*ds = append(*ds, Decision{Claim: m.cfg.ID, Log: fmt.Sprintf("claim %q", m.cfg.ID)})
		case m.latest != nil:
			j, more := m.reconcile(now, m.latest)
			*ds = append(*ds, more...)
			if j == nil {
				return
			}
			m.job = j
		default:
			return
		}
	}
}

// advanceJob moves the job in flight on at now: it expires closed arrival
// windows and, once no issue is outstanding, issues the tasks due. Once every
// task is settled, the job is over: what it handled is recorded and saved, the
// names it tried to move count toward the rate limit, what failed is logged
// and, when this host was the previous assignee, notified.
func (m *Decider) advanceJob(now time.Time, ds *[]Decision) {
	m.job.expire(now, m.cfg.ArrivalWindow)
	if m.job.busy() {
		return
	}
	if !m.job.settled(m.maxAttempts()) {
		if d, ok := m.job.issue(now, m.maxAttempts(), m.cfg.RetrySpacing); ok {
			*ds = append(*ds, d)
		}
		return
	}
	m.handled.add(m.job.record)
	for key := range m.job.tried() {
		m.limiter.count(now, key)
	}
	*ds = append(*ds, m.job.failureLogs()...)
	d := Decision{Save: m.handled.agentState(), Log: "recording " + m.job.record.String()}
	if names := m.job.notifyFor(m.cfg.ID); len(names) > 0 {
		d.Notify = true
		d.Log += "; notifying: I was the previous assignee of " + strings.Join(names, ", ")
	}
	*ds = append(*ds, d)
	m.job = nil
}

// wakeAt returns the earliest future deadline the decider is tracking: the
// claim, then the job's deadlines, or the rate limiter when idle.
func (m *Decider) wakeAt(now time.Time) time.Time {
	candidates := []time.Time{m.claimAt}
	switch {
	case m.job != nil:
		candidates = append(candidates, m.job.deadlines(m.maxAttempts())...)
	case m.latest != nil:
		candidates = append(candidates, limitLifts(pendingAssignments(now, m.latest, m.cfg.ID, *m.handled, &m.limiter)))
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
