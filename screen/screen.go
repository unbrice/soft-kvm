// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen.go: the `screen-switch` virtual switch command — input aliases, the
// mapping tables, the argv parser and the confirm loop both backends share
// (SPEC §5.6). The per-OS files drive the monitor: ddcutil on Linux,
// IOAVService on macOS.

package screen

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Runner executes one argv slice and returns its combined output: nil error
// on exit 0. It is the seam the tests fake; main passes platform.Output.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// Table is how one family of monitors wants its input selected: which VCP
// feature to write, from which I²C source address, and the value per port.
// The port names are the table's own: a command is always about one monitor,
// so no list of inputs spans the tables.
type Table struct {
	Name string
	VCP  uint8
	// Source overrides the I²C source address; 0 keeps the backend's default.
	Source uint8
	// Mfg lists the EDID manufacturer IDs `detect` maps to this table.
	Mfg    []string
	Inputs map[string]uint8
	// Reads is what the monitor reports in Input Source for a port, where
	// that is not the value written — a table that writes another feature. A
	// port missing from it has no known read-back value.
	Reads map[string]uint8
}

// reads returns the Input Source value the monitor reports while it shows
// alias. ok is false when this table does not know it.
func (t *Table) reads(alias string) (value uint8, ok bool) {
	if v, ok := t.Reads[alias]; ok {
		return v, true
	}
	if t.VCP != inputSourceVCP {
		return 0, false
	}
	v, ok := t.Inputs[alias]
	return v, ok
}

// inputSourceVCP is the MCCS Input Source feature: what the standard table
// writes, and what both backends read back to confirm a switch.
const inputSourceVCP = 0x60

// StandardName names the table a command gets when it names none, and the
// one TableFor falls back to.
const StandardName = "standard"

// standard is the MCCS table, and the one a command names no table for. MCCS
// stops at HDMI 2: usbc1 is 0x1B by convention only and vendors deviate, which
// is what a raw INPUT value is for.
var standard = &Table{
	Name: StandardName,
	VCP:  inputSourceVCP,
	Inputs: map[string]uint8{
		"dp1": 0x0F, "dp2": 0x10, "hdmi1": 0x11, "hdmi2": 0x12, "usbc1": 0x1B,
	},
}

// lg is LG's private side channel: firmware that ignores 0x60 writes honours
// 0xF4 sent from source address 0x50 (SPEC §5.6).
var lg = &Table{
	Name:   "lg",
	VCP:    0xF4,
	Source: 0x50,
	Mfg:    []string{"GSM"},
	Inputs: map[string]uint8{
		"dp1": 0xD0, "usbc1": 0xD1, "hdmi1": 0x90, "hdmi2": 0x91,
	},
	// It reports the standard values in Input Source: measured on dp1,
	// assumed for the other ports.
	Reads: standard.Inputs,
}

// tables holds every mapping, standard first. A vendor table describes what
// a backend that writes raw VCP features must send, and both backends do.
var tables = []*Table{standard, lg}

// Tables lists the mapping tables, standard first.
func Tables() []*Table { return tables }

// TableNames lists the mapping tables' names, standard first.
func TableNames() []string {
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.Name
	}
	return names
}

// Aliases lists the port names this table maps, sorted.
func (t *Table) Aliases() []string {
	return slices.Sorted(maps.Keys(t.Inputs))
}

// AliasFor names the port a value read from Input Source stands for. ok is
// false when the table cannot tell: no port of it is known to read as value.
func (t *Table) AliasFor(value uint8) (alias string, ok bool) {
	for alias := range t.Inputs {
		if v, ok := t.reads(alias); ok && v == value {
			return alias, true
		}
	}
	return "", false
}

// TableFor maps an EDID manufacturer ID to its table: standard unless a
// vendor table claims the ID. Only `detect` calls it — screen-switch itself
// never guesses a table.
func TableFor(mfg string) *Table {
	for _, t := range tables {
		if slices.Contains(t.Mfg, strings.ToUpper(mfg)) {
			return t
		}
	}
	return standard
}

func tableByName(name string) *Table {
	for _, t := range tables {
		if t.Name == strings.ToLower(name) {
			return t
		}
	}
	return nil
}

const usage = "usage: screen-switch INPUT [TABLE] [display=MATCH]"

// Switch is a parsed `screen-switch INPUT [TABLE] [display=MATCH]` command.
type Switch struct {
	Table *Table
	// Value is what gets written to Table.VCP.
	Value uint8
	// Expect is the Input Source value a monitor that still answers DDC
	// reports while it shows the target. ExpectOK is false when the table
	// does not know it; the monitor going silent is then the only receipt.
	Expect   uint8
	ExpectOK bool
	// Display narrows the command to one monitor; "" leaves the choice to the
	// backend, which is right when one external display is attached.
	Display string
}

// Parse validates a screen-switch argv (without the "screen-switch" word).
// INPUT is one of the table's aliases or a raw value (0x1B, 27) written as
// is; TABLE defaults to standard and is never guessed here — `detect` prints it when the monitor
// needs one (SPEC §5.6).
func Parse(args []string) (*Switch, error) {
	if len(args) == 0 {
		return nil, errors.New(usage)
	}
	s := &Switch{Table: standard}
	input, rest := strings.ToLower(args[0]), args[1:]
	tableSet := false
	for _, arg := range rest {
		if match, ok := strings.CutPrefix(arg, "display="); ok {
			if match == "" || s.Display != "" {
				return nil, fmt.Errorf("invalid argument %q: %s", arg, usage)
			}
			s.Display = match
			continue
		}
		t := tableByName(arg)
		if t == nil || tableSet {
			return nil, fmt.Errorf("invalid argument %q: want a mapping table (%s) or display=MATCH",
				arg, strings.Join(TableNames(), ", "))
		}
		s.Table, tableSet = t, true
	}
	if v, ok := s.Table.Inputs[input]; ok {
		s.Value = v
		s.Expect, s.ExpectOK = s.Table.reads(input)
	} else {
		v, err := strconv.ParseUint(input, 0, 8)
		if err != nil {
			return nil, fmt.Errorf("invalid input %q for table %q: want %s or a raw value such as 0x1B",
				args[0], s.Table.Name, strings.Join(s.Table.Aliases(), ", "))
		}
		s.Value = uint8(v)
		// A raw value reads back as itself only where the write went to the
		// feature that is read.
		if s.Table.VCP == inputSourceVCP {
			s.Expect, s.ExpectOK = s.Value, true
		}
	}
	return s, nil
}

// Display is one DDC-capable monitor `detect` found.
type Display struct {
	Mfg   string // EDID manufacturer ID, e.g. GSM
	Model string // the name display=MATCH selects it by
	// Input is the monitor's current Input Source value — this host's own
	// port, since it is the one on screen. InputOK is false when unread.
	Input   uint8
	InputOK bool
}

// Report is what Probe learned for `detect`: whether the backend works at all
// and, where it can tell, which monitors it reaches.
type Report struct {
	Backend  string // the program screen-switch delegates to on this OS
	Err      error  // why the backend is unusable; nil when it answered
	Displays []Display
}

// parseInputSource reads the value out of `ddcutil --terse getvcp 60`, e.g.
// "VCP 60 SNC x0f": the low byte is the last field.
func parseInputSource(out []byte) (uint8, bool) {
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != "VCP" || !strings.EqualFold(f[1], "60") {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimPrefix(f[len(f)-1], "x"), 16, 8)
		if err != nil {
			return 0, false
		}
		return uint8(v), true
	}
	return 0, false
}

// parseDetect reads the usable monitors out of `ddcutil detect --terse`: one
// "Display N" block per monitor that answers DDC, each with a
// "Monitor: MFG:MODEL:SERIAL" line. "Invalid display" blocks are skipped —
// they are what the inactive input looks like.
func parseDetect(out []byte) []Display {
	var displays []Display
	valid := false
	for line := range strings.Lines(string(out)) {
		if line == strings.TrimLeft(line, " \t") { // a block header
			valid = strings.HasPrefix(line, "Display ")
			continue
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Monitor:")
		if !ok || !valid {
			continue
		}
		f := strings.SplitN(strings.TrimSpace(rest), ":", 3)
		if len(f) < 2 {
			continue
		}
		displays = append(displays, Display{Mfg: f[0], Model: f[1]})
	}
	return displays
}

// The confirm window: how long the monitor may keep answering on this input
// after the write before the switch counts as failed, and the poll spacing.
// Variables so the tests can shrink them.
var (
	confirmWindow = 4 * time.Second
	confirmPoll   = 500 * time.Millisecond
)

// confirm polls this host's own read of Input Source after a write, the
// receipt both backends share (SPEC §5.6): it fails, or reports the target,
// once the switch landed. read returns err when the monitor no longer
// answers, and ok false when it answered with nothing readable.
func (s *Switch) confirm(ctx context.Context, read func(context.Context) (v uint8, ok bool, err error)) error {
	deadline := time.Now().Add(confirmWindow)
	for {
		v, ok, err := read(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil // DDC went silent: this input is no longer the active one
		}
		if ok && s.ExpectOK && v == s.Expect {
			// The monitor shows the target and still answers: it answers on
			// every input, or the target is this host's own port.
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("screen-switch: monitor still on this input %v after the write", confirmWindow)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(confirmPoll):
		}
	}
}

// DDC/CI for a backend that speaks it in-process (SPEC §5.6). The I²C call
// itself sends the source address first; the frames below follow it.
const (
	ddcAddr  = 0x37 // the monitor's DDC/CI I²C address
	ddcHost  = 0x51 // the standard host source address
	edidAddr = 0x50 // the EDID's I²C address
)

// errNoReply is a read that brought nothing back from the monitor: it no
// longer answers DDC/CI on this input.
var errNoReply = errors.New("no DDC/CI reply")

// getVCPRequest frames a Get VCP Feature request for vcp, sent from src.
func getVCPRequest(src, vcp uint8) []byte { return ddcFrame(src, 0x01, vcp) }

// setVCPRequest frames a Set VCP Feature request writing value to vcp.
func setVCPRequest(src, vcp uint8, value uint16) []byte {
	return ddcFrame(src, 0x03, vcp, byte(value>>8), byte(value))
}

// ddcFrame lays out a host message after its source byte: the length, the
// payload, and a checksum that also covers the two addresses.
func ddcFrame(src uint8, payload ...byte) []byte {
	msg := append([]byte{0x80 | byte(len(payload))}, payload...)
	sum := byte(ddcAddr<<1) ^ src
	for _, b := range msg {
		sum ^= b
	}
	return append(msg, sum)
}

// parseVCPReply reads the current value out of the 11-byte reply to a Get
// VCP Feature request for vcp.
func parseVCPReply(reply []byte, vcp uint8) (uint16, error) {
	if len(reply) < 11 || reply[0] != ddcAddr<<1 {
		return 0, errNoReply
	}
	// A null message, what this LG answers once it shows another input:
	// nothing to report, which ddcutil counts as no response too.
	if reply[1] == 0x80 && reply[2] == 0xBE {
		return 0, fmt.Errorf("%w: null message", errNoReply)
	}
	// On this M4 Pro the length byte of a VCP reply has come back as 0x51,
	// with a checksum that still covers the 0x88 the length must be: check
	// against that length, not the byte read.
	sum := byte(0x50) ^ reply[0] ^ 0x88
	for _, b := range reply[2:10] {
		sum ^= b
	}
	switch {
	case sum != reply[10]:
		return 0, fmt.Errorf("DDC/CI reply % x: bad checksum", reply)
	case reply[2] != 0x02 || reply[4] != vcp:
		return 0, fmt.Errorf("DDC/CI reply % x: not about VCP %#02x", reply, vcp)
	case reply[3] != 0:
		return 0, fmt.Errorf("DDC/CI reply % x: VCP %#02x unsupported", reply, vcp)
	}
	return uint16(reply[8])<<8 | uint16(reply[9]), nil
}

// parseEDID reads the manufacturer and the monitor name out of an EDID base
// block. ok is false when edid is not one: wrong header, length or checksum.
func parseEDID(edid []byte) (mfg, name string, ok bool) {
	if len(edid) < 128 || string(edid[:8]) != "\x00\xff\xff\xff\xff\xff\xff\x00" {
		return "", "", false
	}
	var sum byte
	for _, b := range edid[:128] {
		sum += b
	}
	if sum != 0 {
		return "", "", false
	}
	// Four 18-byte descriptors follow the timings; tag 0xFC is the name:
	// thirteen bytes, ended by a newline and padded with spaces.
	for d := 54; d < 126; d += 18 {
		if edid[d] == 0 && edid[d+1] == 0 && edid[d+3] == 0xFC {
			name, _, _ = strings.Cut(string(edid[d+5:d+18]), "\n")
			name = strings.TrimRight(name, " ")
		}
	}
	return pnpID(uint16(edid[8])<<8 | uint16(edid[9])), name, true
}

// pnpID decodes an EDID manufacturer ID: three letters of five bits each, 'A'
// as 1, so 0x1E6D is GSM. It returns "" for 0, a display that names none.
func pnpID(code uint16) string {
	if code == 0 {
		return ""
	}
	return string([]byte{'@' + byte(code>>10&31), '@' + byte(code>>5&31), '@' + byte(code&31)})
}
