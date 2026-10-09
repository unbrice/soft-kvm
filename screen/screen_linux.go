// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_linux.go: screen-switch through ddcutil — write, then confirm the
// monitor left this input (SPEC §4.3, §5.6).

package screen

import (
	"context"
	"fmt"
	"time"
)

// The confirm window: how long the monitor may keep answering on this input
// after the write before the switch counts as failed, and the poll spacing.
// Variables so the tests can shrink them.
var (
	confirmWindow = 4 * time.Second
	confirmPoll   = 500 * time.Millisecond
)

const backend = "ddcutil"

// vendorTables are the monitors that do not take the standard write from
// ddcutil. lg is LG's private side channel: firmware that ignores 0x60 writes
// honours 0xF4 sent from source address 0x50 (SPEC §5.6).
var vendorTables = []*Table{
	{
		Name:   "lg",
		VCP:    0xF4,
		Source: 0x50,
		Mfg:    []string{"GSM"},
		Inputs: map[string]uint8{
			"dp1": 0xD0, "usbc1": 0xD1, "hdmi1": 0x90, "hdmi2": 0x91,
		},
		// It reports the standard values in Input Source: measured on dp1,
		// assumed for the other ports.
		Reads: standard.Inputs,
	},
}

func (s *Switch) selector() []string {
	if s.Display == "" {
		return nil
	}
	return []string{"--model", s.Display}
}

// Do writes the input and reports whether the monitor moved. The write is
// unacknowledged in practice and --noverify is load-bearing — read-back
// verification fails on firmware that goes silent on the inactive input — so
// the receipt is this host's own read: it fails, or reports the target, once
// the switch landed. A non-nil error is a switch that did not land.
func (s *Switch) Do(ctx context.Context, run Runner) error {
	argv := []string{backend, "setvcp",
		fmt.Sprintf("0x%02X", s.Table.VCP), fmt.Sprintf("0x%02X", s.Value), "--noverify"}
	if s.Table.Source != 0 {
		argv = append(argv, fmt.Sprintf("--i2c-source-addr=0x%02X", s.Table.Source))
	}
	if _, err := run(ctx, append(argv, s.selector()...)); err != nil {
		return err
	}

	read := append([]string{backend, "--terse", "getvcp", "60"}, s.selector()...)
	deadline := time.Now().Add(confirmWindow)
	for {
		out, err := run(ctx, read)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil // DDC went silent: this input is no longer the active one
		}
		if v, ok := parseInputSource(out); ok && s.ExpectOK && v == s.Expect {
			// The monitor shows the target and still answers: it answers on
			// every input, or the target is this host's own port.
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("screen-switch: monitor still on this input %v after the write", confirmWindow)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(confirmPoll):
		}
	}
}

// Probe reports whether ddcutil reaches a monitor, which, and the input each
// is on — this host's own port, since a monitor answers for the active input.
func Probe(ctx context.Context, run Runner) Report {
	r := Report{Backend: backend}
	out, err := run(ctx, []string{backend, "detect", "--terse"})
	if err != nil {
		r.Err = err
		return r
	}
	r.Displays = parseDetect(out)
	for i := range r.Displays {
		d := &r.Displays[i]
		out, err := run(ctx, []string{backend, "--terse", "getvcp", "60", "--model", d.Model})
		if err != nil {
			continue
		}
		d.Input, d.InputOK = parseInputSource(out)
	}
	return r
}
