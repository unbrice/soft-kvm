// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_darwin.go: screen-switch through BetterDisplay — a fire-and-forget
// write, since DDC reads are unreliable from macOS (SPEC §5.6, §6.2).

package screen

import (
	"context"
	"fmt"
)

const backend = "/Applications/BetterDisplay.app/Contents/MacOS/BetterDisplay"

// vendorTables is empty: BetterDisplay takes the standard Input Source value
// and does any per-monitor translation itself.
var vendorTables []*Table

// Do writes the input through BetterDisplay. Exit 0 is the only receipt
// there is: a non-zero exit is a switch that did not land.
func (s *Switch) Do(ctx context.Context, run Runner) error {
	argv := []string{backend, "set", "-ddc", "-vcp=inputSelect", fmt.Sprintf("-value=%d", s.Value)}
	if s.Display != "" {
		argv = append(argv, "-nameLike="+s.Display)
	}
	_, err := run(ctx, argv)
	return err
}

// Probe reports whether BetterDisplay answers. It names no monitor: nothing
// here reads the manufacturer or the current input.
func Probe(ctx context.Context, run Runner) Report {
	_, err := run(ctx, []string{backend, "get", "-identifiers"})
	return Report{Backend: "BetterDisplay", Err: err}
}
