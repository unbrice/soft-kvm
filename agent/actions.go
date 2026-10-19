// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// actions.go: the action worker — commands, takes and the notification
// (SPEC §4.3, §5.5, §5.6, §11.1).

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/model"
	"github.com/unbrice/soft-kvm/screen"
	"github.com/unbrice/soft-kvm/state"
)

// Built-in -A commands: run in-process, not exec'd (SPEC §5.4).
const (
	TakeScreen = "take-screen"
	TakeHID    = "take-hid"
)

// errSwitchTimeout names the class of failure where --switch-timeout kills a
// hung command or write. The machine counts it like any failure, but the log
// line must distinguish a hung child from a spontaneous ddcutil death.
var errSwitchTimeout = errors.New("switch timed out")

// effectKind is the kind of work the action worker runs.
type effectKind int

const (
	effectDeactivate effectKind = iota // run the --on-deactivation commands
	effectActivate                     // run the --on-activation commands
	effectRun                          // run a batch of takes
	effectNotify
)

// effect is one unit of work for the action worker. The Machine emits at most
// one of Activate, Deactivate and Run per Action, so a worker never holds two
// runs.
type effect struct {
	kind effectKind
	// retry re-runs only the commands that have not succeeded yet.
	retry bool
	// activation is the activation take-screen and take-hid post under.
	activation int64
	// holds are the takes of an effectRun.
	holds []state.Hold
}

// commands is one side's ordered argv list and which of them already exited 0
// in the command run in progress. Only the action worker touches it.
type commands struct {
	argv [][]string
	done []bool
}

// commandEffect maps an Action's Activate or Deactivate to the effect the
// worker runs; ok is false for an action that runs neither.
func commandEffect(act model.Action) (e effect, ok bool) {
	switch {
	case act.Activate:
		return effect{kind: effectActivate, retry: act.Retry, activation: act.Activation}, true
	case act.Deactivate:
		return effect{kind: effectDeactivate, retry: act.Retry}, true
	}
	return effect{}, false
}

// actionLoop runs one effect at a time, in order, until ctx is cancelled.
// Serial execution is safe because the Machine runs one command run or take
// batch at a time: it never emits a second run before the result of the first
// has been processed.
func (a *agent) actionLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-a.actionCh:
			a.runEffect(ctx, e)
		}
	}
}

func (a *agent) runEffect(ctx context.Context, e effect) {
	switch e.kind {
	case effectDeactivate:
		a.runCommands(ctx, &a.deactivation, e)
	case effectActivate:
		a.runCommands(ctx, &a.activation, e)
	case effectRun:
		a.runTakes(ctx, e.holds)
	case effectNotify:
		if err := a.cfg.Runner(ctx, a.cfg.NotifyArgv); err != nil {
			slog.Error("notify command failed", "error", err)
		}
	}
}

// post reports a result to the loop as an ordinary event.
func (a *agent) post(ctx context.Context, x model.Exit) {
	select {
	case a.results <- model.Event{Exit: &x}:
	case <-ctx.Done():
	}
}

// bounded runs fn under --switch-timeout and distinguishes a hung child from a
// spontaneous failure.
func (a *agent) bounded(ctx context.Context, fn func(context.Context) error) error {
	sctx, cancel := context.WithTimeout(ctx, a.cfg.switchTimeoutOrDefault())
	err := fn(sctx)
	cancel()
	if errors.Is(sctx.Err(), context.DeadlineExceeded) {
		if err == nil {
			return errSwitchTimeout
		}
		return fmt.Errorf("%w: %w", errSwitchTimeout, err)
	}
	return err
}

// runCommand runs one command: take-screen and take-hid in-process, anything
// else as an external argv.
func (a *agent) runCommand(ctx context.Context, argv []string, activation int64) error {
	switch argv[0] {
	case TakeScreen:
		return a.take(ctx, activation, a.screenTakes())
	case TakeHID:
		return a.take(ctx, activation, a.hidTakes(ctx))
	}
	return a.cfg.Runner(ctx, argv)
}

// runCommands runs one side's commands in order, each with its own timeout,
// and reports the joined result for the machine. One failure does not skip the
// rest — the commands target independent things — and a retry re-runs only
// what failed, so a take the server already holds is not posted again (§4.3).
func (a *agent) runCommands(ctx context.Context, c *commands, e effect) {
	if !e.retry || len(c.done) != len(c.argv) {
		c.done = make([]bool, len(c.argv))
	}
	var errs []error
	for i, argv := range c.argv {
		if c.done[i] {
			continue
		}
		err := a.bounded(ctx, func(ctx context.Context) error { return a.runCommand(ctx, argv, e.activation) })
		c.done[i] = err == nil
		if err != nil {
			slog.Warn("command failed", "argv", argv, "error", err)
		}
		errs = append(errs, err)
	}
	a.post(ctx, model.Exit{Err: errors.Join(errs...)})
}

// screenTakes is what take-screen posts: for each declaration, the write that
// selects this host's input (SPEC §5.6).
func (a *agent) screenTakes() []state.Take {
	takes := make([]state.Take, len(a.cfg.Screens))
	for i, d := range a.cfg.Screens {
		takes[i] = state.Take{Kind: state.KindScreen, Name: d.Name, Input: d.Write.String()}
	}
	return takes
}

// hidTakes is what take-hid posts: for each declaration with a channel, the
// channel this host occupies and whether the device is linked here already. A
// presence check that fails reads as absent — the arrival watcher reports it
// later (SPEC §4.3).
func (a *agent) hidTakes(ctx context.Context) []state.Take {
	var takes []state.Take
	for _, d := range a.cfg.HIDs {
		if d.Channel == 0 {
			continue
		}
		arrived := false
		if a.cfg.HIDPresent != nil {
			var err error
			if arrived, err = a.cfg.HIDPresent(ctx, d.Target); err != nil {
				slog.Warn("could not tell whether the device is here", "device", d.Name, "error", err)
				arrived = false
			}
		}
		takes = append(takes, state.Take{Kind: state.KindHID, Name: d.Name, Channel: d.Channel, Arrived: arrived})
	}
	return takes
}

// take posts takes to the server under activation. A 409 is a failure like
// any other: this host stopped being the owner while its commands ran.
func (a *agent) take(ctx context.Context, activation int64, takes []state.Take) error {
	return a.eachServer(ctx, func(ctx context.Context, base string) error {
		return a.cfg.Client.Take(ctx, base, a.cfg.ID, activation, takes)
	})
}

// eachServer calls fn against the candidates the resolver yields, the way a
// claim does, until one answers. An answer that is a refusal ends it: another
// candidate would be the same server.
func (a *agent) eachServer(ctx context.Context, fn func(ctx context.Context, base string) error) error {
	var last error
	for base := range a.cfg.Resolver.Resolve(ctx, a.cfg.ExplicitServer, a.cfg.KeyFP) {
		err := fn(ctx, base)
		if err == nil {
			a.cfg.Resolver.Save(base)
			return nil
		}
		if errors.Is(err, client.ErrNotOwner) {
			return err
		}
		slog.Debug("candidate failed", "base", base, "error", err)
		last = err
	}
	if last == nil {
		last = errors.New("no server candidate answered")
	}
	return last
}

// runTakes runs a batch of takes, in order, each write bounded by
// --switch-timeout. One failing does not skip the rest: they target
// independent devices.
func (a *agent) runTakes(ctx context.Context, holds []state.Hold) {
	out := make(map[state.HoldKey]model.Outcome, len(holds))
	for _, h := range holds {
		out[h.Key()] = a.runTake(ctx, h)
	}
	a.post(ctx, model.Exit{Takes: out})
}

// runTake moves one thing on this host: a screen is written to the monitor of
// that name, a device is sent to the holder's channel. A host with nothing of
// that name, or the device not linked here, has nothing to move (SPEC §4.3).
func (a *agent) runTake(ctx context.Context, h state.Hold) model.Outcome {
	start := time.Now()
	var out model.Outcome
	switch h.Kind {
	case state.KindScreen:
		out = a.takeScreen(ctx, h)
	case state.KindHID:
		out = a.takeDevice(ctx, h)
	default:
		out = model.Outcome{Err: fmt.Errorf("unknown kind %q", h.Kind)}
	}
	switch {
	case out.Err != nil:
		slog.Warn("take failed", "take", h.Key().String(), "error", out.Err)
	case out.NotHere:
		slog.Info("nothing to move here", "take", h.Key().String())
	default:
		slog.Info("take done", "take", h.Key().String(), "holder", h.Holder, "took", time.Since(start).Round(time.Millisecond))
	}
	return out
}

func (a *agent) takeScreen(ctx context.Context, h state.Hold) model.Outcome {
	w, err := screen.ParseWrite(h.Input)
	if err != nil {
		return model.Outcome{Err: err}
	}
	if a.cfg.WriteScreen == nil {
		return model.Outcome{Err: errors.New("no screen backend configured")}
	}
	err = a.bounded(ctx, func(ctx context.Context) error { return a.cfg.WriteScreen(ctx, w, h.Name) })
	if errors.Is(err, screen.ErrNoDisplay) {
		return model.Outcome{NotHere: true}
	}
	return model.Outcome{Err: err}
}

func (a *agent) takeDevice(ctx context.Context, h state.Hold) model.Outcome {
	d, ok := a.hids[h.Name]
	if !ok {
		return model.Outcome{NotHere: true}
	}
	if a.cfg.SendHID == nil {
		return model.Outcome{Err: errors.New("no device backend configured")}
	}
	var sent bool
	err := a.bounded(ctx, func(ctx context.Context) error {
		var err error
		sent, err = a.cfg.SendHID(ctx, d.Target, h.Channel)
		return err
	})
	switch {
	case err != nil:
		return model.Outcome{Err: err}
	case !sent:
		return model.Outcome{NotHere: true}
	}
	return model.Outcome{}
}
