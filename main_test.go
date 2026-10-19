// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRequireToken(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Setenv("SOFTKVM_TOKEN", "")
		if _, err := requireToken(); !errors.Is(err, errToken) {
			t.Fatalf("got %v, want errToken", err)
		}
	})

	t.Run("too short", func(t *testing.T) {
		t.Setenv("SOFTKVM_TOKEN", strings.Repeat("a", tokenMinLen-1))
		if _, err := requireToken(); !errors.Is(err, errToken) {
			t.Fatalf("got %v, want errToken", err)
		}
	})

	t.Run("minimum accepted", func(t *testing.T) {
		want := strings.Repeat("a", tokenMinLen)
		t.Setenv("SOFTKVM_TOKEN", want)
		got, err := requireToken()
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q, nil", got, err, want)
		}
	})

	t.Run("long accepted", func(t *testing.T) {
		want := strings.Repeat("a", tokenGoodLen)
		t.Setenv("SOFTKVM_TOKEN", want)
		got, err := requireToken()
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q, nil", got, err, want)
		}
	})
}

func TestRejectExtraArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
		// wantErr is a substring of the expected error; empty means success.
		wantErr string
	}{
		{name: "none expected, none given", args: nil, want: 0},
		{name: "within limit", args: []string{"8700"}, want: 1},
		{name: "at limit", args: []string{"linux"}, want: 1},
		{name: "extra positional", args: []string{"8700", "8800"}, want: 1, wantErr: `unexpected extra argument "8800"`},
		{name: "extra when none expected", args: []string{"8700"}, want: 0, wantErr: `unexpected extra argument "8700"`},
		{
			name:    "flag after positional",
			args:    []string{"8700", "--state", "/tmp/x"},
			want:    1,
			wantErr: `"--state" follows a positional argument and would be ignored; flags must come first`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectExtraArgs("cmd", tc.args, tc.want)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("got error %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "one word", in: "take-screen", want: []string{"take-screen"}},
		{name: "words", in: "ddcutil setvcp 0xF4  0xD0", want: []string{"ddcutil", "setvcp", "0xF4", "0xD0"}},
		{name: "surrounding space", in: "  a b\t", want: []string{"a", "b"}},
		{name: "single quotes", in: `notify-send 'two words' x`, want: []string{"notify-send", "two words", "x"}},
		{name: "double quotes", in: `notify-send "two words"`, want: []string{"notify-send", "two words"}},
		{name: "quotes join a word", in: `a"b c"'d e'f`, want: []string{"ab cd ef"}},
		{name: "empty quoted argument", in: `a '' b`, want: []string{"a", "", "b"}},
		{name: "backslash space", in: `/opt/my\ dir/run x`, want: []string{"/opt/my dir/run", "x"}},
		{name: "escapes inside double quotes", in: `say "a \"b\" \\ \n"`, want: []string{"say", `a "b" \ \n`}},
		{name: "no expansion", in: `echo $HOME *.go $(id) a|b; c`, want: []string{"echo", "$HOME", "*.go", "$(id)", "a|b;", "c"}},
		{name: "single quotes are literal", in: `'a\b'`, want: []string{`a\b`}},
		{name: "empty", in: "", wantErr: true},
		{name: "empty quoted program", in: `''`, wantErr: true},
		{name: "empty quoted program with args", in: `"" arg`, wantErr: true},
		{name: "only space", in: "  \t", wantErr: true},
		{name: "unterminated single", in: `a 'b`, wantErr: true},
		{name: "unterminated double", in: `a "b`, wantErr: true},
		{name: "trailing backslash", in: `a\`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitCommand(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %q, nil; want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("got error %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// -A and -D are spellings of the long flags, repeat, and keep their order.
func TestConnectCommandFlags(t *testing.T) {
	fs, v := newConnectFlagSet()
	fs.SetOutput(io.Discard)
	args := []string{"-D", "notify-send 'left'", "--on-deactivation", "logger bye", "-A", "notify-send 'hi there'", "--trigger", "kbd", "--declare-hid", "kbd 046d:c52b"}
	if err := parseArgs(fs, args); err != nil {
		t.Fatal(err)
	}
	if _, err := validateConnect(fs, v); err != nil {
		t.Fatal(err)
	}
	wantD := [][]string{{"notify-send", "left"}, {"logger", "bye"}}
	if !slices.EqualFunc(v.onDeactivation.argv, wantD, slices.Equal) {
		t.Errorf("on-deactivation = %q, want %q", v.onDeactivation.argv, wantD)
	}
	wantA := [][]string{{"notify-send", "hi there"}}
	if !slices.EqualFunc(v.onActivation.argv, wantA, slices.Equal) {
		t.Errorf("on-activation = %q, want %q", v.onActivation.argv, wantA)
	}

	// A command that does not split is a parse error, not a runtime surprise.
	fs, _ = newConnectFlagSet()
	fs.SetOutput(io.Discard)
	if err := parseArgs(fs, []string{"-D", "unterminated 'quote"}); !errors.Is(err, errUsage) {
		t.Errorf("unterminated quote: got %v, want errUsage", err)
	}
}

func parseConnect(t *testing.T, args ...string) (wiring, error) {
	t.Helper()
	fs, v := newConnectFlagSet()
	fs.SetOutput(io.Discard)
	// A trigger every case needs, declared with no channel; a --trigger in
	// args comes later and wins.
	args = append([]string{"--trigger", "trig", "--declare-hid", "trig 046d:c52b"}, args...)
	if err := parseArgs(fs, args); err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	return validateConnect(fs, v)
}

func TestValidateConnect(t *testing.T) {
	const lg = `"LG HDR WQHD" dp1 lg`
	cases := []struct {
		name    string
		args    []string
		wantErr string // substring; empty means success
	}{
		{name: "the full wiring", args: []string{
			"--declare-screen", lg, "--declare-hid", "keyboard 046d:c548:1 host=1", "--declare-hid", "mouse 046d:c548:2 host=1",
			"-A", "take-screen", "-A", "take-hid", "-D", "notify-send left"}},
		{name: "no command at all", args: nil},
		{name: "only -D", args: []string{"-D", "notify-send left"}},
		{name: "a device declared without take-hid still gives it away", args: []string{
			"--declare-hid", "mouse 046d:c548:2 host=1"}},
		{name: "take-screen alone", args: []string{"--declare-screen", lg, "-A", "take-screen"}},

		{name: "take-screen without a screen", args: []string{"-A", "take-screen"}, wantErr: "take-screen needs at least one --declare-screen"},
		{name: "a screen without take-screen", args: []string{"--declare-screen", lg}, wantErr: "does nothing without -A take-screen"},
		{name: "a screen with only take-hid", args: []string{"--declare-screen", lg, "--declare-hid", "m 046d:c548:2 host=1", "-A", "take-hid"}, wantErr: "does nothing without -A take-screen"},
		{name: "take-hid without a device", args: []string{"-A", "take-hid"}, wantErr: "take-hid needs at least one --declare-hid"},
		{name: "take-screen in -D", args: []string{"--declare-screen", lg, "-A", "take-screen", "-D", "take-screen"}, wantErr: "belongs in -A"},
		{name: "take-hid in -D", args: []string{"-D", "take-hid"}, wantErr: "belongs in -A"},
		{name: "take-screen with an argument", args: []string{"--declare-screen", lg, "-A", "take-screen dp1"}, wantErr: "takes no argument"},
		{name: "take-hid with an argument", args: []string{"--declare-hid", "m 046d:c548:2 host=1", "-A", "take-hid mouse"}, wantErr: "takes no argument"},

		{name: "screen declaration that does not parse", args: []string{"--declare-screen", "LG vga", "-A", "take-screen"}, wantErr: "--declare-screen: invalid input"},
		{name: "screen declaration with one word", args: []string{"--declare-screen", "LG", "-A", "take-screen"}, wantErr: "--declare-screen"},
		{name: "device without its channel", args: []string{"--declare-hid", "m 046d:c548:2", "-A", "take-hid"}, wantErr: "--declare-hid"},
		{name: "device with a bad name", args: []string{"--declare-hid", "my-mouse! 046d:c548:2 host=1", "-A", "take-hid"}, wantErr: "invalid device name"},
		{name: "a monitor declared twice", args: []string{"--declare-screen", "LG dp1", "--declare-screen", "lg hdmi1", "-A", "take-screen"}, wantErr: "declared twice"},
		{name: "a device declared twice", args: []string{"--declare-hid", "m 046d:c548:2 host=1", "--declare-hid", "m 046d:c548:3 host=2", "-A", "take-hid"}, wantErr: "declared twice"},
		{name: "a monitor and a device may share a name", args: []string{
			"--declare-screen", "desk dp1", "--declare-hid", "desk 046d:c548:2 host=1", "-A", "take-screen", "-A", "take-hid"}},
		{name: "a stray positional", args: []string{"--", "take-screen"}, wantErr: "-A take-screen"},

		{name: "no trigger", args: []string{"--trigger", ""}, wantErr: "--trigger is required"},
		{name: "a trigger by address", args: []string{"--trigger", "046d:c52b"}, wantErr: "no --declare-hid has that name"},
		{name: "two triggers by their declared names", args: []string{
			"--trigger", "keyboard,trig", "--declare-hid", "keyboard 046d:c548:1 host=1"}},
		{name: "a trigger naming no declaration", args: []string{
			"--trigger", "mouse", "--declare-hid", "keyboard 046d:c548:1 host=1"}, wantErr: `--trigger "mouse"`},
		{name: "take-hid with only a device without its channel", args: []string{"-A", "take-hid"}, wantErr: "with its host=N"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseConnect(t, c.args...)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, c.wantErr)
			}
		})
	}
}

func TestValidateConnectWiring(t *testing.T) {
	w, err := parseConnect(t,
		"--declare-screen", `"LG HDR WQHD" usbc1 lg`, "--declare-screen", `DELL\ U2720Q hdmi1`,
		"--declare-hid", "keyboard 046d:c548:1 host=1", "--declare-hid", "mouse 046d:b035 host=3",
		"-A", "take-screen", "-A", "take-hid")
	if err != nil {
		t.Fatal(err)
	}
	// The helper's own trigger declaration comes first.
	if !w.takes || len(w.screens) != 2 || len(w.hids) != 3 || w.detector == nil {
		t.Fatalf("wiring = %+v", w)
	}
	if got := w.screens[0]; got.Name != "LG HDR WQHD" || got.Write.String() != "F4:50=D1" {
		t.Errorf("screen 0 = %+v", got)
	}
	if got := w.screens[1]; got.Name != `DELL U2720Q` || got.Write.String() != "60:51=11" {
		t.Errorf("screen 1 = %+v", got)
	}
	if got := w.hids[2]; got.Name != "mouse" || got.Target.Slot != 0 || got.Channel != 3 {
		t.Errorf("hid 2 = %+v", got)
	}

	w, err = parseConnect(t, "--declare-hid", "mouse 046d:c548:2 host=1")
	if err != nil || w.takes {
		t.Fatalf("a device that is only given away: %+v, %v; want no takes", w, err)
	}
}

// TestParseArgs pins that -h/--help is a success and a bad flag is not:
// flag.Parse reports both as an error, and only one of them is one.
func TestParseArgs(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("id", "", "identity")
		return fs
	}
	if err := parseArgs(newFS(), []string{"--id", "linux"}); err != nil {
		t.Errorf("a valid parse returned %v", err)
	}
	if err := parseArgs(newFS(), []string{"--help"}); !errors.Is(err, errHelp) {
		t.Errorf("--help returned %v, want errHelp", err)
	}
	if err := parseArgs(newFS(), []string{"-h"}); !errors.Is(err, errHelp) {
		t.Errorf("-h returned %v, want errHelp", err)
	}
	if err := parseArgs(newFS(), []string{"--nope"}); !errors.Is(err, errUsage) {
		t.Errorf("an unknown flag returned %v, want errUsage", err)
	}
}

// TestPrintFlags pins the double-dash spelling the documentation uses, and
// that a backquoted word in the usage names the flag's argument — the
// mistake that rendered --trigger as "-trigger soft-kvm detect".
func TestPrintFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("trigger", "", "required: comma-separated `VID:PID` filters")
	fs.Bool("no-advertise", false, "skip mDNS advertisement")
	fs.Duration("settle", 2*time.Second, "attach must persist this long")
	var buf bytes.Buffer
	printFlags(fs, &buf)
	out := buf.String()
	for _, want := range []string{"  --trigger VID:PID", "  --no-advertise", "(default 2s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("printFlags omitted %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n  -trigger") {
		t.Errorf("single-dash spelling leaked into the flag list:\n%s", out)
	}
	if strings.Contains(out, "default false") {
		t.Errorf("a false default is noise, not information:\n%s", out)
	}
}

// TestConsoleHandler pins the terminal log line: a clock, a fixed-width
// level, the message, then key=value attrs — and no escape codes when the
// writer is not a terminal.
func TestConsoleHandler(t *testing.T) {
	var buf bytes.Buffer
	var level slog.LevelVar
	log := slog.New(newConsoleHandler(&buf, &level, false))
	log.Info("serving", "addr", ":8700", "instance", "desk top")
	out := buf.String()
	if !strings.HasSuffix(out, `INFO  serving addr=:8700 instance="desk top"`+"\n") {
		t.Errorf("unexpected console line: %q", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape codes written to a non-terminal: %q", out)
	}
	if !regexp.MustCompile(`^\d\d:\d\d:\d\d `).MatchString(out) {
		t.Errorf("line does not open with a clock: %q", out)
	}

	// Debug is dropped at the default level, and WithAttrs/WithGroup carry.
	buf.Reset()
	log.Debug("quiet")
	if buf.Len() != 0 {
		t.Errorf("debug logged at info level: %q", buf.String())
	}
	buf.Reset()
	log.With("id", "linux").WithGroup("hid").Info("attached", "vid", "046d")
	if got := buf.String(); !strings.Contains(got, "attached id=linux hid.vid=046d") {
		t.Errorf("WithAttrs/WithGroup lost: %q", got)
	}

	// Colour marks severity, and only when asked.
	buf.Reset()
	slog.New(newConsoleHandler(&buf, &level, true)).Error("boom")
	if got := buf.String(); !strings.Contains(got, ansiRed+"ERROR"+ansiReset) {
		t.Errorf("error level not coloured: %q", got)
	}
}
