// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// service_test.go: validation and rendering for the service subcommand (SPEC §5.6).
// These tests never touch /etc or ~/.config and never fork systemctl: they
// exercise the pure validators and the renderers only.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

const testToken = "0123456789abcdef0123456789abcdef"

func mustReadServiceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := serviceFiles.ReadFile(name)
	if err != nil {
		t.Fatalf("reading embedded %s: %v", name, err)
	}
	return string(b)
}

func renderForTest(t *testing.T, tplName, bin string, args []string) string {
	t.Helper()
	unit, err := renderUnit(mustReadServiceFile(t, tplName), bin, args)
	if err != nil {
		t.Fatalf("renderUnit(%s, %q, %v): %v", tplName, bin, args, err)
	}
	return unit
}

// TestUnitContent pins the canonical units' hardening and wiring: what
// install serves must match what SPEC §6.4 promises.
func TestUnitContent(t *testing.T) {
	serve := mustReadServiceFile(t, "units/soft-kvm-serve.service")
	for _, want := range []string{
		"DynamicUser=yes",
		"NoNewPrivileges=yes",
		"CapabilityBoundingSet=",
		"StateDirectory=soft-kvm",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"EnvironmentFile=/etc/soft-kvm/env",
	} {
		if !strings.Contains(serve, want+"\n") && !strings.HasSuffix(serve, want) {
			t.Errorf("serve unit lacks %q", want)
		}
	}

	agent := mustReadServiceFile(t, "units/soft-kvm.service")
	if !strings.Contains(agent, "EnvironmentFile=%h/.config/soft-kvm/env") {
		t.Errorf("agent unit lacks EnvironmentFile=%%h/.config/soft-kvm/env")
	}
	if !strings.Contains(agent, "WantedBy=default.target") {
		t.Errorf("agent unit must install into default.target, not graphical-session.target")
	}
	if strings.Contains(agent, "graphical-session") {
		t.Errorf("agent unit must not reference graphical-session (it starts without a session)")
	}
}

// TestNoUnresolvedTokens renders every template with placeholder
// substitutions and pins that no placeholder survives.
func TestNoUnresolvedTokens(t *testing.T) {
	serveUnit := renderForTest(t, "units/soft-kvm-serve.service", serveBinPath, []string{"8700"})
	agentUnit := renderForTest(t, "units/soft-kvm.service", "/home/u/soft-kvm", []string{"--trigger", "046d:c52b"})
	serveEnv := renderEnv(mustReadServiceFile(t, "deploy/env.serve.example"), testToken)
	connectEnv := renderEnv(mustReadServiceFile(t, "deploy/env.connect.example"), testToken)

	for name, rendered := range map[string]string{
		"serve unit":  serveUnit,
		"agent unit":  agentUnit,
		"serve env":   serveEnv,
		"connect env": connectEnv,
	} {
		for _, ph := range []string{"__SOFTKVM_BIN__", "__ARGS__", "__SOFTKVM_TOKEN__"} {
			if strings.Contains(rendered, ph) {
				t.Errorf("%s still contains unresolved placeholder %s", name, ph)
			}
		}
	}
}

// TestSystemdAnalyzeVerify runs systemd-analyze verify on the rendered
// units when it is on PATH (--user for the agent unit). A missing
// EnvironmentFile is not an error, so any output or non-zero exit fails.
// verify requires the ExecStart binary to exist and, in system mode,
// loads the host's unit tree via [Install] — so the serve unit is
// verified against an empty --root with stub targets and a real binary
// at the baked path, and the agent unit (whose --user mode rejects
// --root) is rendered with the test binary itself as ExecStart.
func TestSystemdAnalyzeVerify(t *testing.T) {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze not on PATH")
	}
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("serve", func(t *testing.T) {
		root := t.TempDir()
		unitDir := filepath.Join(root, "etc/systemd/system")
		if err := os.MkdirAll(unitDir, 0755); err != nil {
			t.Fatal(err)
		}
		// verify needs the transaction's target units to exist; empty
		// stubs keep the check about our unit, not the host's tree.
		for _, name := range []string{
			"sysinit.target", "multi-user.target", "basic.target",
			"sockets.target", "timers.target", "paths.target",
			"slices.target", "network-online.target", "default.target",
		} {
			stub := "[Unit]\nDescription=stub for systemd-analyze verify\n"
			if err := os.WriteFile(filepath.Join(unitDir, name), []byte(stub), 0644); err != nil {
				t.Fatal(err)
			}
		}
		binDir := filepath.Join(root, "usr/local/bin")
		if err := os.MkdirAll(binDir, 0755); err != nil {
			t.Fatal(err)
		}
		binBytes, err := os.ReadFile(testBin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, "soft-kvm"), binBytes, 0755); err != nil {
			t.Fatal(err)
		}
		unit := renderForTest(t, "units/soft-kvm-serve.service", serveBinPath, nil)
		if err := os.WriteFile(filepath.Join(unitDir, "soft-kvm-serve.service"), []byte(unit), 0644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("systemd-analyze", "verify", "--root="+root,
			"/etc/systemd/system/soft-kvm-serve.service")
		out, err := cmd.CombinedOutput()
		if len(out) > 0 || err != nil {
			t.Errorf("systemd-analyze verify: exit=%v output=%s", err, out)
		}
	})

	t.Run("agent", func(t *testing.T) {
		unit := renderForTest(t, "units/soft-kvm.service", testBin, nil)
		path := filepath.Join(t.TempDir(), "soft-kvm.service")
		if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("systemd-analyze", "verify", "--user", path)
		out, err := cmd.CombinedOutput()
		if len(out) > 0 || err != nil {
			t.Errorf("systemd-analyze verify --user: exit=%v output=%s", err, out)
		}
	})
}

func TestServeInstallRequiresRoot(t *testing.T) {
	err := validateServeInstall(1000, testToken, []string{"8700"})
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("non-zero euid: got %v, want a refusal naming root", err)
	}
	if err := validateServeInstall(0, testToken, []string{"8700"}); err != nil {
		t.Fatalf("euid 0 with valid token and address: got %v, want nil", err)
	}
}

func TestServeInstallRequiresToken(t *testing.T) {
	for _, token := range []string{"", "short"} {
		if err := validateServeInstall(0, token, []string{"8700"}); !errors.Is(err, errToken) {
			t.Fatalf("token %q: got %v, want errToken", token, err)
		}
	}
}

func TestConnectInstallRefusesRoot(t *testing.T) {
	err := validateConnectInstall(0, testToken, []string{"--trigger", "046d:c52b"})
	if err == nil || !strings.Contains(err.Error(), "regular user") {
		t.Fatalf("euid 0: got %v, want a refusal naming the regular user", err)
	}
	if err := validateConnectInstall(1000, testToken, []string{"--trigger", "046d:c52b"}); err != nil {
		t.Fatalf("non-root with valid trigger: got %v, want nil", err)
	}
}

func TestConnectInstallRequiresTrigger(t *testing.T) {
	if err := validateConnectInstall(1000, testToken, nil); err == nil ||
		!strings.Contains(err.Error(), "--trigger") {
		t.Fatalf("no trigger: got %v, want a refusal naming --trigger", err)
	}
	// A --trigger inside a switch-command argv must not satisfy the
	// requirement: only the parsed flag counts.
	err := validateConnectInstall(1000, testToken, []string{"--", "somecmd", "--trigger", "foo"})
	if err == nil || !strings.Contains(err.Error(), "--trigger") {
		t.Fatalf("trigger inside switch-command argv: got %v, want a refusal naming --trigger", err)
	}
}

func TestServeInstallValidatesAddress(t *testing.T) {
	for _, addr := range []string{"notaport", "99999"} {
		if err := validateServeInstall(0, testToken, []string{addr}); err == nil {
			t.Fatalf("address %q: got nil, want the same refusal serve itself gives", addr)
		}
	}
	if _, _, err := serveListenAddr("99999"); err == nil {
		t.Fatal("serveListenAddr(99999): got nil, want port-range refusal")
	}
}

func TestQuoteSystemdArg(t *testing.T) {
	cases := []struct {
		name    string
		arg     string
		want    string
		wantErr bool
	}{
		{name: "plain token unchanged", arg: "ddcutil", want: "ddcutil"},
		{name: "digits", arg: "8700", want: "8700"},
		{name: "spaces wrapped", arg: "hello world", want: `"hello world"`},
		{name: "double quote escaped", arg: `say "hi"`, want: `"say \"hi\""`},
		{name: "backslash escaped when wrapped", arg: `a\b c`, want: `"a\\b c"`},
		{name: "single quote wrapped", arg: "it's", want: `"it's"`},
		{name: "percent doubled", arg: "50%", want: "50%%"},
		{name: "dollar doubled inside word", arg: "x${FOO}y", want: "x$${FOO}y"},
		{name: "standalone semicolon escaped", arg: ";", want: `\;`},
		{name: "embedded semicolon untouched", arg: "a;b", want: "a;b"},
		{name: "newline refused", arg: "a\nb", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := quoteSystemdArg(tc.arg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %q, nil; want error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}

func TestRenderEnv(t *testing.T) {
	serve := renderEnv(mustReadServiceFile(t, "deploy/env.serve.example"), testToken)
	if !strings.Contains(serve, "SOFTKVM_TOKEN="+testToken) {
		t.Errorf("serve env lacks the substituted token line")
	}
	if !strings.Contains(serve, "# SOFTKVM_LOG=debug") {
		t.Errorf("serve env lost the commented SOFTKVM_LOG knob")
	}
	if strings.Contains(serve, "SOFTKVM_SERVER") {
		t.Errorf("serve env must not carry SOFTKVM_SERVER; the coordinator never reads it")
	}

	connect := renderEnv(mustReadServiceFile(t, "deploy/env.connect.example"), testToken)
	if !strings.Contains(connect, "SOFTKVM_TOKEN="+testToken) {
		t.Errorf("connect env lacks the substituted token line")
	}
	if !strings.Contains(connect, "# SOFTKVM_SERVER=desktop.lan:8700") {
		t.Errorf("connect env lost the commented SOFTKVM_SERVER knob")
	}
}

// TestBakesRawArgs pins that install bakes the raw argument tokens, never a
// re-serialisation of parsed flags: the parsed --state default read from the
// installer's environment must not leak into the unit, and token order is
// preserved.
func TestBakesRawArgs(t *testing.T) {
	rawArgs := []string{"--no-advertise", "8700"}
	if err := validateServeArgs(rawArgs); err != nil {
		t.Fatalf("validateServeArgs(%v): %v", rawArgs, err)
	}
	unit := renderForTest(t, "units/soft-kvm-serve.service", serveBinPath, rawArgs)
	execLine := ""
	for line := range strings.Lines(unit) {
		if strings.HasPrefix(line, "ExecStart=") {
			execLine = strings.TrimSpace(line)
		}
	}
	if execLine == "" {
		t.Fatal("rendered serve unit has no ExecStart=")
	}
	if strings.Contains(execLine, "--state") {
		t.Errorf("ExecStart bakes a --state the user never passed: %s", execLine)
	}
	want := "ExecStart=" + serveBinPath + " serve --no-advertise 8700"
	if execLine != want {
		t.Errorf("got %s, want %s (raw token order preserved)", execLine, want)
	}
}

// TestInstallGuidanceIsShellShaped pins the transcript shape the styling
// rule depends on: every line is a #-comment, a command at column 0, a
// continuation of a \-terminated command, or blank — and none of them
// wraps in an 80-column terminal.
func TestInstallGuidanceIsShellShaped(t *testing.T) {
	body := fmt.Sprintf(installGuidance, "/usr/local/bin/soft-kvm")
	if strings.Contains(body, "%!") {
		t.Errorf("installGuidance has a stray format verb: %s", body)
	}
	prev := ""
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\n")
		if n := utf8.RuneCountInString(line); n > 76 {
			t.Errorf("line is %d columns, it wraps in an 80-column terminal: %q", n, line)
		}
		switch {
		case line == "", strings.HasPrefix(line, "#"):
		case !strings.HasPrefix(line, " "):
		case strings.HasSuffix(prev, `\`):
		default:
			t.Errorf("line is neither comment, command nor continuation: %q", line)
		}
		prev = line
	}
}

// TestVerifyUnit pins the install's last step: an active unit is a success,
// anything else is an error naming what systemctl reported.
func TestVerifyUnit(t *testing.T) {
	saved, savedSettle, savedTail := runSystemctl, unitSettle, journalTail
	t.Cleanup(func() { runSystemctl, unitSettle, journalTail = saved, savedSettle, savedTail })
	unitSettle = 0
	journalTail = func(context.Context, []string, string) string { return "(journal)" }

	var got []string
	runSystemctl = func(_ context.Context, args ...string) (string, error) {
		got = args
		return "active\n", nil
	}
	if err := verifyUnit(t.Context(), []string{"--user"}, "soft-kvm"); err != nil {
		t.Errorf("an active unit reported %v", err)
	}
	want := []string{"--user", "is-active", "soft-kvm"}
	if !slices.Equal(got, want) {
		t.Errorf("systemctl called with %v, want %v", got, want)
	}

	runSystemctl = func(_ context.Context, _ ...string) (string, error) {
		return "failed\n", errors.New("exit status 3")
	}
	err := verifyUnit(t.Context(), nil, "soft-kvm-serve")
	if err == nil || !strings.Contains(err.Error(), `"failed"`) {
		t.Errorf("a dead unit reported %v, want an error naming the state", err)
	}
}
