// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Per-OS flag defaults for connect (SPEC §5.4) — Linux column.

package platform

import (
	"os"
	"path/filepath"
)

const (
	DefaultID        = "linux"
	DefaultNotifyCmd = "notify-send 'soft-kvm' 'Press Input on the monitor'"
)

// StateDir is $XDG_STATE_HOME/soft-kvm, defaulting to ~/.local/state/soft-kvm.
func StateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "soft-kvm"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "soft-kvm"), nil
}
