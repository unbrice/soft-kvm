// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Per-OS flag defaults for connect (SPEC §5.4) — macOS column.

package platform

import (
	"os"
	"path/filepath"
)

const (
	DefaultID        = "mac"
	DefaultCheckCmd  = `"/Applications/BetterDisplay.app/Contents/MacOS/BetterDisplay" get -ddc -vcp=inputSelect`
	DefaultNotifyCmd = `osascript -e 'display notification "Press Input on the monitor" with title "soft-kvm"'`
)

// DefaultSwitchCmd points the monitor at the other host's input through BetterDisplay,
// which writes the standard VCP 0x60. 15 is BetterDisplay's DisplayPort
// value; §10 records this monitor's real per-port codes, and `-- SWITCH-CMD
// ARGS...` overrides the whole command when they deviate (SPEC §5.4).
var DefaultSwitchCmd = []string{
	"/Applications/BetterDisplay.app/Contents/MacOS/BetterDisplay", "set", "-ddc",
	"-vcp=inputSelect", "-value=15",
}

// StateDir is ~/Library/Application Support/soft-kvm (SPEC §4.3).
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "soft-kvm"), nil
}
