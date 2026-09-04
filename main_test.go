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

func TestSplitSwitchCommands(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    [][]string
		wantErr bool
	}{
		{name: "empty", args: nil, want: nil},
		{name: "one", args: []string{"ddcutil", "setvcp", "0xF4"}, want: [][]string{{"ddcutil", "setvcp", "0xF4"}}},
		{
			name: "two",
			args: []string{"ddcutil", "setvcp", "0xF4", "--", "usb-switch", "2"},
			want: [][]string{{"ddcutil", "setvcp", "0xF4"}, {"usb-switch", "2"}},
		},
		{name: "lone separator", args: []string{"--", "cmd"}, wantErr: true},
		{name: "trailing separator", args: []string{"cmd", "--"}, wantErr: true},
		{name: "empty middle", args: []string{"a", "--", "--", "b"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitSwitchCommands(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %v, nil; want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("got error %v", err)
			}
			if !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
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
