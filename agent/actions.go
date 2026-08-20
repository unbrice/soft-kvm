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

	"github.com/unbrice/soft-kvm/model"
)

// errSwitchTimeout names the class of failure where --switch-timeout kills a
// hung SWITCH-CMD. The machine's breaker counts it like any failure, but the
// log line must distinguish a hung child from a spontaneous ddcutil death.
var errSwitchTimeout = errors.New("switch timed out")

// effect is the kind of command the action worker runs. The Machine emits at
// most one of Switch/Probe/Notify per Action, so the worker's payload is just
// the kind.
type effect int

const (
	effectSwitch effect = iota
	effectProbe
	effectNotify
)

// actionLoop runs one command at a time, in order, until ctx is cancelled.
// Serial execution is safe because the Machine's mode is single-activity by
// construction: it never emits a second Switch/Probe before the result of the
// first has been processed.
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

// runSwitch runs one switch command bounded by --switch-timeout and
// distinguishes a hung child from a spontaneous non-zero exit.
func (a *agent) runSwitch(ctx context.Context, argv []string) error {
	sctx, cancel := context.WithTimeout(ctx, a.cfg.switchTimeoutOrDefault())
	err := a.cfg.Runner(sctx, argv)
	cancel()
	if errors.Is(sctx.Err(), context.DeadlineExceeded) {
		if err == nil {
			return errSwitchTimeout
		}
		return fmt.Errorf("%w: %w", errSwitchTimeout, err)
	}
	return err
}

func (a *agent) runEffect(ctx context.Context, e effect) {
	switch e {
	case effectSwitch:
		// The switch is a set of argv commands (display, then e.g. a USB
		// device) run in order; each gets its own timeout. One failure does
		// not skip the rest — the commands target independent devices and the
		// display probe remains the receipt (§4.3).
		var errs []error
		for _, argv := range a.cfg.SwitchCommands {
			errs = append(errs, a.runSwitch(ctx, argv))
		}
		err := errors.Join(errs...)
		select {
		case a.results <- model.Event{SwitchExit: &err}:
		case <-ctx.Done():
		}
	case effectProbe:
		pctx, cancel := context.WithTimeout(ctx, a.cfg.CheckTimeout)
		err := a.cfg.Runner(pctx, a.cfg.CheckArgv)
		cancel()
		select {
		case a.results <- model.Event{ProbeExit: &err}:
		case <-ctx.Done():
		}
	case effectNotify:
		if err := a.cfg.Runner(ctx, a.cfg.NotifyArgv); err != nil {
			slog.Error("notify command failed", "error", err)
		}
	}
}
