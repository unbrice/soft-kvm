// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_linux_test.go: the ddcutil write-then-confirm sequence against a
// fake runner, and the lg vendor table.

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

func mustParse(t *testing.T, args ...string) *Switch {
	t.Helper()
	s, err := Parse(args)
	if err != nil {
		t.Fatalf("Parse(%v): %v", args, err)
	}
	return s
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

func TestLGTable(t *testing.T) {
	lg := tableByName("lg")
	if lg == nil {
		t.Fatal("no lg table on Linux")
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
