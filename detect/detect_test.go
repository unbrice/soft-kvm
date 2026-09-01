// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Tests for detect.go rendering and suggestion logic.

package detect

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/unbrice/soft-kvm/hidpp"
)

func TestUsageName(t *testing.T) {
	cases := []struct {
		page, usage uint16
		want        string
	}{
		{0x01, 0x06, "keyboard"},
		{0x01, 0x02, "mouse"},
		{0x0C, 0x01, "consumer"},
		{0x01, 0x04, "raw"},
		{0xFF00, 0x01, "raw"},
	}
	for _, c := range cases {
		got := usageName(c.page, c.usage)
		if got != c.want {
			t.Errorf("usageName(0x%04x, 0x%04x) = %q, want %q", c.page, c.usage, got, c.want)
		}
	}
}

func TestDeviceName(t *testing.T) {
	cases := []struct {
		mfr, product string
		want         string
	}{
		{"Logitech", "USB Receiver", "Logitech — USB Receiver"},
		{"", "Product", "Product"},
		{"Mfr", "", "Mfr"},
		{"", "", "(unknown)"},
	}
	for _, c := range cases {
		dev := &hidDevice{mfr: c.mfr, product: c.product}
		got := dev.name()
		if got != c.want {
			t.Errorf("name(%q, %q) = %q, want %q", c.mfr, c.product, got, c.want)
		}
	}
}

func TestHasKeyboard(t *testing.T) {
	withKB := &hidDevice{interfaces: []ifaceUsage{{0x01, 0x06}, {0x01, 0x02}}}
	withoutKB := &hidDevice{interfaces: []ifaceUsage{{0x01, 0x02}}}
	if !withKB.hasKeyboard() {
		t.Error("expected keyboard")
	}
	if withoutKB.hasKeyboard() {
		t.Error("expected no keyboard")
	}
}

func TestRenderDevices(t *testing.T) {
	devices := []probedDevice{
		{
			hidDevice: hidDevice{
				key:        deviceKey{vid: 0x046d, pid: 0xc548},
				mfr:        "Logitech",
				product:    "USB Receiver",
				interfaces: []ifaceUsage{{0x01, 0x06}, {0x01, 0x02}, {0xFF00, 0x01}},
			},
			status: probeOK,
			inv: &hidpp.Inventory{
				Kind: hidpp.KindReceiver,
				Paired: []hidpp.PairedDevice{
					{Index: 1, Kind: hidpp.KindKeyboard},
					{Index: 2, Kind: hidpp.KindMouse},
				},
			},
		},
		{
			hidDevice: hidDevice{
				key:        deviceKey{vid: 0x1532, pid: 0x0285},
				mfr:        "Razer",
				product:    "DeathAdder V2",
				interfaces: []ifaceUsage{{0x01, 0x02}, {0xFF00, 0x01}},
			},
			status: probeFailed,
			err:    errPermission,
		},
	}
	var buf bytes.Buffer
	if err := renderDevices(&buf, devices); err != nil {
		t.Fatalf("renderDevices: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "046d:c548") {
		t.Error("missing receiver")
	}
	if !strings.Contains(out, "3 interfaces: keyboard, mouse, raw") {
		t.Error("missing interface list")
	}
	if !strings.Contains(out, "(no keyboard)") {
		t.Error("missing no-keyboard marker")
	}
	if !strings.Contains(out, "(HID++)") {
		t.Error("missing HID++ marker")
	}
	if !strings.Contains(out, "slot 1: keyboard") || !strings.Contains(out, "slot 2: mouse") {
		t.Error("missing receiver slot map")
	}
	if !strings.Contains(out, "HID++ scan failed: 🔒 permission denied") {
		t.Error("missing scan failure reason")
	}
}

func TestRenderDevicesProbeOKWithoutVendor(t *testing.T) {
	// A Bluetooth HID++ mouse may flatten the vendor collection into the
	// primary mouse node; a successful probe should still show the HID++ marker.
	devices := []probedDevice{
		{
			hidDevice: hidDevice{
				key:        deviceKey{vid: 0x046d, pid: 0xb036},
				mfr:        "Logitech",
				product:    "Pebble M350s",
				interfaces: []ifaceUsage{{0x01, 0x02}},
			},
			status: probeOK,
			inv:    &hidpp.Inventory{Kind: hidpp.KindMouse},
		},
	}
	var buf bytes.Buffer
	if err := renderDevices(&buf, devices); err != nil {
		t.Fatalf("renderDevices: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "(HID++)") {
		t.Errorf("missing HID++ marker for probeOK device without vendor interface:\n%s", out)
	}
}

func TestRenderSuggestions(t *testing.T) {
	devices := []probedDevice{
		{hidDevice: hidDevice{
			key:        deviceKey{vid: 0x046d, pid: 0xc548},
			mfr:        "Logitech",
			product:    "USB Receiver",
			interfaces: []ifaceUsage{{0x01, 0x06}},
		}},
		{hidDevice: hidDevice{
			key:        deviceKey{vid: 0x046d, pid: 0xb35b},
			mfr:        "Logitech",
			product:    "MX Keys",
			interfaces: []ifaceUsage{{0x01, 0x06}},
		}},
		{hidDevice: hidDevice{
			key:        deviceKey{vid: 0x1532, pid: 0x0285},
			mfr:        "Razer",
			product:    "DeathAdder V2",
			interfaces: []ifaceUsage{{0x01, 0x02}},
		}},
	}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, devices); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "soft-kvm connect --trigger 046d:b35b,046d:c548\n") {
		t.Error("missing suggestion listing the keyboard-capable devices only, sorted")
	}
	if strings.Contains(out, "1532:0285") {
		t.Error("non-keyboard device leaked into the suggestion")
	}
	if strings.Contains(out, "hid-switch") {
		t.Error("unexpected hid-switch suggestion without a probed mouse")
	}
}

func TestRenderSuggestionsFiltersVIDZero(t *testing.T) {
	internal := probedDevice{hidDevice: hidDevice{
		key:        deviceKey{vid: 0, pid: 0},
		interfaces: []ifaceUsage{{0x01, 0x06}},
	}}
	external := probedDevice{hidDevice: hidDevice{
		key:        deviceKey{vid: 0x046d, pid: 0xb359},
		interfaces: []ifaceUsage{{0x01, 0x06}},
	}}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, []probedDevice{internal, external}); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "0000:0000") {
		t.Errorf("expected internal device 0000:0000 to be filtered from suggestions:\n%s", out)
	}
	if !strings.Contains(out, "046d:b359") {
		t.Errorf("expected external keyboard 046d:b359 to be in suggestions:\n%s", out)
	}
}

func TestRenderSuggestionsHIDSwitch(t *testing.T) {
	keyboard := probedDevice{hidDevice: hidDevice{
		key:        deviceKey{vid: 0x046d, pid: 0xb35b},
		mfr:        "Logitech",
		product:    "MX Keys",
		interfaces: []ifaceUsage{{0x01, 0x06}, {0xFF43, 0x02}},
	}}
	receiver := probedDevice{hidDevice: hidDevice{
		key:        deviceKey{vid: 0x046d, pid: 0xc548},
		mfr:        "Logitech",
		product:    "USB Receiver",
		interfaces: []ifaceUsage{{0x01, 0x06}, {0xFF43, 0x02}},
	}}

	// A directly attached HID++ mouse: two-argument form.
	direct := probedDevice{
		hidDevice: hidDevice{
			key:        deviceKey{vid: 0x046d, pid: 0xb034},
			mfr:        "Logitech",
			product:    "MX Master 3S",
			interfaces: []ifaceUsage{{0x01, 0x02}, {0xFF43, 0x02}},
		},
		status: probeOK,
		inv:    &hidpp.Inventory{Kind: hidpp.KindMouse},
	}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, []probedDevice{keyboard, direct}); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "soft-kvm connect --trigger 046d:b35b -- hid-switch 046d:b034 <host-index>") {
		t.Errorf("missing direct-mouse hid-switch suggestion:\n%s", out)
	}

	// A receiver with a paired mouse: kind form through the receiver.
	withMouse := receiver
	withMouse.status = probeOK
	withMouse.inv = &hidpp.Inventory{
		Kind:   hidpp.KindReceiver,
		Paired: []hidpp.PairedDevice{{Index: 1, Kind: hidpp.KindKeyboard}, {Index: 2, Kind: hidpp.KindMouse}},
	}
	buf.Reset()
	if err := renderSuggestions(&buf, []probedDevice{keyboard, withMouse}); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "-- hid-switch 046d:c548 mouse <host-index>") {
		t.Errorf("missing receiver-mouse hid-switch suggestion:\n%s", out)
	}

	// A receiver with only a keyboard paired suggests nothing.
	kbdOnly := receiver
	kbdOnly.status = probeOK
	kbdOnly.inv = &hidpp.Inventory{
		Kind:   hidpp.KindReceiver,
		Paired: []hidpp.PairedDevice{{Index: 1, Kind: hidpp.KindKeyboard}},
	}
	buf.Reset()
	if err := renderSuggestions(&buf, []probedDevice{keyboard, kbdOnly}); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "hid-switch") {
		t.Errorf("unexpected hid-switch suggestion without a mouse:\n%s", out)
	}

	// A failed probe suggests nothing either.
	failed := receiver
	failed.status = probeFailed
	failed.err = fs.ErrPermission
	buf.Reset()
	if err := renderSuggestions(&buf, []probedDevice{keyboard, failed}); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "hid-switch") {
		t.Errorf("unexpected hid-switch suggestion after a failed probe:\n%s", out)
	}

	// Both peripherals probed and directly attached: both gestures are
	// suggested, each moving the other device.
	probedKeyboard := keyboard
	probedKeyboard.status = probeOK
	probedKeyboard.inv = &hidpp.Inventory{Kind: hidpp.KindKeyboard}
	buf.Reset()
	if err := renderSuggestions(&buf, []probedDevice{probedKeyboard, direct}); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "follow the keyboard:") ||
		!strings.Contains(out, "soft-kvm connect --trigger 046d:b35b -- hid-switch 046d:b034 <host-index>") {
		t.Errorf("missing keyboard-led suggestion:\n%s", out)
	}
	if !strings.Contains(out, "follow the mouse:") ||
		!strings.Contains(out, "soft-kvm connect --trigger 046d:b034 -- hid-switch 046d:b35b <host-index>") {
		t.Errorf("missing mouse-led suggestion:\n%s", out)
	}
}

func TestHasHIDPP(t *testing.T) {
	with := &hidDevice{interfaces: []ifaceUsage{{0x01, 0x06}, {0xFF43, 0x02}}}
	without := &hidDevice{interfaces: []ifaceUsage{{0x01, 0x06}, {0x0C, 0x01}}}
	if !with.hasHIDPP() {
		t.Error("expected HID++")
	}
	if without.hasHIDPP() {
		t.Error("expected no HID++")
	}
}

func TestRenderSuggestionsNoCandidates(t *testing.T) {
	devices := []probedDevice{
		{hidDevice: hidDevice{
			key:        deviceKey{vid: 0x1532, pid: 0x0285},
			mfr:        "Razer",
			product:    "DeathAdder V2",
			interfaces: []ifaceUsage{{0x01, 0x02}},
		}},
	}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, devices); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "No trigger candidates detected") {
		t.Error("missing no-candidates message")
	}
	if strings.Contains(out, "Suggested invocation") {
		t.Error("unexpected suggestion when no candidates")
	}
}

func TestProbeAllSkipsNonHIDPP(t *testing.T) {
	// No vendor interface and not Logitech: no goroutine, no hardware touched —
	// every device comes back probeSkipped, in order.
	devices := []*hidDevice{
		{key: deviceKey{vid: 0x1532, pid: 0x0285}, interfaces: []ifaceUsage{{0x01, 0x02}}},
		{key: deviceKey{vid: 0x1050, pid: 0x0407}, interfaces: []ifaceUsage{{0x01, 0x06}}},
	}
	out := probeAll(t.Context(), devices)
	if len(out) != len(devices) {
		t.Fatalf("probeAll returned %d devices, want %d", len(out), len(devices))
	}
	for i, d := range out {
		if d.key != devices[i].key {
			t.Errorf("device %d: key = %v, want %v (order changed)", i, d.key, devices[i].key)
		}
		if d.status != probeSkipped {
			t.Errorf("device %d: status = %d, want probeSkipped", i, d.status)
		}
	}
}

func TestScanFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"permission", errPermission, "🔒 permission denied"},
		{"no answer", errNoAnswer, "⏳ no answer"},
		{"other", errors.New("boom"), "⚠️ boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := probedDevice{status: probeFailed, err: c.err}
			if got := d.scanFailure(); !strings.Contains(got, c.want) {
				t.Errorf("scanFailure() = %q, want substring %q", got, c.want)
			}
		})
	}
}

func TestClassifyProbeErr(t *testing.T) {
	if got := classifyProbeErr(fs.ErrPermission); got != errPermission {
		t.Errorf("classifyProbeErr(fs.ErrPermission) = %v, want errPermission", got)
	}
	if got := classifyProbeErr(context.DeadlineExceeded); got != errNoAnswer {
		t.Errorf("classifyProbeErr(DeadlineExceeded) = %v, want errNoAnswer", got)
	}
	// The context cause joined onto a probe error still classifies.
	joined := errors.Join(errors.New("io"), context.DeadlineExceeded)
	if got := classifyProbeErr(joined); got != errNoAnswer {
		t.Errorf("classifyProbeErr(%v) = %v, want errNoAnswer", joined, got)
	}
	other := errors.New("boom")
	if got := classifyProbeErr(other); got != other {
		t.Errorf("classifyProbeErr(boom) = %v, want it unchanged", got)
	}
}
