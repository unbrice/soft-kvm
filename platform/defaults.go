// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// defaults.go: flag defaults shared by both OSes (SPEC §5.2).

package platform

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultServeStatePath is the serve --state default (SPEC §5.2):
// $STATE_DIRECTORY/state.json when systemd hands the unit a StateDirectory
// (created with the right owner whatever User= the unit runs as),
// StateDir/state.json otherwise — launchd sets no equivalent, so macOS
// always lands on StateDir.
func DefaultServeStatePath() (string, error) {
	if dirs := os.Getenv("STATE_DIRECTORY"); dirs != "" {
		dir, _, _ := strings.Cut(dirs, ":") // one per StateDirectory= entry
		return filepath.Join(dir, "state.json"), nil
	}
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}
