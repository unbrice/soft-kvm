// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_linux_test.go: the ddcutil write-then-confirm sequence against a
// fake runner.

package screen

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// scriptRunner answers the write with nil and each read from reads, repeating
// the last entry once the script runs out.
type scriptRunner struct {
	calls    [][]string
	writeErr error
	reads    []readResult
}

type readResult struct {
	out string
	err error
}

func (r *scriptRunner) run(_ context.Context, argv []string) ([]byte, error) {
	r.calls = append(r.calls, argv)
	if slices.Contains(argv, "setvcp") {
		return nil, r.writeErr
	}
	n := 0
	for _, c := range r.calls {
		if slices.Contains(c, "getvcp") {
			n++
		}
	}
	res := r.reads[min(n, len(r.reads))-1]
	return []byte(res.out), res.err
}

func shrinkConfirm(t *testing.T) {
	t.Helper()
	w, p := confirmWindow, confirmPoll
	confirmWindow, confirmPoll = 40*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { confirmWindow, confirmPoll = w, p })
}

func TestDoWritesTheTable(t *testing.T) {
	shrinkConfirm(t)
	silent := []readResult{{err: errors.New("no DDC reply")}}

	t.Run("standard", func(t *testing.T) {
		r := &scriptRunner{reads: silent}
		if err := mustParse(t, "hdmi1").Do(t.Context(), r.run); err != nil {
			t.Fatalf("Do: %v", err)
		}
		want := []string{"ddcutil", "setvcp", "0x60", "0x11", "--noverify"}
		if !slices.Equal(r.calls[0], want) {
			t.Errorf("write = %v, want %v", r.calls[0], want)
		}
	})

	t.Run("lg with a display", func(t *testing.T) {
		r := &scriptRunner{reads: silent}
		if err := mustParse(t, "dp1", "lg", "display=LG ULTRAWIDE").Do(t.Context(), r.run); err != nil {
			t.Fatalf("Do: %v", err)
		}
		want := []string{"ddcutil", "setvcp", "0xF4", "0xD0", "--noverify",
			"--i2c-source-addr=0x50", "--model", "LG ULTRAWIDE"}
		if !slices.Equal(r.calls[0], want) {
			t.Errorf("write = %v, want %v", r.calls[0], want)
		}
		read := []string{"ddcutil", "--terse", "getvcp", "60", "--model", "LG ULTRAWIDE"}
		if !slices.Equal(r.calls[1], read) {
			t.Errorf("read = %v, want %v", r.calls[1], read)
		}
	})
}

func TestDoConfirm(t *testing.T) {
	shrinkConfirm(t)
	still := readResult{out: "VCP 60 SNC x0f\n"}

	t.Run("silence after a few polls is the receipt", func(t *testing.T) {
		r := &scriptRunner{reads: []readResult{still, still, {err: errors.New("no DDC reply")}}}
		if err := mustParse(t, "hdmi1").Do(t.Context(), r.run); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if len(r.calls) != 4 {
			t.Errorf("expected 1 write + 3 reads, got %v", r.calls)
		}
	})

	t.Run("a monitor answering on every input reports the target", func(t *testing.T) {
		r := &scriptRunner{reads: []readResult{still, {out: "VCP 60 SNC x11\n"}}}
		if err := mustParse(t, "hdmi1").Do(t.Context(), r.run); err != nil {
			t.Fatalf("Do: %v", err)
		}
	})

	t.Run("still on this input past the window fails", func(t *testing.T) {
		r := &scriptRunner{reads: []readResult{still}}
		err := mustParse(t, "hdmi1").Do(t.Context(), r.run)
		if err == nil || !strings.Contains(err.Error(), "still on this input") {
			t.Fatalf("Do error = %v, want a still-on-this-input failure", err)
		}
	})

	t.Run("a failed write is reported with no read", func(t *testing.T) {
		boom := errors.New("ddcutil requires module i2c-dev")
		r := &scriptRunner{writeErr: boom, reads: []readResult{still}}
		if err := mustParse(t, "hdmi1").Do(t.Context(), r.run); !errors.Is(err, boom) {
			t.Fatalf("Do error = %v, want the write error", err)
		}
		if len(r.calls) != 1 {
			t.Errorf("expected the write only, got %v", r.calls)
		}
	})

	t.Run("cancellation is not a receipt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		r := &scriptRunner{reads: []readResult{{err: context.Canceled}}}
		run := func(c context.Context, argv []string) ([]byte, error) {
			out, err := r.run(c, argv)
			if slices.Contains(argv, "getvcp") {
				cancel() // the read died because the agent is shutting down
			}
			return out, err
		}
		if err := mustParse(t, "hdmi1").Do(ctx, run); !errors.Is(err, context.Canceled) {
			t.Fatalf("Do error = %v, want context.Canceled", err)
		}
	})
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

// Switching to the port the monitor already shows succeeds the same way
// through either table: the read reports the target.
func TestDoToOwnPort(t *testing.T) {
	shrinkConfirm(t)
	for _, args := range [][]string{{"dp1"}, {"dp1", "lg"}} {
		r := &scriptRunner{reads: []readResult{{out: "VCP 60 SNC x0f\n"}}}
		if err := mustParse(t, args...).Do(t.Context(), r.run); err != nil {
			t.Errorf("screen-switch %v on dp1: %v", args, err)
		}
	}
}
