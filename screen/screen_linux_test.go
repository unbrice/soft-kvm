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

// recorder is a Runner that records its calls and returns err.
type recorder struct {
	calls [][]string
	err   error
}

func (r *recorder) run(_ context.Context, argv []string) ([]byte, error) {
	r.calls = append(r.calls, argv)
	return nil, r.err
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
	t.Run("standard, from the default source", func(t *testing.T) {
		attach(t, "DELL U2720Q")
		r := &recorder{}
		if err := (Write{0x60, 0x51, 0x11}).Do(t.Context(), r.run, "DELL U2720Q"); err != nil {
			t.Fatalf("Do: %v", err)
		}
		want := []string{"ddcutil", "setvcp", "0x60", "0x11", "--noverify", "--model", "DELL U2720Q"}
		if len(r.calls) != 1 || !slices.Equal(r.calls[0], want) {
			t.Errorf("calls = %v, want one write %v", r.calls, want)
		}
	})

	t.Run("lg, from its own source, name matched ignoring case", func(t *testing.T) {
		attach(t, "LG HDR WQHD")
		r := &recorder{}
		if err := (Write{0xF4, 0x50, 0xD0}).Do(t.Context(), r.run, "lg hdr wqhd"); err != nil {
			t.Fatalf("Do: %v", err)
		}
		// One write and nothing else: the monitor is never read back.
		want := []string{"ddcutil", "setvcp", "0xF4", "0xD0", "--noverify",
			"--i2c-source-addr=0x50", "--model", "LG HDR WQHD"}
		if len(r.calls) != 1 || !slices.Equal(r.calls[0], want) {
			t.Errorf("calls = %v, want one write %v", r.calls, want)
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
		r := &recorder{err: boom}
		if err := (Write{0xF4, 0x50, 0xD0}).Do(t.Context(), r.run, "LG HDR WQHD"); !errors.Is(err, boom) {
			t.Fatalf("Do = %v, want the write error", err)
		}
		if len(r.calls) != 1 {
			t.Errorf("calls = %v, want one", r.calls)
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
	if got := attachedNames(); !slices.Equal(got, []string{"LG HDR WQHD"}) {
		t.Errorf("attachedNames = %q, want only the LG", got)
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
