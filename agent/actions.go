// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// actions.go: the action worker (SPEC §4.3, §11.1).

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/unbrice/soft-kvm/hidpp"
	"github.com/unbrice/soft-kvm/model"
	"github.com/unbrice/soft-kvm/screen"
)

// errSwitchTimeout names the class of failure where --switch-timeout kills a
// hung command. The machine's breaker counts it like any failure, but the
// log line must distinguish a hung child from a spontaneous ddcutil death.
var errSwitchTimeout = errors.New("switch timed out")

// effect is the kind of command the action worker runs. The Machine emits at
// most one of Activate/Deactivate/Notify per Action, so the worker's payload is
// just the kind.
type effect int

const (
	effectDeactivate      effect = iota // a fresh sequence: run every --on-deactivation command
	effectDeactivateRetry               // a retry: run only the commands that have not succeeded yet
	effectActivate                      // the same, for --on-activation
	effectActivateRetry
	effectNotify
)

// commands is one side's ordered argv list and which of them already exited 0
// in the sequence in progress. Only the action worker touches it.
type commands struct {
	argv [][]string
	done []bool
}

// commandEffect maps an Action's Activate or Deactivate to the effect the
// worker runs; ok is false for an action that runs neither.
func commandEffect(act model.Action) (e effect, ok bool) {
	switch {
	case act.Activate && act.Retry:
		return effectActivateRetry, true
	case act.Activate:
		return effectActivate, true
	case act.Deactivate && act.Retry:
		return effectDeactivateRetry, true
	case act.Deactivate:
		return effectDeactivate, true
	}
	return 0, false
}

// actionLoop runs one command at a time, in order, until ctx is cancelled.
// Serial execution is safe because the Machine's mode is single-activity by
// construction: it never emits a second Switch before the result of the first
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

// runSwitch runs one command bounded by --switch-timeout and distinguishes a
// hung child from a spontaneous non-zero exit.
func (a *agent) runSwitch(ctx context.Context, argv []string) error {
	sctx, cancel := context.WithTimeout(ctx, a.cfg.switchTimeoutOrDefault())
	err := a.runSwitchCommand(sctx, argv, uint8(a.ownerChannel.Load()))
	cancel()
	if errors.Is(sctx.Err(), context.DeadlineExceeded) {
		if err == nil {
			return errSwitchTimeout
		}
		return fmt.Errorf("%w: %w", errSwitchTimeout, err)
	}
	return err
}

// runSwitchCommand runs one command: the built-in virtual commands in-process —
// hid-switch (SPEC §5.5) and screen-switch (§5.6) — anything else as an
// external argv. fallbackChannel is the owner's published Easy-Switch channel,
// used when a hid-switch carries no explicit host=N.
func (a *agent) runSwitchCommand(ctx context.Context, argv []string, fallbackChannel uint8) error {
	switch argv[0] {
	case "hid-switch":
		sw, err := hidpp.Parse(argv[1:])
		if err != nil {
			return err
		}
		return sw.Do(ctx, fallbackChannel)
	case "screen-switch":
		sw, err := screen.Parse(argv[1:])
		if err != nil {
			return err
		}
		return sw.Do(ctx, a.cfg.ScreenRunner)
	}
	return a.cfg.Runner(ctx, argv)
}

// runCommands runs one side's commands in order, each with its own timeout,
// and reports the joined result for the machine. One failure does not skip the
// rest — the commands target independent devices — and a retry re-runs only
// what failed, so a peripheral that already left is not chased again (§4.3).
// Exit 0 from all of them is the receipt.
func (a *agent) runCommands(ctx context.Context, c *commands, retry bool) {
	if !retry || len(c.done) != len(c.argv) {
		c.done = make([]bool, len(c.argv))
	}
	var errs []error
	for i, argv := range c.argv {
		if c.done[i] {
			continue
		}
		err := a.runSwitch(ctx, argv)
		c.done[i] = err == nil
		errs = append(errs, err)
	}
	err := errors.Join(errs...)
	select {
	case a.results <- model.Event{SwitchExit: &err}:
	case <-ctx.Done():
	}
}

func (a *agent) runEffect(ctx context.Context, e effect) {
	switch e {
	case effectDeactivate, effectDeactivateRetry:
		a.runCommands(ctx, &a.deactivation, e == effectDeactivateRetry)
	case effectActivate, effectActivateRetry:
		a.runCommands(ctx, &a.activation, e == effectActivateRetry)
	case effectNotify:
		if err := a.cfg.Runner(ctx, a.cfg.NotifyArgv); err != nil {
			slog.Error("notify command failed", "error", err)
		}
	}
}
