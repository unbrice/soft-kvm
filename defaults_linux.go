// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Per-OS flag defaults for connect (SPEC §5.4) — Linux column.

package main

import (
	"os"
	"path/filepath"
)

const (
	defaultID        = "linux"
	defaultCheckCmd  = "ddcutil getvcp 60"
	defaultNotifyCmd = "notify-send 'soft-kvm' 'Press Input on the monitor'"

	// btFallbackOK reports whether the --bt-mac fallback detector exists on
	// this OS (SPEC §6.3: Linux only).
	btFallbackOK = true
)

// defaultSwitchCmd points the LG at the Mac's input: the LG-specific VCP 0xF4
// with source address 0x50. --noverify is load-bearing, not a shortcut:
// read-back verification fails on this firmware (SPEC §5.4).
var defaultSwitchCmd = []string{
	"ddcutil", "setvcp", "0xF4", "0xD0", "--i2c-source-addr=0x50", "--noverify",
}

// stateDir is $XDG_STATE_HOME/soft-kvm, defaulting to ~/.local/state/soft-kvm.
func stateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "soft-kvm")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "soft-kvm")
}

// pickDetector selects the BlueZ fallback when --bt-mac is given, else the
// netlink USB detector (SPEC §6.1, §6.3).
func pickDetector(btMac, usb string) (Detector, error) {
	if btMac != "" {
		return newBTDetector(btMac), nil
	}
	return newUSBDetector(usb)
}

// newGuard: the always-on desktop has no guards (SPEC §6.1).
func newGuard(string) Guard { return alwaysOK{reason: "no guards"} }
