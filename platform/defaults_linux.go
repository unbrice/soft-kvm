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
	DefaultCheckCmd  = "ddcutil getvcp 60"
	DefaultNotifyCmd = "notify-send 'soft-kvm' 'Press Input on the monitor'"
)

// DefaultSwitchCmd points the LG at the Mac's input: the LG-specific VCP 0xF4
// with source address 0x50. --noverify is load-bearing, not a shortcut:
// read-back verification fails on this firmware (SPEC §5.4).
var DefaultSwitchCmd = []string{
	"ddcutil", "setvcp", "0xF4", "0xD0", "--i2c-source-addr=0x50", "--noverify",
}

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
