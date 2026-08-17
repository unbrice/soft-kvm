// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// HID attach detector (Linux and macOS): HID device add/remove events via
// github.com/telesma-app/hid — purego FFI, no cgo, no forked processes.
// Matches any of the configured VID:PID targets: the USB receiver and,
// optionally, a Bluetooth keyboard (HID over BT surfaces like any HID
// device on both OSes) (SPEC §6.1, §6.3).

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/telesma-app/hid"
)

type vidpid struct{ vid, pid uint16 }

// parseVIDPIDList parses a comma-separated "046d:c548,046d:b35b" list.
func parseVIDPIDList(list string) ([]vidpid, error) {
	var out []vidpid
	for _, s := range strings.Split(list, ",") {
		parts := strings.Split(strings.TrimSpace(s), ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid VID:PID %q", s)
		}
		v, err := strconv.ParseUint(parts[0], 16, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid VID %q: %w", parts[0], err)
		}
		p, err := strconv.ParseUint(parts[1], 16, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid PID %q: %w", parts[1], err)
		}
		out = append(out, vidpid{uint16(v), uint16(p)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty VID:PID list")
	}
	return out, nil
}

func newHIDDetector(list string) (Detector, error) {
	targets, err := parseVIDPIDList(list)
	if err != nil {
		return nil, err
	}
	return &hidDetector{targets: targets}, nil
}

type hidDetector struct {
	targets []vidpid
}

func (d *hidDetector) matches(info *hid.DeviceInfo) bool {
	if info == nil {
		return false
	}
	for _, t := range d.targets {
		if info.VendorID == t.vid && info.ProductID == t.pid {
			return true
		}
	}
	return false
}

// Run watches HID device events until ctx is cancelled, emitting one attach
// edge per absent→present transition of the targets. Returns nil on ctx
// cancellation.
func (d *hidDetector) Run(ctx context.Context, attach chan<- struct{}) error {
	w, err := hid.Watch()
	if err != nil {
		return fmt.Errorf("hid watch: %w", err)
	}
	defer func() { _ = w.Close() }()

	// Each target exposes several HID interfaces (keyboard, mouse, raw),
	// each reported as a separate device with the same VID:PID. Track the
	// present interface paths so one physical attach yields exactly one edge.
	present := map[string]bool{}

	emit := func(info *hid.DeviceInfo) bool {
		select {
		case attach <- struct{}{}:
			slog.Info("hid target attached",
				"vid", fmt.Sprintf("%04x", info.VendorID),
				"pid", fmt.Sprintf("%04x", info.ProductID))
			return true
		case <-ctx.Done():
			return false
		}
	}

	// A target already attached at startup counts as an attach edge.
	var first *hid.DeviceInfo
	for _, dev := range w.Snapshot().Devices {
		if d.matches(dev.DeviceInfo) {
			present[dev.DeviceInfo.Path] = true
			if first == nil {
				first = dev.DeviceInfo
			}
		}
	}
	if first != nil && !emit(first) {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Listen():
			if !ok {
				return fmt.Errorf("hid watch closed unexpectedly")
			}
			if ev.MetadataErr != nil {
				slog.Debug("hid event metadata incomplete", "type", ev.Type, "error", ev.MetadataErr)
			}
			switch ev.Type {
			case hid.DeviceEventConnected:
				if d.matches(ev.DeviceInfo) {
					present[ev.DeviceInfo.Path] = true
					if len(present) == 1 && !emit(ev.DeviceInfo) {
						return nil
					}
				}
			case hid.DeviceEventDisconnected:
				if d.matches(ev.DeviceInfo) {
					delete(present, ev.DeviceInfo.Path)
				}
			}
		}
	}
}
