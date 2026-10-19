// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_linux.go: screen-switch through ddcutil — write, then confirm the
// monitor left this input (SPEC §4.3, §5.6).

package screen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

const backend = "ddcutil"

// DisplayNames is the macOS guard's presence check. The Linux desktop has no
// guards (SPEC §6.1), so nothing calls it here.
func DisplayNames() ([]string, error) { return nil, errors.ErrUnsupported }

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
	return s.confirm(ctx, func(ctx context.Context) (uint8, bool, error) {
		out, err := run(ctx, read)
		if err != nil {
			return 0, false, err
		}
		v, ok := parseInputSource(out)
		return v, ok, nil
	})
}

// unreachableErr says why no monitor answers DDC although ddcutil exited 0
// and found some by EDID: it reports a bus it may not open on its output only.
func unreachableErr(out []byte, unreachable []Display) error {
	if bytes.Contains(out, []byte("EACCES")) {
		return errors.New("ddcutil: permission denied on /dev/i2c-*.\n" +
			"Add this user to the i2c group: https://www.ddcutil.com/i2c_permissions")
	}
	models := make([]string, len(unreachable))
	for i, d := range unreachable {
		models[i] = d.Model
	}
	return fmt.Errorf("%s does not answer DDC/CI: it is showing another input,\n"+
		"or DDC/CI is off in its menu", strings.Join(models, ", "))
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
	r.Displays, r.Unreachable = parseDetect(out)
	if len(r.Displays) == 0 && len(r.Unreachable) > 0 {
		r.Err = unreachableErr(out, r.Unreachable)
	}
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
