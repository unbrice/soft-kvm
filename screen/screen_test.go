// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_test.go: the screen-switch parser, the tables and the ddcutil output
// parsers — everything that needs no monitor.

package screen

import (
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
	got := parseDetect([]byte(out))
	want := []Display{{Mfg: "GSM", Model: "LG ULTRAWIDE"}, {Mfg: "ACI", Model: "PA278QV"}}
	if len(got) != len(want) {
		t.Fatalf("parseDetect found %d displays, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("display %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got := parseDetect([]byte("No displays found.\n")); len(got) != 0 {
		t.Errorf("parseDetect on no displays = %+v, want none", got)
	}
}
