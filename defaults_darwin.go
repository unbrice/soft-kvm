// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Per-OS flag defaults for connect (SPEC §5.4) — macOS column.

package main

import (
	"os"
	"path/filepath"
)

const (
	defaultID        = "mac"
	defaultCheckCmd  = "betterdisplaycli get -productNameLike=LG -feature=ddc -vcp=inputSelect"
	defaultNotifyCmd = `osascript -e 'display notification "Press Input on the monitor" with title "soft-kvm"'`
)

// defaultSwitchCmd points the LG at the Linux input through BetterDisplay,
// which writes the standard VCP 0x60. 15 is BetterDisplay's DisplayPort
// value; §10 records this monitor's real per-port codes, and `-- SWITCH-CMD
// ARGS...` overrides the whole command when they deviate (SPEC §5.4).
var defaultSwitchCmd = []string{
	"betterdisplaycli", "set", "-productNameLike=LG", "-feature=ddc",
	"-vcp=inputSelect", "-value=15",
}

// stateDir is ~/Library/Application Support/soft-kvm (SPEC §4.3).
func stateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "soft-kvm")
}
