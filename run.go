// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// run.go: the argv runner (SPEC §11.2) and the §11.1 child-process
// conventions.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Runner executes one argv slice and reports the exit status: nil on exit 0,
// an error wrapping *exec.ExitError otherwise. It is a func type, not an
// interface: there is one implementation, and the seam the tests fake is the
// runner itself (SPEC §11.2). Child output is captured and attached to the
// error, never streamed.
type Runner func(ctx context.Context, argv []string) error

// commandContext builds a *exec.Cmd with the §11.1 conventions: cancel sends
// SIGTERM first (a SIGKILL mid-I2C transaction is the fallback, not the first
// move), and WaitDelay bounds how long SIGKILL is withheld.
func commandContext(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Cancel = func() error {
		err := cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// run is the real Runner.
func run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty argv")
	}
	cmd := commandContext(ctx, argv)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(out.String()); tail != "" {
			return fmt.Errorf("%s: %w: %s", argv[0], err, tail)
		}
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil
}

// shellArgv wraps a --check-cmd / --notify-cmd flag string for `sh -c` — those
// defaults carry shell quoting, so they go through a shell. The trailing
// SWITCH-CMD is the opposite: an argv slice, never a shell string (SPEC §9).
func shellArgv(cmdline string) []string {
	return []string{"sh", "-c", cmdline}
}
