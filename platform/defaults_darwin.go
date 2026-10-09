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
	DefaultNotifyCmd = `osascript -e 'display notification "Press Input on the monitor" with title "soft-kvm"'`
)

// StateDir is ~/Library/Application Support/soft-kvm (SPEC §4.3).
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "soft-kvm"), nil
}
