// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Tests for the attach detector's presence bookkeeping (SPEC §6.1).

package detect

import (
	"slices"
	"testing"

	"github.com/telesma-app/hid"
	"github.com/unbrice/soft-kvm/hidpp"
)

func dev(path string, vid, pid uint16) *hid.DeviceInfo {
	return &hid.DeviceInfo{Path: path, VendorID: vid, ProductID: pid}
}

// One physical attach fires one event per HID interface; only the first is an
// edge (SPEC §6.1).
func TestPresenceDeduplicatesInterfaces(t *testing.T) {
	p := newPresence()
	if !p.add(dev("/dev/hidraw4", 0x046d, 0xc548)) {
		t.Fatal("first interface of a target must be an edge")
	}
	for _, path := range []string{"/dev/hidraw5", "/dev/hidraw6"} {
		if p.add(dev(path, 0x046d, 0xc548)) {
			t.Errorf("%s: further interfaces of the same target must not be edges", path)
		}
	}
	if p.add(dev("/dev/hidraw4", 0x046d, 0xc548)) {
		t.Error("re-adding a known path must not be an edge")
	}
}

// The regression: a receiver that stays plugged in must not mask the attach
// edges of the keyboard paired through it.
func TestPresenceAlwaysOnTargetDoesNotMaskOthers(t *testing.T) {
	p := newPresence()
	// The Bolt receiver's three interfaces, plugged in for months.
	for _, path := range []string{"/dev/hidraw4", "/dev/hidraw5", "/dev/hidraw6"} {
		p.add(dev(path, 0x046d, 0xc548))
	}

	// Easy-Switch moves the keyboard here, away, and back again.
	for i := range 3 {
		if !p.add(dev("/dev/hidraw3", 0x046d, 0x4088)) {
			t.Fatalf("attach %d: keyboard attach must be an edge", i)
		}
		p.remove(dev("/dev/hidraw3", 0x046d, 0x4088))
	}
	if p.count[vidpid{0x046d, 0xc548}] != 3 {
		t.Errorf("receiver interfaces = %d, want 3", p.count[vidpid{0x046d, 0xc548}])
	}
}

// A disconnect may arrive with the path but no VID:PID once the node is gone
// from sysfs; it must still clear the interface, or the next attach is masked.
func TestPresenceRemoveWithoutMetadata(t *testing.T) {
	p := newPresence()
	if !p.add(dev("/dev/hidraw3", 0x046d, 0x4088)) {
		t.Fatal("attach must be an edge")
	}
	p.remove(&hid.DeviceInfo{Path: "/dev/hidraw3"})
	if !p.add(dev("/dev/hidraw3", 0x046d, 0x4088)) {
		t.Error("re-attach after a metadata-less disconnect must be an edge")
	}
}

// A disconnect for a path that was never tracked, or a nil DeviceInfo, must
// not corrupt the counts.
func TestPresenceRemoveUnknown(t *testing.T) {
	p := newPresence()
	p.add(dev("/dev/hidraw3", 0x046d, 0x4088))
	p.remove(dev("/dev/hidraw9", 0x046d, 0x4088))
	p.remove(nil)
	p.remove(&hid.DeviceInfo{})
	if got := p.count[vidpid{0x046d, 0x4088}]; got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
	if p.add(dev("/dev/hidraw3", 0x046d, 0x4088)) {
		t.Error("the tracked interface must still be present")
	}
}

func TestParseTriggers(t *testing.T) {
	named := map[string]hidpp.Target{
		"ergo-k860": {VID: 0x046d, PID: 0xc52b, Slot: 2},
		"lift":      {VID: 0x046d, PID: 0xb031},
	}
	cases := []struct {
		in   string
		want []trigger
	}{
		{"lift", []trigger{{vidpid{0x046d, 0xb031}, 0}}},
		{"ergo-k860", []trigger{{vidpid{0x046d, 0xc52b}, 2}}},
		{" ergo-k860 , lift", []trigger{
			{vidpid{0x046d, 0xc52b}, 2},
			{vidpid{0x046d, 0xb031}, 0},
		}},
	}
	for _, c := range cases {
		got, err := parseTriggers(c.in, named)
		if err != nil {
			t.Errorf("parseTriggers(%q): %v", c.in, err)
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("parseTriggers(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseTriggersRejects(t *testing.T) {
	// Only a declared name is a trigger: an address is spelled in its
	// declaration, never here.
	named := map[string]hidpp.Target{"lift": {VID: 0x046d, PID: 0xb031}}
	for _, in := range []string{"", "lift,", "mouse", "046d:b031", "046d:c52b:2", "LIFT"} {
		if got, err := parseTriggers(in, named); err == nil {
			t.Errorf("parseTriggers(%q) = %v, want error", in, got)
		}
	}
}

// Each entry must reach the detector that can actually see it: a device
// declared by VID:PID to the HID watcher, one declared by VID:PID:SLOT to the
// receiver watcher, with slots of one receiver gathered under a single target.
func TestNewDetectorRoutesAndGroups(t *testing.T) {
	d, err := NewDetector("dongle,kbd,mouse,dongle-again", map[string]hidpp.Target{
		"dongle":       {VID: 0x046d, PID: 0xc548},
		"kbd":          {VID: 0x046d, PID: 0xc52b, Slot: 2},
		"mouse":        {VID: 0x046d, PID: 0xc52b, Slot: 1},
		"dongle-again": {VID: 0x046d, PID: 0xc548},
	})
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	if want := []vidpid{{0x046d, 0xc548}}; !slices.Equal(d.hidTargets, want) {
		t.Errorf("hidTargets = %v, want %v (deduplicated)", d.hidTargets, want)
	}
	if len(d.receivers) != 1 {
		t.Fatalf("receivers = %v, want one grouped target", d.receivers)
	}
	if d.receivers[0].vidpid != (vidpid{0x046d, 0xc52b}) {
		t.Errorf("receiver = %v, want 046d:c52b", d.receivers[0].vidpid)
	}
	if want := []uint8{2, 1}; !slices.Equal(d.receivers[0].slots, want) {
		t.Errorf("slots = %v, want %v", d.receivers[0].slots, want)
	}
}

// emitAttach must never block the detector: an edge already queued makes the
// next one redundant (agent invariant 3).
func TestEmitAttachCoalesces(t *testing.T) {
	ch := make(chan struct{}, 1)
	for range 5 {
		emitAttach(ch) // would deadlock if it blocked
	}
	if len(ch) != 1 {
		t.Errorf("queued %d edges, want 1", len(ch))
	}
}
