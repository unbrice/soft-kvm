// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_darwin.go: screen-switch over IOAVService, in-process — write, then
// confirm the monitor left this input, as on Linux (SPEC §4.3, §5.6).

package screen

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const backend = "IOAVService"

// monitor is one external display: what detect shows, and the channel to it.
type monitor struct {
	Display
	svc avService
}

// monitors opens the external displays screen-switch can drive, named from
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
		if edid, err := svc.edid(); err == nil {
			d.Mfg, d.Model, _ = parseEDID(edid)
		}
		if d.Mfg == "APP" {
			svc.close()
			continue
		}
		ms = append(ms, monitor{d, svc})
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

// pick finds the one monitor display=MATCH names — a case-insensitive
// substring of its EDID name — or the only one when no MATCH is given.
func (s *Switch) pick(ms []monitor) (monitor, error) {
	var found []monitor
	var names []string
	for _, m := range ms {
		names = append(names, m.Model)
		if strings.Contains(strings.ToLower(m.Model), strings.ToLower(s.Display)) {
			found = append(found, m)
		}
	}
	switch {
	case len(ms) == 0:
		return monitor{}, errors.New("screen-switch: no external display has an IOAVService")
	case len(found) == 1:
		return found[0], nil
	case len(found) == 0:
		return monitor{}, fmt.Errorf("screen-switch: no display matches %q among %s",
			s.Display, strings.Join(names, ", "))
	default:
		return monitor{}, fmt.Errorf("screen-switch: %d displays match %q (%s): name one with display=MATCH",
			len(found), s.Display, strings.Join(names, ", "))
	}
}

// Do writes the input over IOAVService and reports whether the monitor moved,
// by the same receipt as on Linux: this host's own read fails, or reports the
// target, once the switch landed. run is unused: the DDC/CI exchange happens
// in-process.
func (s *Switch) Do(ctx context.Context, _ Runner) error {
	ms, err := monitors()
	if err != nil {
		return fmt.Errorf("screen-switch: %w", err)
	}
	defer closeAll(ms)
	m, err := s.pick(ms)
	if err != nil {
		return err
	}
	src := s.Table.Source
	if src == 0 {
		src = ddcHost
	}
	if err := m.svc.setVCP(ctx, src, s.Table.VCP, uint16(s.Value)); err != nil {
		return fmt.Errorf("screen-switch: %w", err)
	}
	if err := sleep(ctx, ddcDelay); err != nil {
		return err
	}
	return s.confirm(ctx, func(ctx context.Context) (uint8, bool, error) {
		v, err := m.svc.getVCP(ctx, inputSourceVCP)
		if errors.Is(err, errNoReply) {
			return 0, false, err
		}
		return uint8(v), err == nil, nil
	})
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
	for _, m := range ms {
		d := m.Display
		if v, err := m.svc.getVCP(ctx, inputSourceVCP); err == nil {
			d.Input, d.InputOK = uint8(v), true
		}
		r.Displays = append(r.Displays, d)
	}
	return r
}
