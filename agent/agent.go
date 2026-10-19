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
//  3. Channels are scoped by freshness (§4.1): attachCh/stateCh/arrivalCh are
//     created per generation; results is run-level, capacity 1; actionCh is
//     run-level, chan effect, capacity 4 (the worst case is a notification and
//     the next run in one batch while an earlier effect still runs). State,
//     result and effect sends are blocking (must not be lost); attach sends
//     inside a generation are non-blocking and coalescing (idempotent
//     triggers); arrivalCh is capacity 1 and latest-wins, so the arrival
//     watcher can never stall the loop.
//  4. Every goroutine belongs to exactly one errgroup and exits only on its
//     context or a sentinel. No goroutine outlives run.
//  5. No blocking I/O on the loop goroutine. Exception: Save does a small
//     atomic file write inline; nothing downstream depends on it synchronously,
//     and moving it would reorder it against logs for no gain.
//  6. The loop decides nothing: every decision is a model.Action. The action
//     worker and the arrival watcher execute and report, and a result is an
//     ordinary event.

package agent

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"time"

	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/discover"
	"github.com/unbrice/soft-kvm/hidpp"
	"github.com/unbrice/soft-kvm/model"
	"github.com/unbrice/soft-kvm/screen"
	"github.com/unbrice/soft-kvm/state"
	"golang.org/x/sync/errgroup"
)

// DefaultSwitchTimeout bounds one --on-activation or --on-deactivation command
// run, and one write of a take. A hung I²C write must not freeze the agent
// (§4.3).
const DefaultSwitchTimeout = 30 * time.Second

const (
	// DefaultArrivalPoll and DefaultArrivalFor are how often, and for how
	// long after a take, the holder polls a device it took (SPEC §4.3).
	DefaultArrivalPoll = 1 * time.Second
	DefaultArrivalFor  = 60 * time.Second
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
// The reason distinguishes "dormant, no AC power" from "dormant, display not seen" in
// logs. Linux has no guards; macOS checks AC power and display presence (§6.2).
type Guard interface {
	OK(ctx context.Context) (ok bool, reason string)
}

// Runner executes one argv slice and reports the exit status: nil on exit 0,
// an error wrapping *exec.ExitError otherwise. It is a func type, not an
// interface: there is one implementation, and the seam the tests fake is the
// runner itself (SPEC §11.2). Child output is captured and attached to the
// error, never streamed.
type Runner func(ctx context.Context, argv []string) error

// ScreenWriter writes w to the attached monitor named name, once. It returns
// screen.ErrNoDisplay when none has the name. Implemented by screen.Write.Do;
// a func type for the reason Runner is (SPEC §11.2).
type ScreenWriter func(ctx context.Context, w screen.Write, name string) error

// DeviceSender sends the device to an Easy-Switch channel. sent is false, with
// a nil error, when the device is not linked to this host. Implemented by
// hidpp.Target.Send (SPEC §11.2).
type DeviceSender func(ctx context.Context, t hidpp.Target, channel uint8) (sent bool, err error)

// DevicePresence reports whether the device is linked to this host.
// Implemented by hidpp.Target.Present (SPEC §11.2).
type DevicePresence func(ctx context.Context, t hidpp.Target) (bool, error)

// Config is the wiring the agent loop needs. It contains no policy; that
// lives in Machine (SPEC §11.3).
type Config struct {
	ID             string
	ExplicitServer string
	KeyFP          string
	Detector       Detector
	Guard          Guard
	Client         *client.Client
	Runner         Runner
	Machine        *model.MachineConfig
	AgentStatePath string
	// OnActivation runs when this host becomes the owner, OnDeactivation when
	// it stops being the owner; each is an ordered list of argv slices
	// (SPEC §4.3, §5.4). Either may be empty. In OnActivation the argv
	// take-screen and take-hid run in-process.
	OnActivation   [][]string
	OnDeactivation [][]string
	// Screens and HIDs are this host's declarations: what take-screen and
	// take-hid post, and — for HIDs, by name — how this host reaches a device
	// another host took (SPEC §5.5, §5.6).
	Screens []screen.Declaration
	HIDs    []hidpp.Declaration
	// WriteScreen, SendHID and HIDPresent are the seams to the monitor and the
	// peripherals.
	WriteScreen   ScreenWriter
	SendHID       DeviceSender
	HIDPresent    DevicePresence
	NotifyArgv    []string
	SwitchTimeout time.Duration
	GuardPoll     time.Duration
	BackoffBase   time.Duration
	// ArrivalPoll and ArrivalFor pace the arrival watcher; zero means the
	// defaults.
	ArrivalPoll time.Duration
	ArrivalFor  time.Duration
	Resolver    *discover.Resolver
}

func (c Config) switchTimeoutOrDefault() time.Duration {
	if c.SwitchTimeout > 0 {
		return c.SwitchTimeout
	}
	return DefaultSwitchTimeout
}

func (c Config) guardPollOrDefault() time.Duration {
	if c.GuardPoll > 0 {
		return c.GuardPoll
	}
	return defaultGuardPoll
}

func (c Config) arrivalPollOrDefault() time.Duration {
	if c.ArrivalPoll > 0 {
		return c.ArrivalPoll
	}
	return DefaultArrivalPoll
}

func (c Config) arrivalForOrDefault() time.Duration {
	if c.ArrivalFor > 0 {
		return c.ArrivalFor
	}
	return DefaultArrivalFor
}

func (c Config) backoffBaseOrDefault() time.Duration {
	if c.BackoffBase > 0 {
		return c.BackoffBase
	}
	return backoffBase
}

// agent runs the connect loop until ctx is cancelled.
type agent struct {
	cfg      Config
	claims   errgroup.Group
	actionCh chan effect
	results  chan model.Event

	// hids indexes the local device declarations by name.
	hids map[string]hidpp.Declaration
	// activation and deactivation are the two command lists with their
	// progress in the current command run. Only the action worker touches them.
	activation, deactivation commands
}

// newBackoff returns a backoff using the configured base.
func (a *agent) newBackoff() *backoff {
	return newBackoffWithBase(a.cfg.backoffBaseOrDefault())
}

var errGuardsDown = errors.New("guards down")

// Run starts the connect agent and blocks until ctx is cancelled. Returns nil
// on ctx cancellation.
func Run(ctx context.Context, cfg Config) error {
	as := state.AgentState{}
	_, statErr := os.Stat(cfg.AgentStatePath)
	hasRecord := statErr == nil
	if err := state.Load(cfg.AgentStatePath, &as); err != nil {
		slog.Warn("corrupt agent state, starting fresh", "path", cfg.AgentStatePath, "error", err)
		as = state.AgentState{}
		hasRecord = false
	}

	// Invariant 1: the Machine lives on the loop goroutine, never on agent.
	m := model.NewMachine(*cfg.Machine, as, hasRecord)

	a := &agent{cfg: cfg, hids: make(map[string]hidpp.Declaration, len(cfg.HIDs))}
	for _, d := range cfg.HIDs {
		a.hids[d.Name] = d
	}
	a.activation.argv = cfg.OnActivation
	a.deactivation.argv = cfg.OnDeactivation
	a.actionCh = make(chan effect, 4)
	a.results = make(chan model.Event, 1)

	return a.run(ctx, m)
}

// run starts the action worker and claims group, then loops over guards-up
// generations until ctx is cancelled.
func (a *agent) run(ctx context.Context, m *model.Machine) error {
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
	ok, reason := a.cfg.Guard.OK(ctx)
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
		ok, reason = a.cfg.Guard.OK(ctx)
	}
	slog.Info("guards up", "reason", reason)
	return true
}

// generation runs one guards-up generation. Its five goroutines share a
// context cancelled on first error. The only non-nil errors a generation
// goroutine may return are errGuardsDown and ctx.Err(); transient failures
// (HTTP, HID) are retried in place. runCtx is the run-level context the loop
// uses for claims: a guard flap must not cancel an in-flight claim.
func (a *agent) generation(ctx context.Context, m *model.Machine) error {
	attachCh := make(chan struct{}, 1)
	stateCh := make(chan *state.ServerState, 1)
	arrivalCh := make(chan *state.ServerState, 1)

	runCtx := ctx
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.detectorLoop(ctx, attachCh) })
	g.Go(func() error { return a.watch(ctx, stateCh) })
	g.Go(func() error { return a.guardWatch(ctx) })
	g.Go(func() error { return a.arrivalWatch(ctx, arrivalCh) })
	g.Go(func() error { return a.loop(ctx, m, attachCh, stateCh, arrivalCh, runCtx) })
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
			ok, reason := a.cfg.Guard.OK(ctx)
			if !ok {
				slog.Info("guards down, tearing down generation", "reason", reason)
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
		err := a.cfg.Detector.Run(ctx, attachCh)
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
func (a *agent) loop(ctx context.Context, m *model.Machine, attachCh <-chan struct{}, stateCh <-chan *state.ServerState, arrivalCh chan *state.ServerState, runCtx context.Context) error {
	timer := stoppedTimer()
	send := func(e effect) error {
		select {
		case a.actionCh <- e:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	step := func(ev model.Event) error {
		ev.Now = time.Now() // the loop is the only clock (§11.3)
		sawWake := false
		for _, act := range m.Step(ev) {
			if act.Log != "" {
				slog.Info(act.Log)
			}
			if act.Save != nil {
				if err := state.Save(a.cfg.AgentStatePath, *act.Save); err != nil {
					slog.Error("failed to save agent state", "path", a.cfg.AgentStatePath, "error", err)
				}
			}
			if act.Claim != "" {
				if !a.claims.TryGo(func() error { a.claim(runCtx, act.Claim); return nil }) {
					slog.Debug("claim already in flight, coalesced", "id", act.Claim)
				}
			}
			if e, ok := commandEffect(act); ok {
				if err := send(e); err != nil {
					return err
				}
			}
			if len(act.Run) > 0 {
				if err := send(effect{kind: effectRun, holds: act.Run}); err != nil {
					return err
				}
			}
			if act.Notify {
				if err := send(effect{kind: effectNotify}); err != nil {
					return err
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
	if err := step(model.Event{}); err != nil {
		return err
	}
	for {
		var ev model.Event
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-attachCh:
			ev = model.Event{Attach: true}
		case s := <-stateCh:
			offerLatest(arrivalCh, s)
			ev = model.Event{State: s}
		case r := <-a.results:
			ev = r
		case <-timer.C:
		}
		if err := step(ev); err != nil {
			return err
		}
	}
}

// offerLatest hands s to the arrival watcher without ever blocking: a state
// it has not read yet is replaced, since only the newest one matters. The
// loop is the only sender, so the second send cannot lose a race.
func offerLatest(ch chan *state.ServerState, s *state.ServerState) {
	select {
	case ch <- s:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- s:
		default:
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
		for base := range a.cfg.Resolver.Resolve(cctx, a.cfg.ExplicitServer, a.cfg.KeyFP) {
			changed, err := a.cfg.Client.Claim(cctx, base, id, false, false)
			if err != nil {
				slog.Debug("claim: candidate failed", "base", base, "attempt", attempt+1, "error", err)
				continue
			}
			if changed {
				a.cfg.Resolver.Save(base)
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
