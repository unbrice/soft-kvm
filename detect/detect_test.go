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
	"unicode/utf8"

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
	if !strings.Contains(out, "keyboard, mouse, raw, HID++") {
		t.Error("missing interface list and HID++ tag")
	}
	if !strings.Contains(out, triggerMark+"046d:c548") {
		t.Error("keyboard-capable device is not marked as a trigger candidate")
	}
	if !strings.Contains(out, "paired: 1 keyboard, 2 mouse") {
		t.Error("missing receiver slot map")
	}
	if !strings.Contains(out, "🔒 HID++ scan denied") {
		t.Error("missing scan failure reason")
	}
	// The Razer mouse has no keyboard interface and no successful probe:
	// it is not a candidate, and a failed scan must not tag it HID++.
	for line := range strings.Lines(out) {
		if !strings.Contains(line, "1532:0285") {
			continue
		}
		if strings.HasPrefix(line, triggerMark) {
			t.Errorf("non-candidate marked as a trigger: %q", line)
		}
		if strings.Contains(line, "HID++") {
			t.Errorf("HID++ claimed for a device whose scan failed: %q", line)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("line is %d columns, it wraps in an 80-column terminal: %q", n, line)
		}
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
	if out := buf.String(); !strings.Contains(out, "mouse, HID++") {
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
	if out := buf.String(); !strings.Contains(out, "soft-kvm connect --trigger 046d:b35b -- hid-switch 046d:b034 HOST") {
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
	if out := buf.String(); !strings.Contains(out, "-- hid-switch 046d:c548 mouse HOST") {
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
	if !strings.Contains(out, "# FOLLOW THE KEYBOARD") ||
		!strings.Contains(out, "soft-kvm connect --trigger 046d:b35b -- hid-switch 046d:b034 HOST") {
		t.Errorf("missing keyboard-led suggestion:\n%s", out)
	}
	if !strings.Contains(out, "# FOLLOW THE MOUSE") ||
		!strings.Contains(out, "soft-kvm connect --trigger 046d:b034 -- hid-switch 046d:b35b HOST") {
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
		vid  uint16
		err  error
		want string
	}{
		{"permission logitech", logitechVID, errPermission, permissionRemediation},
		{"permission other", 0x0b05, errPermission, "no HID++ to find"},
		{"no answer", 0, errNoAnswer, "⏳ no answer"},
		{"other", 0, errors.New("boom"), "⚠️ boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := probedDevice{status: probeFailed, err: c.err}
			d.key.vid = c.vid
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

// TestDimComments pins the transcript styling: only #-comment lines are
// dimmed, each closes its own reset, and colour off is the identity.
func TestDimComments(t *testing.T) {
	in := "# a comment\n\nsoft-kvm connect --trigger 046d:c548\n#\n"
	if got := dimComments(in, false); got != in {
		t.Errorf("colour off: got %q, want the input unchanged", got)
	}
	want := "\x1b[2m# a comment\x1b[0m\n\nsoft-kvm connect --trigger 046d:c548\n\x1b[2m#\x1b[0m\n"
	if got := dimComments(in, true); got != want {
		t.Errorf("colour on: got %q, want %q", got, want)
	}
}

// TestDimNonTerminal pins that a writer that is not a terminal — every
// test's bytes.Buffer, and `soft-kvm detect > file` — renders plain.
func TestDimNonTerminal(t *testing.T) {
	if got := Dim(&bytes.Buffer{}, "# a comment\n"); got != "# a comment\n" {
		t.Errorf("a bytes.Buffer must not be styled, got %q", got)
	}
}
