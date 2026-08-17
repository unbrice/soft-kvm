// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// actions.go: the action worker (SPEC §4.3, §11.1).

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

func (a *agent) runEffect(ctx context.Context, e effect) {
	switch e {
	case effectSwitch:
		sctx, cancel := context.WithTimeout(ctx, a.cfg.switchTimeoutOrDefault())
		err := a.cfg.runner(sctx, a.cfg.switchArgv)
		cancel()
		if errors.Is(sctx.Err(), context.DeadlineExceeded) {
			if err == nil {
				err = errSwitchTimeout
			} else {
				err = fmt.Errorf("%w: %w", errSwitchTimeout, err)
			}
		}
		select {
		case a.results <- Event{SwitchExit: &err}:
		case <-ctx.Done():
		}
	case effectProbe:
		pctx, cancel := context.WithTimeout(ctx, a.cfg.checkTimeout)
		err := a.cfg.runner(pctx, a.cfg.checkArgv)
		cancel()
		select {
		case a.results <- Event{ProbeExit: &err}:
		case <-ctx.Done():
		}
	case effectNotify:
		if err := a.cfg.runner(ctx, a.cfg.notifyArgv); err != nil {
			slog.Error("notify command failed", "error", err)
		}
	}
}
