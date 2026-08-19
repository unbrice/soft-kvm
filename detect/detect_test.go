// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Tests for detect.go rendering and suggestion logic.

package detect

import (
	"bytes"
	"strings"
	"testing"
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
		got := deviceName(dev)
		if got != c.want {
			t.Errorf("deviceName(%q, %q) = %q, want %q", c.mfr, c.product, got, c.want)
		}
	}
}

func TestHasKeyboard(t *testing.T) {
	withKB := &hidDevice{interfaces: []ifaceUsage{{0x01, 0x06}, {0x01, 0x02}}}
	withoutKB := &hidDevice{interfaces: []ifaceUsage{{0x01, 0x02}}}
	if !hasKeyboard(withKB) {
		t.Error("expected keyboard")
	}
	if hasKeyboard(withoutKB) {
		t.Error("expected no keyboard")
	}
}

func TestRenderDevices(t *testing.T) {
	devices := []*hidDevice{
		{
			key:        deviceKey{vid: 0x046d, pid: 0xc548},
			mfr:        "Logitech",
			product:    "USB Receiver",
			interfaces: []ifaceUsage{{0x01, 0x06}, {0x01, 0x02}, {0xFF00, 0x01}},
		},
		{
			key:        deviceKey{vid: 0x1532, pid: 0x0285},
			mfr:        "Razer",
			product:    "DeathAdder V2",
			interfaces: []ifaceUsage{{0x01, 0x02}, {0xFF00, 0x01}},
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
}

func TestRenderSuggestions(t *testing.T) {
	devices := []*hidDevice{
		{
			key:        deviceKey{vid: 0x046d, pid: 0xc548},
			mfr:        "Logitech",
			product:    "USB Receiver",
			interfaces: []ifaceUsage{{0x01, 0x06}},
		},
		{
			key:        deviceKey{vid: 0x046d, pid: 0xb35b},
			mfr:        "Logitech",
			product:    "MX Keys",
			interfaces: []ifaceUsage{{0x01, 0x06}},
		},
		{
			key:        deviceKey{vid: 0x1532, pid: 0x0285},
			mfr:        "Razer",
			product:    "DeathAdder V2",
			interfaces: []ifaceUsage{{0x01, 0x02}},
		},
	}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, devices); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "soft-kvm connect --trigger 046d:c548,046d:b35b\n") {
		t.Error("missing suggestion listing the keyboard-capable devices only")
	}
	if strings.Contains(out, "1532:0285") {
		t.Error("non-keyboard device leaked into the suggestion")
	}
}

func TestRenderSuggestionsNoCandidates(t *testing.T) {
	devices := []*hidDevice{
		{
			key:        deviceKey{vid: 0x1532, pid: 0x0285},
			mfr:        "Razer",
			product:    "DeathAdder V2",
			interfaces: []ifaceUsage{{0x01, 0x02}},
		},
	}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, devices); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "No keyboard-capable HID devices detected") {
		t.Error("missing no-candidates message")
	}
	if strings.Contains(out, "Suggested invocation") {
		t.Error("unexpected suggestion when no candidates")
	}
}
