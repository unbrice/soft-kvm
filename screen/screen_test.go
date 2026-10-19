// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_test.go: the screen-switch parser, the tables, the ddcutil output
// parsers, and the DDC/CI framing and EDID parsing the macOS backend speaks
// through — everything that needs no monitor.

package screen

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantTable string
		wantValue uint8
		wantExp   uint8
		wantExpOK bool
		wantDisp  string
		wantErr   string // substring; empty means success
	}{
		{name: "alias, standard by default", args: []string{"dp1"}, wantTable: "standard", wantValue: 0x0F, wantExp: 0x0F, wantExpOK: true},
		{name: "alias is case-insensitive", args: []string{"HDMI2"}, wantTable: "standard", wantValue: 0x12, wantExp: 0x12, wantExpOK: true},
		{name: "raw hex", args: []string{"0x1B"}, wantTable: "standard", wantValue: 0x1B, wantExp: 0x1B, wantExpOK: true},
		{name: "raw decimal", args: []string{"27"}, wantTable: "standard", wantValue: 27, wantExp: 27, wantExpOK: true},
		{name: "display match", args: []string{"dp2", "display=LG ULTRAWIDE"}, wantTable: "standard", wantValue: 0x10, wantExp: 0x10, wantExpOK: true, wantDisp: "LG ULTRAWIDE"},
		{name: "no arguments", args: nil, wantErr: "usage"},
		{name: "unknown input", args: []string{"vga"}, wantErr: "invalid input"},
		{name: "value out of range", args: []string{"0x100"}, wantErr: "invalid input"},
		{name: "unknown table", args: []string{"dp1", "dell"}, wantErr: "mapping table"},
		{name: "empty display", args: []string{"dp1", "display="}, wantErr: "invalid argument"},
		{name: "display twice", args: []string{"dp1", "display=a", "display=b"}, wantErr: "invalid argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := Parse(c.args)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("Parse(%v) error = %v, want substring %q", c.args, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%v): %v", c.args, err)
			}
			if s.Table.Name != c.wantTable || s.Value != c.wantValue || s.Display != c.wantDisp {
				t.Errorf("Parse(%v) = table %q value %#x display %q, want %q %#x %q",
					c.args, s.Table.Name, s.Value, s.Display, c.wantTable, c.wantValue, c.wantDisp)
			}
			if s.Expect != c.wantExp || s.ExpectOK != c.wantExpOK {
				t.Errorf("Parse(%v) expect = %#x/%v, want %#x/%v", c.args, s.Expect, s.ExpectOK, c.wantExp, c.wantExpOK)
			}
		})
	}
}

// Table names are unique and the standard table comes first: it is the
// default, and the order the help and the errors print.
func TestTablesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, tbl := range tables {
		if seen[tbl.Name] {
			t.Errorf("duplicate table name %q", tbl.Name)
		}
		seen[tbl.Name] = true
		if len(tbl.Inputs) == 0 {
			t.Errorf("table %q maps no input", tbl.Name)
		}
	}
	if tables[0] != standard {
		t.Error("the standard table must come first")
	}
}

func TestTableFor(t *testing.T) {
	if got := TableFor("DEL"); got != standard {
		t.Errorf("TableFor(DEL) = %q, want standard", got.Name)
	}
	if got := TableFor(""); got != standard {
		t.Errorf("TableFor(\"\") = %q, want standard", got.Name)
	}
}

func TestTableAliases(t *testing.T) {
	if got := strings.Join(standard.Aliases(), ","); got != "dp1,dp2,hdmi1,hdmi2,usbc1" {
		t.Errorf("standard aliases = %q", got)
	}
}

func TestAliasFor(t *testing.T) {
	if got, ok := standard.AliasFor(0x0F); !ok || got != "dp1" {
		t.Errorf("standard AliasFor(0x0F) = %q/%v, want dp1", got, ok)
	}
	if got, ok := standard.AliasFor(0x42); ok {
		t.Errorf("standard AliasFor(0x42) = %q, want none", got)
	}
}

func TestParseInputSource(t *testing.T) {
	cases := []struct {
		out    string
		want   uint8
		wantOK bool
	}{
		{"VCP 60 SNC x0f\n", 0x0F, true},
		{"VCP 60 CNC x00 x00 x00 x11\n", 0x11, true},
		{"some warning\nVCP 60 SNC x1b\n", 0x1B, true},
		{"VCP 10 C 50 100\n", 0, false},
		{"VCP 60 ERR\n", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseInputSource([]byte(c.out))
		if got != c.want || ok != c.wantOK {
			t.Errorf("parseInputSource(%q) = %#x/%v, want %#x/%v", c.out, got, ok, c.want, c.wantOK)
		}
	}
}

func TestParseDetect(t *testing.T) {
	out := `Display 1
   I2C bus:  /dev/i2c-6
   DRM connector:           card1-DP-3
   Monitor:                 GSM:LG ULTRAWIDE:206NTAB12345

Invalid display
   I2C bus:  /dev/i2c-7
   DRM connector:           card1-HDMI-A-1
   Monitor:                 DEL:DELL U2720Q:XYZ

Display 2
   I2C bus:  /dev/i2c-8
   Monitor:                 ACI:PA278QV:
`
	got, unreachable := parseDetect([]byte(out))
	if dell := (Display{Mfg: "DEL", Model: "DELL U2720Q"}); len(unreachable) != 1 || unreachable[0] != dell {
		t.Errorf("parseDetect unreachable = %+v, want %+v", unreachable, dell)
	}
	want := []Display{{Mfg: "GSM", Model: "LG ULTRAWIDE"}, {Mfg: "ACI", Model: "PA278QV"}}
	if len(got) != len(want) {
		t.Fatalf("parseDetect found %d displays, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("display %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got, unreachable := parseDetect([]byte("No displays found.\n")); len(got)+len(unreachable) != 0 {
		t.Errorf("parseDetect on no displays = %+v / %+v, want none", got, unreachable)
	}
}

func mustParse(t *testing.T, args ...string) *Switch {
	t.Helper()
	s, err := Parse(args)
	if err != nil {
		t.Fatalf("Parse(%v): %v", args, err)
	}
	return s
}

func TestLGTable(t *testing.T) {
	lg := tableByName("lg")
	if lg == nil {
		t.Fatal("no lg table")
	}
	for _, mfg := range []string{"GSM", "gsm"} {
		if got := TableFor(mfg); got != lg {
			t.Errorf("TableFor(%s) = %q, want lg", mfg, got.Name)
		}
	}
	// A table names only the ports it has a value for.
	if got := strings.Join(lg.Aliases(), ","); got != "dp1,hdmi1,hdmi2,usbc1" {
		t.Errorf("lg aliases = %q", got)
	}
	if _, err := Parse([]string{"dp2", "lg"}); err == nil || !strings.Contains(err.Error(), "dp1, hdmi1, hdmi2, usbc1") {
		t.Errorf("Parse(dp2 lg) error = %v, want one listing lg's ports", err)
	}
	// lg writes 0xF4 but reads back the standard Input Source values.
	for value, want := range map[uint8]string{0x0F: "dp1", 0x11: "hdmi1", 0x12: "hdmi2", 0x1B: "usbc1"} {
		if got, ok := lg.AliasFor(value); !ok || got != want {
			t.Errorf("lg AliasFor(%#x) = %q/%v, want %s", value, got, ok, want)
		}
	}
	if got, ok := lg.AliasFor(0xD0); ok {
		t.Errorf("lg AliasFor(0xD0) = %q, want none: 0xD0 is written, never read", got)
	}
	if s := mustParse(t, "dp1", "lg"); !s.ExpectOK || s.Expect != 0x0F {
		t.Errorf("Parse(dp1 lg) expect = %#x/%v, want 0x0F", s.Expect, s.ExpectOK)
	}
	if s := mustParse(t, "usbc1", "lg"); !s.ExpectOK || s.Expect != 0x1B {
		t.Errorf("Parse(usbc1 lg) expect = %#x/%v, want 0x1B", s.Expect, s.ExpectOK)
	}
	// A raw value went to 0xF4: nothing says how the port reads back.
	if s := mustParse(t, "0xD2", "lg"); s.Table != lg || s.ExpectOK {
		t.Errorf("Parse(0xD2 lg) = table %q, ExpectOK %v; want lg, false", s.Table.Name, s.ExpectOK)
	}
}

// The frames follow the source byte on the wire; checksums worked by hand
// over 0x6E, the source and every byte before them.
func TestDDCFrames(t *testing.T) {
	cases := []struct {
		name      string
		got, want []byte
	}{
		{"get input source", getVCPRequest(0x51, 0x60), []byte{0x82, 0x01, 0x60, 0xDC}},
		{"set dp1", setVCPRequest(0x51, 0x60, 0x0F), []byte{0x84, 0x03, 0x60, 0x00, 0x0F, 0xD7}},
		{"lg dp1 from 0x50", setVCPRequest(0x50, 0xF4, 0xD0), []byte{0x84, 0x03, 0xF4, 0x00, 0xD0, 0x9D}},
	}
	for _, c := range cases {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: frame % x, want % x", c.name, c.got, c.want)
		}
	}
}

func TestParseVCPReply(t *testing.T) {
	cases := []struct {
		name    string
		reply   []byte
		want    uint16
		noReply bool // errNoReply rather than another error
		bad     bool // any other error
	}{
		// What this LG sent an M4 Pro: input 0x0F (dp1) of max 0x12, the
		// length byte read back as 0x51.
		{name: "length byte mangled", reply: []byte{0x6e, 0x51, 0x02, 0x00, 0x60, 0x00, 0x00, 0x12, 0x00, 0x0f, 0xc9}, want: 0x0F},
		{name: "length byte intact", reply: []byte{0x6e, 0x88, 0x02, 0x00, 0x60, 0x00, 0x00, 0x12, 0x00, 0x0f, 0xc9}, want: 0x0F},
		{name: "nothing read", reply: make([]byte, 11), noReply: true},
		// What it sent once the LG showed another input: a null message,
		// followed by stale bytes.
		{name: "null message", reply: []byte{0x6e, 0x80, 0xbe, 0x04, 0x00, 0x0d, 0x12, 0x18, 0x27, 0x16, 0x1c}, noReply: true},
		{name: "short read", reply: []byte{0x6e, 0x88, 0x02}, noReply: true},
		{name: "bad checksum", reply: []byte{0x6e, 0x51, 0x02, 0x00, 0x60, 0x00, 0x00, 0x12, 0x00, 0x0f, 0xca}, bad: true},
		{name: "unsupported", reply: []byte{0x6e, 0x88, 0x02, 0x01, 0x60, 0x00, 0x00, 0x00, 0x00, 0x00, 0xd5}, bad: true},
		{name: "another feature", reply: []byte{0x6e, 0x88, 0x02, 0x00, 0x10, 0x00, 0x00, 0x64, 0x00, 0x32, 0xf2}, bad: true},
	}
	for _, c := range cases {
		got, err := parseVCPReply(c.reply, 0x60)
		switch {
		case c.noReply && !errors.Is(err, errNoReply):
			t.Errorf("%s: error = %v, want errNoReply", c.name, err)
		case c.bad && (err == nil || errors.Is(err, errNoReply)):
			t.Errorf("%s: error = %v, want a malformed-reply error", c.name, err)
		case !c.noReply && !c.bad && (err != nil || got != c.want):
			t.Errorf("%s: = %#x, %v; want %#x", c.name, got, err, c.want)
		}
	}
}

// edidFor builds an EDID base block for a manufacturer code and a monitor
// name, the name in descriptor 3 as on the LG, with a valid checksum.
func edidFor(code uint16, name string) []byte {
	e := make([]byte, 128)
	copy(e, "\x00\xff\xff\xff\xff\xff\xff\x00")
	e[8], e[9] = byte(code>>8), byte(code)
	copy(e[90:], []byte{0, 0, 0, 0xFC, 0})
	copy(e[95:108], fmt.Sprintf("%-13s", name+"\n"))
	var sum byte
	for _, b := range e[:127] {
		sum += b
	}
	e[127] = -sum
	return e
}

func TestParseEDID(t *testing.T) {
	for _, c := range []struct {
		code      uint16
		name, mfg string
	}{
		{0x1E6D, "LG ULTRAWIDE", "GSM"}, // thirteen bytes with its newline
		{0x10AC, "DELL U2720Q", "DEL"},  // padded with spaces
	} {
		mfg, name, ok := parseEDID(edidFor(c.code, c.name))
		if !ok || mfg != c.mfg || name != c.name {
			t.Errorf("parseEDID(%#x %q) = %q, %q, %v", c.code, c.name, mfg, name, ok)
		}
	}
	bad := edidFor(0x1E6D, "LG ULTRAWIDE")
	bad[20]++
	if _, _, ok := parseEDID(bad); ok {
		t.Error("parseEDID accepted a block with a bad checksum")
	}
	if _, _, ok := parseEDID(make([]byte, 128)); ok {
		t.Error("parseEDID accepted a block without the EDID header")
	}
}

func TestPnpID(t *testing.T) {
	for code, want := range map[uint16]string{0x1E6D: "GSM", 0x0610: "APP", 0x10AC: "DEL", 0: ""} {
		if got := pnpID(code); got != want {
			t.Errorf("pnpID(%#x) = %q, want %q", code, got, want)
		}
	}
}
