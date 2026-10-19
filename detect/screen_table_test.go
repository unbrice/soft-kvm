// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_table_test.go: the screen-switch suggestion for a monitor whose
// brand maps to a vendor table (SPEC §5.6).

package detect

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/unbrice/soft-kvm/screen"
)

func TestScreenCommandVendorTable(t *testing.T) {
	lg := screen.Display{Mfg: "GSM", Model: "LG ULTRAWIDE"}
	dell := screen.Display{Mfg: "DEL", Model: "DELL U2720Q"}
	if got, want := screenCommand(screen.Report{Displays: []screen.Display{lg}}), "screen-switch DP1_HDMI_ETC lg"; got != want {
		t.Errorf("screenCommand = %q, want %q", got, want)
	}
	// A monitor that does not answer DDC still names its table, ahead of a
	// panel that maps to none.
	unreachable := screen.Report{Err: errors.New("denied"), Unreachable: []screen.Display{dell, lg}}
	if got, want := screenCommand(unreachable), "screen-switch DP1_HDMI_ETC lg"; got != want {
		t.Errorf("screenCommand = %q, want %q", got, want)
	}
	got := screenCommand(screen.Report{Displays: []screen.Display{lg, dell}})
	if want := `screen-switch DP1_HDMI_ETC lg "display=LG ULTRAWIDE"`; got != want {
		t.Errorf("screenCommand = %q, want %q", got, want)
	}
}

func TestRenderScreenUnreachable(t *testing.T) {
	var buf bytes.Buffer
	r := screen.Report{
		Backend:     "ddcutil",
		Err:         errors.New("ddcutil: permission denied on /dev/i2c-*."),
		Unreachable: []screen.Display{{Mfg: "GSM", Model: "LG ULTRAWIDE"}},
	}
	if err := renderScreen(&buf, r); err != nil {
		t.Fatalf("renderScreen: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"port: dp1, hdmi1, hdmi2, usbc1, or a raw",
		"# Monitor: LG ULTRAWIDE (GSM), mapping table lg.\n" +
			"soft-kvm screen-switch DP1_HDMI_ETC lg\n" +
			"# ⚠️ ddcutil: permission denied on /dev/i2c-*.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A vendor table offers its own ports, minus this host's where the table
// knows how that port reads back.
func TestRenderScreenVendorTable(t *testing.T) {
	var buf bytes.Buffer
	r := screen.Report{Backend: "ddcutil", Displays: []screen.Display{
		{Mfg: "GSM", Model: "LG ULTRAWIDE", Input: 0x0F, InputOK: true},
	}}
	if err := renderScreen(&buf, r); err != nil {
		t.Fatalf("renderScreen: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"# Monitor: LG ULTRAWIDE (GSM), mapping table lg, showing this host on dp1.\n",
		"port: hdmi1, hdmi2, usbc1, or a raw",
		"\nsoft-kvm screen-switch DP1_HDMI_ETC lg\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
