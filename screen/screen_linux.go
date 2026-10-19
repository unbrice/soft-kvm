// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_linux.go: the screen write through ddcutil, aimed by EDID name
// (SPEC §4.3, §5.6).

package screen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const backend = "ddcutil"

// drmEDIDs is where the kernel publishes the EDID of every connector. A
// variable so the tests can point it at a temp dir.
var drmEDIDs = "/sys/class/drm/*/edid"

// DisplayNames is the macOS guard's presence check. The Linux desktop has no
// guards (SPEC §6.1), so nothing calls it here.
func DisplayNames() ([]string, error) { return nil, errors.ErrUnsupported }

// attachedNames lists the EDID names of the monitors attached now. It reads
// files only — no DDC traffic — so it costs milliseconds. A connector with no
// monitor has an empty EDID, and one that does not parse is skipped.
func attachedNames() []string {
	paths, _ := filepath.Glob(drmEDIDs)
	var names []string
	for _, p := range paths {
		edid, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if _, name, ok := parseEDID(edid); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// Do writes w once to the attached monitor named name, ignoring case, and
// never reads it back: the monitor's answer is not a receipt (SPEC §4.3).
// ErrNoDisplay means no attached monitor has the name. --noverify is
// load-bearing: ddcutil's own verification fails on firmware that goes silent
// on the input it just left, so a verifying write reports failure after
// succeeding. The monitor is selected by --model rather than a bus number,
// which changes on hotplug and across reboots (SPEC §6.1).
func (w Write) Do(ctx context.Context, run Runner, name string) error {
	names := attachedNames()
	i, err := pickByName(names, name)
	if err != nil {
		return err
	}
	argv := []string{backend, "setvcp",
		fmt.Sprintf("0x%02X", w.VCP), fmt.Sprintf("0x%02X", w.Value), "--noverify"}
	if w.Source != ddcHost {
		argv = append(argv, fmt.Sprintf("--i2c-source-addr=0x%02X", w.Source))
	}
	w.mustBeValid()
	_, err = run(ctx, append(argv, "--model", names[i]))
	return err
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
