// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_darwin.go: the screen write over IOAVService, in-process, aimed by
// EDID name (SPEC §4.3, §5.6).

package screen

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

const backend = "IOAVService"

// monitor is one external display: what detect shows, and the channel to it.
type monitor struct {
	Display
	svc avService
	// edidErr is why Display has no name: the EDID could not be read or does
	// not parse. Such a monitor is still attached, so the guard counts it.
	edidErr error
}

// monitors opens the external displays a write can drive, named from
// their EDID. Apple's monitors are left out, like the built-in panel: neither
// has an input to select. The caller closes them.
func monitors() ([]monitor, error) {
	svcs, err := externalServices()
	if err != nil {
		return nil, err
	}
	var ms []monitor
	for _, svc := range svcs {
		var d Display
		var edidErr error
		if edid, err := svc.edid(); err != nil {
			edidErr = fmt.Errorf("read EDID: %w", err)
		} else if mfg, model, ok := parseEDID(edid); !ok {
			edidErr = errors.New("EDID does not parse")
		} else {
			d.Mfg, d.Model = mfg, model
		}
		if d.Mfg == "APP" {
			svc.close()
			continue
		}
		ms = append(ms, monitor{d, svc, edidErr})
	}
	return ms, nil
}

func closeAll(ms []monitor) {
	for _, m := range ms {
		m.svc.close()
	}
}

// DisplayNames lists the external monitors IOAVService reaches by EDID name,
// "" where the EDID could not be read, and Apple's own displays left out as
// above. The macOS guard checks it for the shared monitor's presence (SPEC
// §6.2): an IORegistry walk and one EDID read each, no DDC/CI traffic.
func DisplayNames() ([]string, error) {
	ms, err := monitors()
	if err != nil {
		return nil, err
	}
	defer closeAll(ms)
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Model
	}
	return names, nil
}

// Do writes w once over IOAVService to the attached monitor named name,
// ignoring case, and never reads it back (SPEC §4.3). ErrNoDisplay means no
// attached monitor has the name. run is unused: the DDC/CI exchange happens
// in-process.
func (w Write) Do(ctx context.Context, _ Runner, name string) error {
	ms, err := monitors()
	if err != nil {
		return fmt.Errorf("screen write: %w", err)
	}
	defer closeAll(ms)
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Model
	}
	i, err := pickByName(names, name)
	if errors.Is(err, ErrNoDisplay) {
		// A monitor with no name might be the one: that is not "not here".
		for _, m := range ms {
			if m.edidErr != nil {
				return fmt.Errorf("screen write: cannot tell whether %q is attached: %w", name, m.edidErr)
			}
		}
	}
	if err != nil {
		return err
	}
	w.mustBeValid()
	if err := ms[i].svc.setVCP(ctx, w.Source, w.VCP, uint16(w.Value)); err != nil {
		return fmt.Errorf("screen write: %w", err)
	}
	return nil
}

// Probe reports which external monitors IOAVService reaches and the input
// each is on — this host's own port, since a monitor answers for the active
// input only.
func Probe(ctx context.Context, _ Runner) Report {
	r := Report{Backend: backend}
	ms, err := monitors()
	if err != nil {
		r.Err = err
		return r
	}
	defer closeAll(ms)
	if len(ms) == 0 {
		r.Err = errors.New("no external display has an IOAVService")
		return r
	}
	var unnamed []error
	for _, m := range ms {
		if m.edidErr != nil {
			unnamed = append(unnamed, m.edidErr)
			continue
		}
		d := m.Display
		if v, err := m.svc.getVCP(ctx, inputSourceVCP); err != nil {
			d.InputErr = err
		} else {
			d.Input, d.InputOK = uint8(v), true
		}
		r.Displays = append(r.Displays, d)
	}
	if len(r.Displays) == 0 && len(unnamed) > 0 {
		r.Err = errors.Join(unnamed...)
	}
	for _, err := range unnamed {
		slog.Warn("ignoring a monitor with no usable EDID", "error", err)
	}
	return r
}
