// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// agent.go: the connect agent — the loop that feeds the Machine (SPEC §5.4,
// §11.3). The shared seams it is built from:

package main

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"

	"golang.org/x/sync/errgroup"
)

// backoff is an exponential backoff with full jitter, base 1 s, cap 30 s
// (SPEC §8). Jitter desynchronises the two agents after a shared outage.
type backoff struct{ cur time.Duration }

func newBackoff() *backoff { return &backoff{cur: time.Second} }

// next returns the sleep for this failure: uniform in [0, cur], then
// doubles cur up to the 30 s cap.
func (b *backoff) next() time.Duration {
	d := b.cur
	if b.cur < 30*time.Second {
		b.cur *= 2
		if b.cur > 30*time.Second {
			b.cur = 30 * time.Second
		}
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

func (b *backoff) reset() { b.cur = time.Second }

// Detector emits one event per receiver attach edge (SPEC §11.2).
// Implementations: netlink uevents (Linux), ioreg poll (macOS), gdbus/bluez
// (Linux fallback), and test fakes. Only attach edges exist: disconnect is
// ambiguous (SPEC finding 2) and never reported. Run returns nil on ctx
// cancellation.
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
}

// agent runs the connect loop until ctx is cancelled.
type agent struct {
	cfg     agentConfig
	machine *Machine
}

// run starts the detector, watcher, and main loop, then blocks until ctx is
// cancelled. It uses one parent context and per-concern child contexts for the
// detector and watcher so guards can restart them (SPEC §11.1, §5.4.4).
// Returns nil on ctx cancellation.
func (a *agent) run(ctx context.Context) error {
	as := agentState{}
	_, statErr := os.Stat(a.cfg.agentStatePath)
	hasRecord := statErr == nil
	if err := loadJSON(a.cfg.agentStatePath, &as); err != nil {
		slog.Warn("corrupt agent state, starting fresh", "path", a.cfg.agentStatePath, "error", err)
		as = agentState{}
		hasRecord = false
	}

	a.machine = NewMachine(*a.cfg.machine, as.LastOwner, hasRecord)

	ctx, cancel := context.WithCancel(ctx)

	// Claims are supervised one-shots. SetLimit(1) + TryGo coalesces a
	// redundant trigger while a claim is still retrying — the claim is always
	// for cfg.id and idempotent server-side, so the dropped trigger loses
	// nothing. cancel must run before Wait, else a claim mid-backoff holds
	// shutdown for the rest of its retry schedule.
	var claims errgroup.Group
	claims.SetLimit(1)
	defer func() {
		cancel()
		_ = claims.Wait() // claims never return an error
	}()

	// Created fresh by startWorkers on every guard-up, so a late event from a
	// cancelled worker generation lands in an orphaned channel and is never
	// processed.
	var (
		attachCh chan struct{}
		stateCh  chan *ServerState
		workers  *errgroup.Group
	)

	guardTicker := time.NewTicker(15 * time.Second)
	defer guardTicker.Stop()

	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	var (
		childCtx    context.Context
		childCancel context.CancelFunc = func() {}
		guardsUp    bool
	)

	startWorkers := func() {
		if guardsUp {
			return
		}
		attachCh = make(chan struct{}, 1)
		stateCh = make(chan *ServerState, 1)
		childCtx, childCancel = context.WithCancel(ctx)
		workers = new(errgroup.Group)
		workers.Go(func() error { return a.detectorLoop(childCtx, attachCh) })
		workers.Go(func() error { return a.watcherLoop(childCtx, stateCh) })
		guardsUp = true
	}

	stopWorkers := func() {
		if !guardsUp {
			return
		}
		childCancel()
		// The workers are cancel-responsive (the netlink detector within one
		// 200 ms poll); wait so a new generation never overlaps a dying one.
		_ = workers.Wait() // the loops return nil
		guardsUp = false
		// Stop the timer so dormant periods do not deliver stale timer events.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	defer stopWorkers()

	feed := func(e Event) {
		queue := []Event{e}
		for len(queue) > 0 {
			ev := queue[0]
			queue = queue[1:]
			actions := a.machine.Step(ev)
			for _, act := range actions {
				if act.Log != "" {
					slog.Info(act.Log)
				}
				if act.SaveOwner != nil {
					if err := saveJSON(a.cfg.agentStatePath, agentState{LastOwner: *act.SaveOwner}); err != nil {
						slog.Error("failed to save agent state", "path", a.cfg.agentStatePath, "error", err)
					}
				}
				if act.Claim != "" {
					if !claims.TryGo(func() error { a.claim(ctx, act.Claim); return nil }) {
						slog.Debug("claim already in flight, coalesced", "id", act.Claim)
					}
				}
				if act.Switch {
					err := a.cfg.runner(ctx, a.cfg.switchArgv)
					queue = append(queue, Event{Now: time.Now(), SwitchExit: &err})
				}
				if act.Probe {
					probeCtx, cancel := context.WithTimeout(ctx, a.cfg.checkTimeout)
					err := a.cfg.runner(probeCtx, a.cfg.checkArgv)
					cancel()
					queue = append(queue, Event{Now: time.Now(), ProbeExit: &err})
				}
				if act.Notify {
					if err := a.cfg.runner(ctx, a.cfg.notifyArgv); err != nil {
						slog.Error("notify command failed", "error", err)
					}
				}
			}
		}
	}

	for {
		ok, reason := a.cfg.guard.OK(ctx)

		if !ok {
			if guardsUp {
				stopWorkers()
				slog.Info("guards down, dormant", "reason", reason)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-guardTicker.C:
			}
			continue
		}

		if !guardsUp {
			startWorkers()
			slog.Info("guards up", "reason", reason)
			// Deadlines may have elapsed while dormant; let the machine catch up.
			feed(Event{Now: time.Now()})
			continue
		}

		now := time.Now()
		wake := a.machine.wakeAt(now)
		if wake.IsZero() {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		} else {
			d := time.Until(wake)
			if d <= 0 {
				d = 1 // deliver a due deadline on the next select
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(d)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-guardTicker.C:
			// loop; guard check at top
		case <-timer.C:
			feed(Event{Now: time.Now()})
		case st := <-stateCh:
			feed(Event{Now: time.Now(), State: st})
		case <-attachCh:
			feed(Event{Now: time.Now(), Attach: true})
		}
	}
}

// detectorLoop runs the detector with backoff until ctx is cancelled. It only
// ever returns nil: failures are retried, cancellation is the only exit.
func (a *agent) detectorLoop(ctx context.Context, attachCh chan<- struct{}) error {
	b := newBackoff()
	for {
		err := a.cfg.detector.Run(ctx, attachCh)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		slog.Error("detector failed", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(b.next()):
		}
	}
}

// watcherLoop resolves the server, reconciles, then long-polls /wait until ctx
// is cancelled. It sends every fresh state to stateCh (SPEC §5.4.3, §8). Like
// detectorLoop, it only ever returns nil.
func (a *agent) watcherLoop(ctx context.Context, stateCh chan<- *ServerState) error {
	b := newBackoff()

	for {
		base, err := resolveServer(ctx, a.cfg.explicitServer)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			slog.Error("watcher: resolve server failed", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(b.next()):
			}
			continue
		}

		state, err := a.clientState(ctx, base)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if errors.Is(err, ErrUnauthorized) {
				slog.Error("watcher: token rejected")
			} else {
				slog.Error("watcher: state failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(b.next()):
			}
			continue
		}
		// A successful /state proves the coordinator is reachable; only now
		// does the backoff reset (SPEC §5.4.3).
		b.reset()
		saveCachedServer(base)
		if !a.sendState(ctx, stateCh, state) {
			return nil
		}

		epoch := state.Epoch
		for {
			// Wall-only start so sleep detection compares wall-clock progress.
			start := time.Now().Round(0)
			woke, err := a.clientWait(ctx, base, epoch)
			if ctx.Err() != nil {
				return nil
			}

			if err != nil {
				if errors.Is(err, ErrUnauthorized) {
					slog.Error("watcher: token rejected")
				} else {
					slog.Error("watcher: wait failed", "error", err)
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(b.next()):
				}
				break // re-resolve on any connection error
			}

			// The internal client timeout is 60 s; a wall jump > 2× that means
			// the machine slept through a long-poll (SPEC §5.4.5).
			if time.Since(start) > 2*60*time.Second {
				slog.Info("watcher: sleep detected, re-resolving")
				break
			}

			if !woke {
				continue
			}

			state, err := a.clientState(ctx, base)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				if errors.Is(err, ErrUnauthorized) {
					slog.Error("watcher: token rejected")
				} else {
					slog.Error("watcher: state failed", "error", err)
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(b.next()):
				}
				break
			}
			b.reset()
			saveCachedServer(base)
			if !a.sendState(ctx, stateCh, state) {
				return nil
			}
			epoch = state.Epoch
		}
	}
}

func (a *agent) clientState(ctx context.Context, base string) (*ServerState, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return a.cfg.client.State(cctx, base)
}

func (a *agent) clientWait(ctx context.Context, base string, epoch int64) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 65*time.Second)
	defer cancel()
	return a.cfg.client.Wait(cctx, base, epoch, a.cfg.id)
}

func (a *agent) sendState(ctx context.Context, stateCh chan<- *ServerState, state *ServerState) bool {
	select {
	case stateCh <- state:
		return true
	case <-ctx.Done():
		return false
	}
}

// claim posts /claim/<id> with force=false, retrying up to four times with
// exponential backoff capped at 30 s. Errors are logged; success caches the
// server address (SPEC §5.4.2, §8).
func (a *agent) claim(ctx context.Context, id string) {
	b := newBackoff()
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(b.next()):
			}
		}

		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		base, err := resolveServer(cctx, a.cfg.explicitServer)
		cancel()
		if err != nil {
			slog.Error("claim: resolve server failed", "attempt", attempt+1, "error", err)
			continue
		}

		cctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		changed, err := a.cfg.client.Claim(cctx, base, id, false)
		cancel()
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				slog.Error("claim: token rejected", "attempt", attempt+1)
			} else {
				slog.Error("claim: failed", "attempt", attempt+1, "error", err)
			}
			continue
		}
		if changed {
			saveCachedServer(base)
		}
		slog.Info("claim: success", "id", id, "changed", changed)
		return
	}
}
