// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// defaults.go: flag defaults shared by both OSes (SPEC §5.2).

package platform

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// ProgName is how to name this binary in messages: "go run ." when an
// unversioned build starts in the source directory, "soft-kvm" otherwise.
var ProgName = progName()

func progName() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Path != "" && info.Main.Version == "(devel)" {
		if b, err := os.ReadFile("go.mod"); err == nil && strings.Contains(string(b), "module "+info.Main.Path+"\n") {
			return "go run ."
		}
	}
	return "soft-kvm"
}

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
