// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// agent.go: the connect agent — supervisor, generations, decision loop and
// claims (SPEC §5.4, §11.3).
//
// Invariants this file is responsible for:
//
//  1. *Machine is owned by the loop goroutine and never shared: it lives on
//     the loop, not on agent.
//  2. No event carries a timestamp; the loop stamps Now at processing time.
//  3. Channels are scoped by freshness (§4.1): attachCh/stateCh are created
//     per generation; results is run-level, capacity 1; actionCh is run-level,
//     chan effect, capacity 2 (the worst case is a two-effect batch while an
//     earlier command still runs). State, result and effect sends are blocking
//     (must not be lost); attach sends inside a generation are non-blocking
//     and coalescing (idempotent triggers).
//  4. Every goroutine belongs to exactly one errgroup and exits only on its
//     context or a sentinel. No goroutine outlives run.
//  5. No blocking I/O on the loop goroutine. Exception: SaveOwner does a small
//     atomic file write inline; nothing downstream depends on it synchronously,
//     and moving it would reorder it against logs for no gain.

package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	// backoffBase and backoffCap bound the exponential backoff with full
	// jitter (SPEC §8). Jitter desynchronises the two agents after a shared
	// outage.
	backoffBase = 1 * time.Second
	backoffCap  = 30 * time.Second

	// defaultGuardPoll is how often guardWatch checks the Guard.
	defaultGuardPoll = 15 * time.Second
)

// backoff is an exponential backoff with full jitter.
type backoff struct {
	base time.Duration
	cur  time.Duration
}

func newBackoff() *backoff { return newBackoffWithBase(backoffBase) }

func newBackoffWithBase(base time.Duration) *backoff {
	return &backoff{base: base, cur: base}
}

// next returns the sleep for this failure: uniform in [0, cur], then doubles
// cur up to backoffCap.
func (b *backoff) next() time.Duration {
	d := b.cur
	if b.cur < backoffCap {
		b.cur *= 2
		if b.cur > backoffCap {
			b.cur = backoffCap
		}
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

func (b *backoff) reset() { b.cur = b.base }

// wait sleeps for the next backoff interval or returns ctx.Err().
func (b *backoff) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(b.next()):
		return nil
	}
}

// Detector emits one event per receiver attach edge (SPEC §11.2).
// Implementations: HID device events via telesma-app/hid, and test fakes.
// Only attach edges exist: disconnect is ambiguous (SPEC finding 2) and
// never reported. Run returns nil on ctx cancellation.
type Detector interface {
	Run(ctx context.Context, attach chan<- struct{}) error
}

// Guard reports whether this host may participate in switching (SPEC §11.2).
// The reason distinguishes "dormant, no AC power" from "dormant, no LG" in
// logs. Linux has no guards; macOS checks AC power and LG presence (§6.2).
type Guard interface {
	OK(ctx context.Context) (ok bool, reason string)
}

// alwaysOK is a Guard adapter used when macOS --no-guards disables the real
// checks.
type alwaysOK struct{ reason string }

func (g alwaysOK) OK(context.Context) (bool, string) { return true, g.reason }

// agentConfig is the wiring the agent loop needs. It contains no policy; that
// lives in Machine (SPEC §11.3).
type agentConfig struct {
	id             string
	explicitServer string
	keyFP          string
	detector       Detector
	guard          Guard
	client         *Client
	runner         Runner
	machine        *MachineConfig
	agentStatePath string
	switchArgv     []string
	checkArgv      []string
	notifyArgv     []string
	checkTimeout   time.Duration
	switchTimeout  time.Duration
	guardPoll      time.Duration
	backoffBase    time.Duration
}

func (c agentConfig) switchTimeoutOrDefault() time.Duration {
	if c.switchTimeout > 0 {
		return c.switchTimeout
	}
	return defaultSwitchTimeout
}

func (c agentConfig) guardPollOrDefault() time.Duration {
	if c.guardPoll > 0 {
		return c.guardPoll
	}
	return defaultGuardPoll
}

func (c agentConfig) backoffBaseOrDefault() time.Duration {
	if c.backoffBase > 0 {
		return c.backoffBase
	}
	return backoffBase
}

// agent runs the connect loop until ctx is cancelled.
type agent struct {
	cfg      agentConfig
	claims   errgroup.Group
	actionCh chan effect
	results  chan Event
}

// newBackoff returns a backoff using the configured base.
func (a *agent) newBackoff() *backoff {
	return newBackoffWithBase(a.cfg.backoffBaseOrDefault())
}

var errGuardsDown = errors.New("guards down")

// run starts the action worker and claims group, then loops over guards-up
// generations until ctx is cancelled. Returns nil on ctx cancellation.
func (a *agent) run(ctx context.Context) error {
	as := agentState{}
	_, statErr := os.Stat(a.cfg.agentStatePath)
	hasRecord := statErr == nil
	if err := loadJSON(a.cfg.agentStatePath, &as); err != nil {
		slog.Warn("corrupt agent state, starting fresh", "path", a.cfg.agentStatePath, "error", err)
		as = agentState{}
		hasRecord = false
	}

	// Invariant 1: the Machine lives on the loop goroutine, never on agent.
	m := NewMachine(*a.cfg.machine, as.LastOwner, hasRecord)

	a.actionCh = make(chan effect, 2)
	a.results = make(chan Event, 1)

	ctx, cancel := context.WithCancel(ctx)

	// Claims are supervised one-shots. SetLimit(1) + TryGo coalesces a
	// redundant trigger while a claim is still retrying — the claim is always
	// for cfg.id and idempotent server-side, so the dropped trigger loses
	// nothing. cancel must run before Wait, else a claim mid-backoff holds
	// shutdown for the rest of its retry schedule.
	a.claims.SetLimit(1)

	var workers errgroup.Group
	workers.Go(func() error { return a.actionLoop(ctx) })

	defer func() {
		cancel()
		_ = a.claims.Wait() // claims never return an error
		_ = workers.Wait()
	}()

	for {
		if !a.waitGuardsUp(ctx) {
			return nil
		}

		err := a.generation(ctx, m)
		if errors.Is(err, errGuardsDown) {
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

// waitGuardsUp blocks until the Guard reports up, logging each transition
// once. Returns false on ctx cancellation.
func (a *agent) waitGuardsUp(ctx context.Context) bool {
	ok, reason := a.cfg.guard.OK(ctx)
	if ok {
		slog.Info("guards up", "reason", reason)
		return true
	}
	slog.Info("guards down, dormant", "reason", reason)
	for !ok {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(a.cfg.guardPollOrDefault()):
		}
		ok, reason = a.cfg.guard.OK(ctx)
	}
	slog.Info("guards up", "reason", reason)
	return true
}

// generation runs one guards-up generation. Its four goroutines share a
// context cancelled on first error. The only non-nil errors a generation
// goroutine may return are errGuardsDown and ctx.Err(); transient failures
// (HTTP, HID) are retried in place. runCtx is the run-level context the loop
// uses for claims: a guard flap must not cancel an in-flight claim.
func (a *agent) generation(ctx context.Context, m *Machine) error {
	attachCh := make(chan struct{}, 1)
	stateCh := make(chan *ServerState, 1)

	runCtx := ctx
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.detectorLoop(ctx, attachCh) })
	g.Go(func() error { return a.watch(ctx, stateCh) })
	g.Go(func() error { return a.guardWatch(ctx) })
	g.Go(func() error { return a.loop(ctx, m, attachCh, stateCh, runCtx) })
	return g.Wait()
}

// guardWatch polls the Guard and returns errGuardsDown on the down edge.
func (a *agent) guardWatch(ctx context.Context) error {
	ticker := time.NewTicker(a.cfg.guardPollOrDefault())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			ok, _ := a.cfg.guard.OK(ctx)
			if !ok {
				return errGuardsDown
			}
		}
	}
}

// detectorLoop runs the detector with backoff until ctx is cancelled. It only
// ever returns nil or ctx.Err(): failures are retried, cancellation is the
// only exit.
func (a *agent) detectorLoop(ctx context.Context, attachCh chan<- struct{}) error {
	b := a.newBackoff()
	for {
		err := a.cfg.detector.Run(ctx, attachCh)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		slog.Error("detector failed", "error", err)
		if err := b.wait(ctx); err != nil {
			return err
		}
	}
}

// loop is the only goroutine that touches m. It stamps Now at processing time
// and dispatches the Actions the Machine returns. runCtx is the run-level
// context claims run under; everything else uses the generation ctx.
func (a *agent) loop(ctx context.Context, m *Machine, attachCh <-chan struct{}, stateCh <-chan *ServerState, runCtx context.Context) error {
	timer := stoppedTimer()
	step := func(ev Event) error {
		ev.Now = time.Now() // the loop is the only clock (§11.3)
		sawWake := false
		for _, act := range m.Step(ev) {
			if act.Log != "" {
				slog.Info(act.Log)
			}
			if act.SaveOwner != nil {
				if err := saveJSON(a.cfg.agentStatePath, agentState{LastOwner: *act.SaveOwner}); err != nil {
					slog.Error("failed to save agent state", "path", a.cfg.agentStatePath, "error", err)
				}
			}
			if act.Claim != "" {
				if !a.claims.TryGo(func() error { a.claim(runCtx, act.Claim); return nil }) {
					slog.Debug("claim already in flight, coalesced", "id", act.Claim)
				}
			}
			if act.Switch {
				select {
				case a.actionCh <- effectSwitch:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if act.Probe {
				select {
				case a.actionCh <- effectProbe:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if act.Notify {
				select {
				case a.actionCh <- effectNotify:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if !act.WakeAt.IsZero() {
				d := time.Until(act.WakeAt)
				if d <= 0 {
					d = time.Nanosecond
				}
				timer.Reset(d)
				sawWake = true
			}
		}
		if !sawWake {
			timer.Stop()
		}
		return nil
	}
	// Deadlines may have elapsed while dormant; let the machine catch up.
	if err := step(Event{}); err != nil {
		return err
	}
	for {
		var ev Event
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-attachCh:
			ev = Event{Attach: true}
		case s := <-stateCh:
			ev = Event{State: s}
		case r := <-a.results:
			ev = r
		case <-timer.C:
		}
		if err := step(ev); err != nil {
			return err
		}
	}
}

func stoppedTimer() *time.Timer {
	t := time.NewTimer(time.Duration(math.MaxInt64))
	t.Stop()
	return t
}

// claim posts /claim/<id> with force=false, retrying up to four times with
// exponential backoff capped at 30 s. Each attempt ranges the server
// candidates under one 5 s context and claims against the first that answers.
// Errors are logged; success caches the server address (SPEC §5.4.2, §8).
func (a *agent) claim(ctx context.Context, id string) {
	b := a.newBackoff()
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			if err := b.wait(ctx); err != nil {
				return
			}
		}

		cctx, cancel := context.WithTimeout(ctx, stateTimeout)
		claimed := false
		for base := range resolveServer(cctx, a.cfg.explicitServer, a.cfg.keyFP) {
			changed, err := a.cfg.client.Claim(cctx, base, id, false)
			if err != nil {
				if errors.Is(err, ErrUnauthorized) {
					slog.Error("claim: token rejected", "attempt", attempt+1)
				} else {
					slog.Debug("claim: candidate failed", "base", base, "attempt", attempt+1, "error", err)
				}
				continue
			}
			if changed {
				saveCachedServer(base)
			}
			slog.Info("claim: success", "id", id, "changed", changed)
			claimed = true
			break
		}
		cancel()
		if claimed {
			return
		}
		if ctx.Err() == nil {
			slog.Error("claim: no candidate answered", "attempt", attempt+1)
		}
	}
}
