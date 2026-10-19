// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// worker.go: the worker that carries out decisions — commands, assignments and
// the notification (SPEC §4.3, §5.5, §5.6, §11.1).

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

// errSwitchTimeout names the class of failure where --switch-timeout kills a
// hung command or write. The decider counts it like any failure, but the log
// line must distinguish a hung child from a spontaneous ddcutil death.
var errSwitchTimeout = errors.New("switch timed out")

// hasWork reports whether d is for the worker: commands, assignments or a
// notification.
func hasWork(d model.Decision) bool {
	return len(d.Run) > 0 || len(d.GiveAway) > 0 || d.Notify
}

// workerLoop carries out one decision at a time, in order, until ctx is
// cancelled. Serial execution is safe because the Decider has one issue
// outstanding at a time: it never issues more work before the result of the
// last has been processed.
func (a *agent) workerLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d := <-a.decisions:
			a.carryOut(ctx, d)
		}
	}
}

func (a *agent) carryOut(ctx context.Context, d model.Decision) {
	if len(d.Run) > 0 {
		a.runCommands(ctx, d.Run)
	}
	if len(d.GiveAway) > 0 {
		a.giveAway(ctx, d.GiveAway)
	}
	if d.Notify {
		if err := a.cfg.Runner(ctx, a.cfg.NotifyArgv); err != nil {
			slog.Error("notify command failed", "error", err)
		}
	}
}

// post reports a result to the loop as an ordinary observation.
func (a *agent) post(ctx context.Context, o model.Observation) {
	select {
	case a.results <- o:
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
func (a *agent) runCommand(ctx context.Context, c model.Command) error {
	switch {
	case c.TakeScreen != 0:
		return a.take(ctx, c.TakeScreen, a.screenTakes())
	case c.TakeHID != 0:
		return a.take(ctx, c.TakeHID, a.hidTakes(ctx))
	}
	return a.cfg.Runner(ctx, c.Argv)
}

// runCommands runs commands in order, each with its own timeout, and reports
// one result per command for the decider. One failure does not skip the rest:
// the commands target independent things.
func (a *agent) runCommands(ctx context.Context, cmds []model.Command) {
	results := make([]error, len(cmds))
	for i, c := range cmds {
		results[i] = a.bounded(ctx, func(ctx context.Context) error { return a.runCommand(ctx, c) })
		if results[i] != nil {
			slog.Warn("command failed", "command", c.String(), "error", results[i])
		}
	}
	a.post(ctx, model.Observation{Ran: &model.CommandsRun{Results: results}})
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

// giveAway gives away a batch of assignments, in order, each write bounded by
// --switch-timeout. One failing does not skip the rest: they target
// independent devices.
func (a *agent) giveAway(ctx context.Context, assignments []state.Assignment) {
	results := make(map[state.AssignmentKey]error, len(assignments))
	for _, h := range assignments {
		results[h.Key()] = a.giveAwayAssignment(ctx, h)
	}
	a.post(ctx, model.Observation{GaveAway: &model.AssignmentsGivenAway{Results: results}})
}

// giveAwayAssignment moves one thing on this host to its assignee: a screen is
// written to the monitor of that name, a device is sent to the assignee's
// channel. A host with nothing of that name, or the device not linked here, has
// nothing to move and returns model.ErrNotHere (SPEC §4.3).
func (a *agent) giveAwayAssignment(ctx context.Context, h state.Assignment) error {
	start := time.Now()
	var err error
	switch h.Kind {
	case state.KindScreen:
		err = a.giveScreen(ctx, h)
	case state.KindHID:
		err = a.giveDevice(ctx, h)
	default:
		err = fmt.Errorf("unknown kind %q", h.Kind)
	}
	switch {
	case errors.Is(err, model.ErrNotHere):
		slog.Info("nothing to move here", "assignment", h.Key().String())
	case err != nil:
		slog.Warn("giving away failed", "assignment", h.Key().String(), "error", err)
	default:
		slog.Info("given away", "assignment", h.Key().String(), "assignee", h.Assignee, "took", time.Since(start).Round(time.Millisecond))
	}
	return err
}

func (a *agent) giveScreen(ctx context.Context, h state.Assignment) error {
	w, err := screen.ParseWrite(h.Input)
	if err != nil {
		return err
	}
	if a.cfg.WriteScreen == nil {
		return errors.New("no screen backend configured")
	}
	err = a.bounded(ctx, func(ctx context.Context) error { return a.cfg.WriteScreen(ctx, w, h.Name) })
	if errors.Is(err, screen.ErrNoDisplay) {
		return model.ErrNotHere
	}
	return err
}

func (a *agent) giveDevice(ctx context.Context, h state.Assignment) error {
	d, ok := a.hids[h.Name]
	if !ok {
		return model.ErrNotHere
	}
	if a.cfg.SendHID == nil {
		return errors.New("no device backend configured")
	}
	var sent bool
	err := a.bounded(ctx, func(ctx context.Context) error {
		var err error
		sent, err = a.cfg.SendHID(ctx, d.Target, h.Channel)
		return err
	})
	if err == nil && !sent {
		return model.ErrNotHere
	}
	return err
}
