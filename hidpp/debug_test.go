// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Tests for debug.go: the mac-debug transcript runs against hostsEmu, a
// scripted hostsInfo device, so the read decoding and both write framings
// are covered without hardware. The real transcript — what the hardware
// actually answers — is the SPEC §10 checklist's.

package hidpp

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

// hostsEmu emulates one hostsInfo-capable device at devIdx: it resolves
// 0x1814 and 0x1815 to feature indexes, answers 0x1814 fn 0 with its
// nbHost/currHost, 0x1815 fn 0/1/3 from its names, and applies fn 4 writes
// to them. Long-framed (0x11) requests are answered only when longOK — with
// it false they stall, the way Bolt receivers STALL the 20-byte SET_REPORT.
// Unknown requests queue no reply; inside a synctest bubble the resulting
// silence costs fake time only.
type hostsEmu struct {
	reportQueue
	writes      [][]byte
	devIdx      uint8
	nbHost      uint8
	currHost    uint8
	nameMax     uint8
	names       map[uint8]string
	status      map[uint8]uint8
	noHostsInfo bool // report 0x1815 as unsupported
	longOK      bool
}

// The feature indexes the emulated device resolves, matching what the
// measurements report on real hardware (0x1815 at index 10 on the K860).
const (
	emuFeatChangeHost = 0x04
	emuFeatHostsInfo  = 0x0a
)

func (e *hostsEmu) Write(_ context.Context, p []byte) (int, error) {
	e.writes = append(e.writes, slices.Clone(p))
	if reply := e.answer(p); reply != nil {
		e.push(reply)
	}
	return len(p), nil
}

// answer computes the reply the device would send for a request report, or
// nil when it would stay silent.
func (e *hostsEmu) answer(p []byte) []byte {
	if len(p) < 5 || p[1] != e.devIdx {
		return nil
	}
	if p[0] == reportLong && !e.longOK {
		return nil // the transport stalls the long report
	}
	featIdx, fn := p[2], p[3]>>4
	switch {
	case featIdx == 0 && fn == fnGetFeature:
		switch feature := uint16(p[4])<<8 | uint16(p[5]); feature {
		case featChangeHost:
			return e.reply(p, emuFeatChangeHost)
		case featHostsInfo:
			if e.noHostsInfo {
				return e.reply(p, 0x00)
			}
			return e.reply(p, emuFeatHostsInfo)
		}
		return e.reply(p, 0x00)
	case featIdx == emuFeatChangeHost && fn == fnGetHostInfo:
		return e.reply(p, e.nbHost, e.currHost, 0x00)
	case featIdx == emuFeatHostsInfo && !e.noHostsInfo:
		return e.answerHosts(p, fn)
	}
	return nil
}

// reply frames a data reply to request p with the given parameters, in p's
// own report size.
func (e *hostsEmu) reply(p []byte, params ...byte) []byte {
	return append([]byte{p[0], p[1], p[2], p[3]}, params...)
}

// answerHosts answers one hostsInfo request. fn 4 applies the write: the
// chunk overwrites the name at byteIndex, extending it — whether a real
// device extends or truncates is one of the checks the transcript settles.
func (e *hostsEmu) answerHosts(p []byte, fn uint8) []byte {
	switch fn {
	case fnHostsFeatureInfo:
		return e.reply(p, 0x03, 0x00, e.nbHost, e.currHost)
	case fnHostsGetInfo:
		host := p[4]
		return e.reply(p, host, e.status[host], 0x01, 0x00,
			uint8(len(e.names[host])), e.nameMax)
	case fnHostsGetName:
		host, off := p[4], int(p[5])
		name := e.names[host]
		var chunk []byte
		if off < len(name) {
			chunk = []byte(name[off:min(off+14, len(name))])
		}
		// The reply rides the long report: the two-byte prefix, the chunk,
		// then zero padding out to the 16 parameter bytes.
		params := append([]byte{host, byte(off)}, chunk...)
		params = append(params, make([]byte, longParamsMax-len(params))...)
		return append([]byte{reportLong, p[1], p[2], p[3]}, params...)
	case fnHostsSetName:
		host, off := p[4], int(p[5])
		chunk := bytes.TrimRight(p[6:], "\x00") // fixed-size reports pad with NULs
		name := []byte(e.names[host])
		if end := off + len(chunk); end > len(name) {
			grown := make([]byte, end)
			copy(grown, name)
			name = grown
		}
		copy(name[off:], chunk)
		e.names[host] = string(name)
		return e.reply(p, p[4:]...) // the reply echoes the parameters
	}
	return nil
}

// setNameWrites picks the fn 4 write reports out of everything written.
func (e *hostsEmu) setNameWrites() [][]byte {
	var out [][]byte
	for _, w := range e.writes {
		if len(w) >= 4 && w[2] == emuFeatHostsInfo && w[3]>>4 == fnHostsSetName {
			out = append(out, w)
		}
	}
	return out
}

// k860Emu is the measured ERGO K860 state: three paired hosts, names as the
// 2026-09-04 transcript read them, nameMaxLen 24.
func k860Emu() *hostsEmu {
	return &hostsEmu{
		devIdx:   2,
		nbHost:   3,
		currHost: 0,
		nameMax:  24,
		names:    map[uint8]string{0: "1b-nix0", 1: "1b-nix1asw", 2: "unbrice-mac"},
		status:   map[uint8]uint8{0: 1, 1: 1, 2: 1},
		longOK:   true,
	}
}

// runTranscript runs the mac-debug body against emu and returns what it
// printed. The interface line comes from openInterface, which needs
// hardware, so the tests enter just below it.
func runTranscript(t *testing.T, emu *hostsEmu, opts HostsDebugOptions) string {
	t.Helper()
	var buf strings.Builder
	d := &hostsDebug{c: &conn{dev: emu}, w: &buf, devIdx: emu.devIdx}
	if err := d.run(t.Context(), opts); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	return buf.String()
}

func TestHostsDebugRead(t *testing.T) {
	emu := k860Emu()
	out := runTranscript(t, emu, HostsDebugOptions{})
	for _, want := range []string{
		"getFeature(0x1814 changeHost)", "  index 4",
		"getFeature(0x1815 hostsInfo)", "  index 10",
		"0x1814 fn 0 getHostInfo", "  nbHost=3 currHost=0 (channel 1)",
		"0x1815 fn 0 getFeatureInfo",
		"0x1815 fn 1 getHostInfo(0)", "  status=1 busType=1 numPages=0 nameLen=7 nameMaxLen=24",
		"0x1815 fn 3 getHostFriendlyName(0, 0)",
		`  name "1b-nix0"`, `  name "1b-nix1asw"`, `  name "unbrice-mac"`,
		// The raw name reply shows the two-byte prefix and the padding:
		// hostIndex, byteIndex, then "1b-nix0" and NULs out to 16 bytes.
		"rx=00 00 31 62 2d 6e 69 78 30 00 00 00 00 00 00 00",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript omitted %q:\n%s", want, out)
		}
	}
	if writes := emu.setNameWrites(); len(writes) != 0 {
		t.Errorf("a read-only run wrote %d times, want 0", len(writes))
	}
}

func TestHostsDebugReadTwoChunks(t *testing.T) {
	emu := k860Emu()
	emu.names[0] = "abcdefghijklmnopqrst" // 20 chars: a second read at 14
	out := runTranscript(t, emu, HostsDebugOptions{})
	for _, want := range []string{
		"0x1815 fn 3 getHostFriendlyName(0, 0)",
		"0x1815 fn 3 getHostFriendlyName(0, 14)",
		`  name "abcdefghijklmnopqrst"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript omitted %q:\n%s", want, out)
		}
	}
}

func TestHostsDebugNoHostsInfo(t *testing.T) {
	emu := k860Emu()
	emu.noHostsInfo = true
	out := runTranscript(t, emu, HostsDebugOptions{})
	if !strings.Contains(out, "no hostsInfo") {
		t.Errorf("transcript omitted the no-hostsInfo line:\n%s", out)
	}
	if strings.Contains(out, "getHostInfo") {
		t.Errorf("the run went on past the missing feature:\n%s", out)
	}
	if len(emu.writes) != 2 {
		t.Errorf("%d reports written, want the two getFeature calls", len(emu.writes))
	}
}

func TestHostsDebugSetShort(t *testing.T) {
	emu := k860Emu()
	emu.names[0] = "" // the post-wipe state
	out := runTranscript(t, emu, HostsDebugOptions{Set: "soft"})
	if emu.names[0] != "soft" {
		t.Errorf("name = %q, want %q", emu.names[0], "soft")
	}
	writes := emu.setNameWrites()
	if len(writes) != len("soft") {
		t.Fatalf("%d short writes, want one per byte (%d)", len(writes), len("soft"))
	}
	for i, w := range writes {
		if len(w) != 7 || w[0] != reportShort {
			t.Errorf("write %d = %x, want a 7-byte 0x10 report", i, w)
		}
		if w[4] != emu.currHost || w[5] != byte(i) || w[6] != "soft"[i] {
			t.Errorf("write %d params = %x, want host=%d byteIndex=%d %q", i, w[4:], emu.currHost, i, "soft"[i])
		}
	}
	if !strings.Contains(out, "verdict: channel 1 name is \"soft\", as written") {
		t.Errorf("transcript omitted the verdict:\n%s", out)
	}
}

func TestHostsDebugSetLong(t *testing.T) {
	emu := k860Emu()
	emu.names[0] = ""
	name := "abcdefghijklmnopqrst" // 20 bytes: chunks of 14 and 6
	out := runTranscript(t, emu, HostsDebugOptions{Set: name, Long: true})
	if emu.names[0] != name {
		t.Errorf("name = %q, want %q", emu.names[0], name)
	}
	writes := emu.setNameWrites()
	if len(writes) != 2 {
		t.Fatalf("%d long writes, want 2 chunks", len(writes))
	}
	for i, w := range writes {
		if len(w) != 20 || w[0] != reportLong {
			t.Errorf("write %d = %x, want a 20-byte 0x11 report", i, w)
		}
	}
	if writes[0][5] != 0 || writes[1][5] != 14 {
		t.Errorf("byteIndexes = %d, %d; want 0, 14", writes[0][5], writes[1][5])
	}
	if !strings.Contains(out, "one 0x11 report per 14-byte chunk") {
		t.Errorf("transcript omitted the framing line:\n%s", out)
	}
}

func TestHostsDebugSetLongStalls(t *testing.T) {
	runSync(t, "write error is the answer", func(t *testing.T) {
		emu := k860Emu()
		emu.names[0] = ""
		emu.longOK = false // the transport STALLs the 20-byte report
		out := runTranscript(t, emu, HostsDebugOptions{Set: "abcdef", Long: true})
		if emu.names[0] != "" {
			t.Errorf("name = %q, want it untouched", emu.names[0])
		}
		if writes := emu.setNameWrites(); len(writes) != 1 {
			t.Errorf("%d long writes, want 1: the phase stops at the first error", len(writes))
		}
		for _, want := range []string{"write stopped at byteIndex 0", "verdict: MISMATCH"} {
			if !strings.Contains(out, want) {
				t.Errorf("transcript omitted %q:\n%s", want, out)
			}
		}
	})
}

func TestHostsDebugWriteBack(t *testing.T) {
	emu := k860Emu()
	out := runTranscript(t, emu, HostsDebugOptions{WriteBack: true})
	if writes := emu.setNameWrites(); len(writes) != len("1b-nix0") {
		t.Errorf("%d write-back writes, want one per byte of %q", len(writes), "1b-nix0")
	}
	if emu.names[0] != "1b-nix0" {
		t.Errorf("name = %q, want it unchanged", emu.names[0])
	}
	if !strings.Contains(out, `verdict: channel 1 name is "1b-nix0", as written`) {
		t.Errorf("transcript omitted the verdict:\n%s", out)
	}
}

func TestHostsDebugWriteBackBlank(t *testing.T) {
	emu := k860Emu()
	emu.names[0] = ""
	out := runTranscript(t, emu, HostsDebugOptions{WriteBack: true})
	if !strings.Contains(out, "blank, nothing to write back") {
		t.Errorf("transcript omitted the blank line:\n%s", out)
	}
	if writes := emu.setNameWrites(); len(writes) != 0 {
		t.Errorf("%d writes for a blank name, want 0", len(writes))
	}
}

func TestHostsDebugSetTruncates(t *testing.T) {
	emu := k860Emu()
	emu.names[0] = ""
	name := strings.Repeat("a", 30)
	out := runTranscript(t, emu, HostsDebugOptions{Set: name})
	if emu.names[0] != name[:24] {
		t.Errorf("name = %q, want it truncated to nameMaxLen 24", emu.names[0])
	}
	if !strings.Contains(out, "truncated to nameMaxLen 24") {
		t.Errorf("transcript omitted the truncation line:\n%s", out)
	}
}
