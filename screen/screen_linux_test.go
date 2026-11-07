// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_linux_test.go: the ddcutil write against a fake runner and a fake
// /sys/class/drm.

package screen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// recorder is a Runner that records its calls. A setvcp returns err; each
// getvcp 60 answers the next of inputs, the last one repeating, where ""
// stands for a monitor that does not answer.
type recorder struct {
	calls  [][]string
	err    error
	inputs []string
}

func (r *recorder) run(_ context.Context, argv []string) ([]byte, error) {
	r.calls = append(r.calls, argv)
	if slices.Contains(argv, "setvcp") {
		return nil, r.err
	}
	input := ""
	if len(r.inputs) > 0 {
		input = r.inputs[0]
	}
	if len(r.inputs) > 1 {
		r.inputs = r.inputs[1:]
	}
	if input == "" {
		return nil, errors.New("ddcutil: exit status 1: DDCRC_NULL_RESPONSE")
	}
	return []byte("VCP 60 SNC x" + input + "\n"), nil
}

// writes is the setvcp calls r received.
func (r *recorder) writes() [][]string {
	var ws [][]string
	for _, c := range r.calls {
		if slices.Contains(c, "setvcp") {
			ws = append(ws, c)
		}
	}
	return ws
}

// quickSettle shortens how long a monitor is given to settle after a write.
func quickSettle(t *testing.T) {
	old := settle
	settle = 0
	t.Cleanup(func() { settle = old })
}

// attach points the EDID glob at a temp dir holding one EDID per name, as
// connectors do under /sys/class/drm.
func attach(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for i, name := range names {
		conn := filepath.Join(dir, fmt.Sprintf("card0-DP-%d", i+1))
		if err := os.Mkdir(conn, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(conn, "edid"), edidFor(0x1E6D, name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A connector with nothing on it has an empty EDID.
	empty := filepath.Join(dir, "card0-HDMI-A-1")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "edid"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := drmEDIDs
	drmEDIDs = filepath.Join(dir, "*", "edid")
	t.Cleanup(func() { drmEDIDs = old })
}

func TestWriteDo(t *testing.T) {
	quickSettle(t)
	t.Run("standard, from the default source", func(t *testing.T) {
		attach(t, "DELL U2720Q")
		r := &recorder{inputs: []string{"0f", ""}}
		if err := (Write{0x60, 0x51, 0x11}).Do(t.Context(), r.run, "DELL U2720Q"); err != nil {
			t.Fatalf("Do: %v", err)
		}
		want := []string{"ddcutil", "setvcp", "0x60", "0x11", "--noverify", "--model", "DELL U2720Q"}
		if ws := r.writes(); len(ws) != 1 || !slices.Equal(ws[0], want) {
			t.Errorf("writes = %v, want one write %v", ws, want)
		}
	})

	t.Run("lg, from its own source, name matched ignoring case", func(t *testing.T) {
		attach(t, "LG HDR WQHD")
		r := &recorder{inputs: []string{"0f", ""}}
		if err := (Write{0xF4, 0x50, 0xD1}).Do(t.Context(), r.run, "lg hdr wqhd"); err != nil {
			t.Fatalf("Do: %v", err)
		}
		want := []string{"ddcutil", "setvcp", "0xF4", "0xD1", "--noverify",
			"--i2c-source-addr=0x50", "--model", "LG HDR WQHD"}
		if ws := r.writes(); len(ws) != 1 || !slices.Equal(ws[0], want) {
			t.Errorf("writes = %v, want one write %v", ws, want)
		}
	})

	t.Run("a write the monitor ignores fails, and the next lands", func(t *testing.T) {
		attach(t, "LG HDR WQHD")
		r := &recorder{inputs: []string{"0f"}}
		w := Write{0xF4, 0x50, 0xD1}
		if err := w.Do(t.Context(), r.run, "LG HDR WQHD"); err == nil || !strings.Contains(err.Error(), "ignored the write") {
			t.Fatalf("Do = %v, want the write reported ignored", err)
		}
		r.inputs = []string{"0f", ""}
		if err := w.Do(t.Context(), r.run, "LG HDR WQHD"); err != nil {
			t.Fatalf("Do again: %v", err)
		}
		if ws := r.writes(); len(ws) != 2 {
			t.Errorf("writes = %v, want two", ws)
		}
	})

	t.Run("a monitor that answers on its new input has left", func(t *testing.T) {
		attach(t, "DELL U2720Q")
		r := &recorder{inputs: []string{"0f", "11"}}
		if err := (Write{0x60, 0x51, 0x11}).Do(t.Context(), r.run, "DELL U2720Q"); err != nil {
			t.Fatalf("Do: %v", err)
		}
	})

	t.Run("a monitor already on the input is not written", func(t *testing.T) {
		attach(t, "LG HDR WQHD")
		r := &recorder{inputs: []string{"0f"}}
		if err := (Write{0xF4, 0x50, 0xD0}).Do(t.Context(), r.run, "LG HDR WQHD"); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if ws := r.writes(); len(ws) != 0 {
			t.Errorf("writes = %v, want none", ws)
		}
	})

	t.Run("a silent monitor gets one write and one check when target is known", func(t *testing.T) {
		attach(t, "LG HDR WQHD")
		r := &recorder{}
		if err := (Write{0xF4, 0x50, 0xD1}).Do(t.Context(), r.run, "LG HDR WQHD"); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if len(r.calls) != 3 || len(r.writes()) != 1 {
			t.Errorf("calls = %v, want two reads and one write", r.calls)
		}
	})

	t.Run("a silent monitor with a raw target gets one write, unchecked", func(t *testing.T) {
		attach(t, "LG HDR WQHD")
		r := &recorder{}
		if err := (Write{0xF4, 0x50, 0xD2}).Do(t.Context(), r.run, "LG HDR WQHD"); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if len(r.calls) != 2 || len(r.writes()) != 1 {
			t.Errorf("calls = %v, want one read and one write", r.calls)
		}
	})

	t.Run("a monitor silent mid-switch that lands on our input fails", func(t *testing.T) {
		attach(t, "LG HDR WQHD")
		r := &recorder{inputs: []string{"", "0f"}}
		w := Write{0xF4, 0x50, 0xD1}
		if err := w.Do(t.Context(), r.run, "LG HDR WQHD"); err == nil || !strings.Contains(err.Error(), "ignored the write") {
			t.Fatalf("Do = %v, want the write reported ignored", err)
		}
	})

	t.Run("no monitor of that name is nothing to move", func(t *testing.T) {
		attach(t, "DELL U2720Q")
		r := &recorder{}
		err := (Write{0x60, 0x51, 0x11}).Do(t.Context(), r.run, "LG HDR WQHD")
		if !errors.Is(err, ErrNoDisplay) || len(r.calls) != 0 {
			t.Fatalf("Do = %v after %v, want ErrNoDisplay and no call", err, r.calls)
		}
	})

	t.Run("no monitor at all", func(t *testing.T) {
		attach(t)
		if err := (Write{0x60, 0x51, 0x11}).Do(t.Context(), (&recorder{}).run, "LG"); !errors.Is(err, ErrNoDisplay) {
			t.Fatalf("Do = %v, want ErrNoDisplay", err)
		}
	})

	t.Run("two monitors of the name fail rather than pick", func(t *testing.T) {
		attach(t, "LG HDR WQHD", "LG HDR WQHD")
		r := &recorder{}
		err := (Write{0xF4, 0x50, 0xD0}).Do(t.Context(), r.run, "LG HDR WQHD")
		if err == nil || errors.Is(err, ErrNoDisplay) || len(r.calls) != 0 {
			t.Fatalf("Do = %v after %v, want an ambiguity error and no call", err, r.calls)
		}
	})

	t.Run("a failed write is reported once, not retried", func(t *testing.T) {
		attach(t, "LG HDR WQHD")
		boom := errors.New("ddcutil requires module i2c-dev")
		r := &recorder{inputs: []string{"0f"}, err: boom}
		if err := (Write{0xF4, 0x50, 0xD1}).Do(t.Context(), r.run, "LG HDR WQHD"); !errors.Is(err, boom) {
			t.Fatalf("Do = %v, want the write error", err)
		}
		if ws := r.writes(); len(ws) != 1 {
			t.Errorf("writes = %v, want one", ws)
		}
	})
}

func TestAttachedNamesSkipsGarbage(t *testing.T) {
	attach(t, "LG HDR WQHD")
	junk := filepath.Join(filepath.Dir(filepath.Dir(drmEDIDs)), "card0-DP-9")
	if err := os.Mkdir(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, "edid"), []byte("not an edid"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := attachedNames()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"LG HDR WQHD"}) {
		t.Errorf("attachedNames = %q, want only the LG", got)
	}
}

func TestAttachedNamesFailsOnUnreadable(t *testing.T) {
	attach(t, "LG HDR WQHD")
	// A directory where the EDID file should be: ReadFile fails, as it would
	// on a permission error.
	bad := filepath.Join(filepath.Dir(filepath.Dir(drmEDIDs)), "card0-DP-9")
	if err := os.MkdirAll(filepath.Join(bad, "edid"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := attachedNames(); err == nil {
		t.Error("attachedNames = nil error, want the read failure")
	}
	err := (Write{0xF4, 0x50, 0xD0}).Do(t.Context(), nil, "LG HDR WQHD")
	if err == nil || errors.Is(err, ErrNoDisplay) {
		t.Errorf("Do = %v, want the read failure, not ErrNoDisplay", err)
	}
}

func TestProbe(t *testing.T) {
	t.Run("backend down", func(t *testing.T) {
		boom := errors.New("ddcutil requires module i2c-dev")
		r := Probe(t.Context(), func(context.Context, []string) ([]byte, error) { return nil, boom })
		if !errors.Is(r.Err, boom) || len(r.Displays) != 0 {
			t.Fatalf("Probe = %+v, want the backend error and no display", r)
		}
	})

	// ddcutil exits 0 when it may not open the bus: the monitor shows up by
	// EDID only, and the denial is on its output.
	t.Run("bus denied", func(t *testing.T) {
		out := "Device /dev/i2c-13 is not readable and writable.  Error = EACCES(13): Permission denied\n" +
			"Invalid display\n   I2C bus:          /dev/i2c-13\n   Monitor:          GSM:LG ULTRAWIDE:1\n"
		r := Probe(t.Context(), func(context.Context, []string) ([]byte, error) { return []byte(out), nil })
		want := Display{Mfg: "GSM", Model: "LG ULTRAWIDE"}
		if len(r.Displays) != 0 || len(r.Unreachable) != 1 || r.Unreachable[0] != want {
			t.Fatalf("Probe = %+v, want %+v unreachable", r, want)
		}
		if r.Err == nil || !strings.Contains(r.Err.Error(), "i2c group") {
			t.Errorf("Probe error = %v, want the i2c group fix", r.Err)
		}
	})

	t.Run("no answer", func(t *testing.T) {
		out := "Invalid display\n   Monitor:          GSM:LG ULTRAWIDE:1\n"
		r := Probe(t.Context(), func(context.Context, []string) ([]byte, error) { return []byte(out), nil })
		if r.Err == nil || !strings.Contains(r.Err.Error(), "LG ULTRAWIDE does not answer DDC/CI") {
			t.Errorf("Probe error = %v, want the monitor named", r.Err)
		}
	})

	t.Run("an input that cannot be read says why", func(t *testing.T) {
		boom := errors.New("ddcutil: busy")
		run := func(_ context.Context, argv []string) ([]byte, error) {
			if slices.Contains(argv, "detect") {
				return []byte("Display 1\n   Monitor:   GSM:LG ULTRAWIDE:1\n"), nil
			}
			return nil, boom
		}
		r := Probe(t.Context(), run)
		if len(r.Displays) != 1 || r.Displays[0].InputOK || !errors.Is(r.Displays[0].InputErr, boom) {
			t.Fatalf("Probe = %+v, want the display with the read error", r)
		}
	})

	t.Run("one display and its input", func(t *testing.T) {
		run := func(_ context.Context, argv []string) ([]byte, error) {
			if slices.Contains(argv, "detect") {
				return []byte("Display 1\n   Monitor:   GSM:LG ULTRAWIDE:1\n"), nil
			}
			return []byte("VCP 60 SNC x0f\n"), nil
		}
		r := Probe(t.Context(), run)
		want := Display{Mfg: "GSM", Model: "LG ULTRAWIDE", Input: 0x0F, InputOK: true}
		if r.Err != nil || len(r.Displays) != 1 || r.Displays[0] != want {
			t.Fatalf("Probe = %+v, want %+v", r, want)
		}
	})
}
