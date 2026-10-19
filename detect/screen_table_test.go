// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_table_test.go: the screen declaration for a monitor whose brand maps
// to a vendor table (SPEC §5.6).

package detect

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/unbrice/soft-kvm/screen"
)

func TestRenderNotesUnreachable(t *testing.T) {
	var buf bytes.Buffer
	r := screen.Report{
		Backend:     "ddcutil",
		Err:         errors.New("ddcutil: permission denied on /dev/i2c-*."),
		Unreachable: []screen.Display{{Mfg: "GSM", Model: "LG ULTRAWIDE"}},
	}
	if err := renderNotes(&buf, r, buildWiring(nil, r, nil)); err != nil {
		t.Fatalf("renderNotes: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"port on the monitor (dp1,\n# hdmi1, hdmi2, usbc1, or a raw value",
		"# Monitor: LG ULTRAWIDE (GSM), mapping table lg.\n",
		"# ⚠️ ddcutil: permission denied on /dev/i2c-*.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A vendor table offers its own ports, and reads this host's from the
// standard Input Source value the monitor reports.
func TestRenderNotesVendorTable(t *testing.T) {
	var buf bytes.Buffer
	r := screen.Report{Backend: "ddcutil", Displays: []screen.Display{
		{Mfg: "GSM", Model: "LG ULTRAWIDE", Input: 0x0F, InputOK: true},
	}}
	w := buildWiring(nil, r, nil)
	if got := w.screens; len(got) != 1 || got[0].input != "dp1" || got[0].table != "lg" {
		t.Fatalf("screens = %+v, want the LG on dp1 through its table", got)
	}
	if err := renderNotes(&buf, r, w); err != nil {
		t.Fatalf("renderNotes: %v", err)
	}
	out := buf.String()
	if want := "# Monitor: LG ULTRAWIDE (GSM), mapping table lg, showing this host on dp1.\n"; !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
}
