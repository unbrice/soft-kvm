// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// run.go: the argv runner (SPEC §11.2) and the §11.1 child-process
// conventions.

package platform

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

// CommandContext builds a *exec.Cmd with the §11.1 conventions: cancel sends
// SIGTERM first (a SIGKILL mid-I2C transaction is the fallback, not the first
// move), and WaitDelay bounds how long SIGKILL is withheld.
func CommandContext(ctx context.Context, argv []string) *exec.Cmd {
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

// Run executes one argv slice and reports the exit status: nil on exit 0,
// an error wrapping *exec.ExitError otherwise. Child output is captured and
// attached to the error, never streamed.
func Run(ctx context.Context, argv []string) error {
	_, err := Output(ctx, argv)
	return err
}

// Output is Run for callers that read what the child printed: it returns the
// combined stdout and stderr alongside the same error.
func Output(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty argv")
	}
	cmd := CommandContext(ctx, argv)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(out.String()); tail != "" {
			return out.Bytes(), fmt.Errorf("%s: %w: %s", argv[0], err, tail)
		}
		return out.Bytes(), fmt.Errorf("%s: %w", argv[0], err)
	}
	return out.Bytes(), nil
}

// ShellArgv wraps the --notify-cmd flag string for `sh -c` — its per-OS
// defaults carry shell quoting, so it runs through a shell. The trailing
// SWITCH-CMD is the opposite: an argv slice, never a shell string (SPEC §9).
func ShellArgv(cmdline string) []string {
	return []string{"sh", "-c", cmdline}
}
