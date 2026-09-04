// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// HID attach detector (Linux and macOS): HID device add/remove events via
// github.com/telesma-app/hid — purego FFI, no cgo, no forked processes.
// Matches any of the configured VID:PID targets: the USB receiver and,
// optionally, a Bluetooth keyboard (HID over BT surfaces like any HID
// device on both OSes) (SPEC §6.1, §6.3).

package detect

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/telesma-app/hid"
)

type vidpid struct{ vid, pid uint16 }

// presence tracks which interface paths of which targets are attached. It
// answers one question: did this target just go from absent to present?
//
// The bookkeeping is per target, not global. The receiver a keyboard is
// paired through is itself a useful --trigger and stays plugged in for
// months, so a single global count never returns to zero and would mask
// every later attach edge (SPEC §6.1).
type presence struct {
	paths map[string]vidpid // attached interface path → its target
	count map[vidpid]int    // interfaces currently attached per target
}

func newPresence() *presence {
	return &presence{paths: map[string]vidpid{}, count: map[vidpid]int{}}
}

// add records one interface and reports whether it is the first one of its
// target — the absent→present edge. Re-adding a known path is not an edge.
func (p *presence) add(info *hid.DeviceInfo) bool {
	if info == nil {
		return false
	}
	if _, dup := p.paths[info.Path]; dup {
		return false
	}
	t := vidpid{info.VendorID, info.ProductID}
	p.paths[info.Path] = t
	p.count[t]++
	return p.count[t] == 1
}

// remove drops one interface, keyed by path: the disconnect event is the one
// place VID:PID may be missing, and the path alone identifies the interface.
func (p *presence) remove(info *hid.DeviceInfo) {
	if info == nil {
		return
	}
	t, ok := p.paths[info.Path]
	if !ok {
		return
	}
	delete(p.paths, info.Path)
	if p.count[t]--; p.count[t] <= 0 {
		delete(p.count, t)
	}
}

// HIDDetector watches HID device events for the configured targets. It is
// built by NewDetector from the slotless --trigger entries.
type HIDDetector struct {
	targets []vidpid
}

func (d *HIDDetector) matches(info *hid.DeviceInfo) bool {
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
func (d *HIDDetector) Run(ctx context.Context, attach chan<- struct{}) error {
	w, err := hid.Watch()
	if err != nil {
		return fmt.Errorf("hid watch: %w", err)
	}
	// A failed close leaves the library's event goroutine running; the caller
	// may re-watch, so a leak would stack watchers.
	defer func() {
		if err := w.Close(); err != nil {
			slog.Warn("hid watcher close failed", "error", err)
		}
	}()

	// Each target exposes several HID interfaces (keyboard, mouse, raw), each
	// reported as a separate device with the same VID:PID, so presence is
	// tracked per target and one physical attach yields exactly one edge.
	p := newPresence()

	emit := func(info *hid.DeviceInfo) {
		slog.Info("hid target attached",
			"vid", fmt.Sprintf("%04x", info.VendorID),
			"pid", fmt.Sprintf("%04x", info.ProductID))
		emitAttach(attach)
	}

	// A target already attached at startup counts as one attach edge.
	var first *hid.DeviceInfo
	for _, dev := range w.Snapshot().Devices {
		if d.matches(dev.DeviceInfo) {
			p.add(dev.DeviceInfo)
			if first == nil {
				first = dev.DeviceInfo
			}
		}
	}
	if first != nil {
		emit(first)
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
				if d.matches(ev.DeviceInfo) && p.add(ev.DeviceInfo) {
					emit(ev.DeviceInfo)
				}
			case hid.DeviceEventDisconnected:
				// Not gated on matches: a disconnect can arrive without
				// VID:PID once the node is gone from sysfs, and remove
				// ignores paths it never tracked anyway.
				p.remove(ev.DeviceInfo)
			}
		}
	}
}
