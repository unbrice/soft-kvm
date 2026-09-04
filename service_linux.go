// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// service_linux.go: the service subcommand — systemd install/print/uninstall (SPEC §5.6).

package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/unbrice/soft-kvm/detect"
)

// Canonical unit templates and env-file templates, embedded so the
// installed binary renders what packaging renders (SPEC §6.1, §6.4).
//
//go:embed units/soft-kvm.service units/soft-kvm-serve.service deploy/env.serve.example deploy/env.connect.example
var serviceFiles embed.FS

// serveBinPath is where install serve copies the binary; the coordinator
// unit bakes it (its ProtectHome=yes rules out a binary under home).
const serveBinPath = "/usr/local/bin/soft-kvm"

const (
	serveUnitPath   = "/etc/systemd/system/soft-kvm-serve.service"
	serveEnvDir     = "/etc/soft-kvm"
	connectUnitPath = ".config/systemd/user/soft-kvm.service"
	connectEnvDir   = ".config/soft-kvm"
)

// runSystemctl is the systemctl seam: every install/uninstall path
// validates before calling it, so the tests never fork it. It returns the
// output as well as the error because the install verification reads what
// `is-active` printed, which is a word, not an exit status.
var runSystemctl = func(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// unitSettle is how long install waits before asking whether the unit is
// still up. `systemctl start` returns as soon as a Type=simple service has
// forked, so a unit that exits immediately — a rejected token, a device
// that is not there — reports success and only then starts crash-looping
// against Restart=always. A variable so the tests need not wait it out.
var unitSettle = 2 * time.Second

// verifyUnit reports whether the unit is running, and prints the tail of
// its journal when it is not: the moment the person who typed install is
// still watching is the moment to tell them.
func verifyUnit(ctx context.Context, scope []string, unit string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(unitSettle):
	}
	out, _ := runSystemctl(ctx, append(slices.Clone(scope), "is-active", unit)...)
	state := strings.TrimSpace(out)
	if state == "active" {
		slog.Info("service is running", "unit", unit)
		return nil
	}
	fmt.Fprintln(os.Stderr, journalTail(ctx, scope, unit))
	return fmt.Errorf("%s is %q, not active — the log above says why", unit, state)
}

// journalTail is the unit's last log lines, for an install that did not
// take. journalctl takes --user in the same spelling systemctl does. A seam
// like runSystemctl, so the tests fork nothing.
var journalTail = func(ctx context.Context, scope []string, unit string) string {
	args := append(slices.Clone(scope), "--no-pager", "-n", "20", "-u", unit)
	out, err := exec.CommandContext(ctx, "journalctl", args...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return fmt.Sprintf("(journalctl -u %s failed: %v)", unit, err)
	}
	return strings.TrimRight(string(out), "\n")
}

func serviceCmd(ctx context.Context) error {
	args := os.Args[2:]
	if len(args) == 0 {
		serviceUsage()
		return errUsage
	}
	op, rest := args[0], args[1:]
	switch op {
	case "install":
		return serviceInstall(ctx, rest)
	case "print":
		return servicePrint(rest)
	case "uninstall":
		return serviceUninstall(ctx, rest)
	default:
		serviceUsage()
		return errUsage
	}
}

func serviceUsage() {
	fmt.Fprintln(os.Stderr, `usage: soft-kvm service <install|print|uninstall> [serve|connect] [args]

  install [serve|connect] [args]   install and enable the service (no args: two-step guidance)
  print [serve|connect] [args]     render the unit to stdout for review (no euid/token checks)
  uninstall [serve|connect]        disable and remove the unit (env files and the binary stay)`)
}

// selfExecutable resolves the running binary's absolute path:
// os.Executable, with exec.LookPath(os.Args[0]) as the fallback.
func selfExecutable() (string, error) {
	if p, err := os.Executable(); err == nil {
		return p, nil
	}
	p, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", fmt.Errorf("cannot resolve the running binary's path: %w", err)
	}
	return p, nil
}

// quoteSystemdArg quotes one raw argument for a unit's ExecStart line:
// plain tokens pass through; tokens containing whitespace or quotes are
// wrapped in "..." with \ and " escaped; % is always doubled (specifier
// expansion); $ is always doubled (${VAR} expands even inside quotes); a
// standalone ; becomes \; (command separator). A newline has no unit-file
// representation and is refused. The escaping applies to baked arguments
// only — template specifiers such as %h pass through renderUnit untouched.
func quoteSystemdArg(arg string) (string, error) {
	if strings.Contains(arg, "\n") {
		return "", fmt.Errorf("argument %q contains a newline, which cannot be represented in a systemd unit", arg)
	}
	arg = strings.NewReplacer("%", "%%", "$", "$$").Replace(arg)
	if arg == ";" {
		return `\;`, nil
	}
	if strings.ContainsAny(arg, " \t\r\v\f\"'") {
		arg = strings.ReplaceAll(arg, `\`, `\\`)
		arg = strings.ReplaceAll(arg, `"`, `\"`)
		return `"` + arg + `"`, nil
	}
	return arg, nil
}

// renderUnit substitutes __SOFTKVM_BIN__ and __ARGS__ (each raw argument
// through quoteSystemdArg, space-joined). Anything else in the template —
// including the %h specifier — passes through untouched.
func renderUnit(tpl, bin string, args []string) (string, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		q, err := quoteSystemdArg(a)
		if err != nil {
			return "", err
		}
		quoted[i] = q
	}
	return strings.NewReplacer(
		"__SOFTKVM_BIN__", bin,
		"__ARGS__", strings.Join(quoted, " "),
	).Replace(tpl), nil
}

// renderEnv substitutes __SOFTKVM_TOKEN__.
func renderEnv(tpl, token string) string {
	return strings.ReplaceAll(tpl, "__SOFTKVM_TOKEN__", token)
}

// validateServeArgs parses serve's raw argument tokens with the same flag
// set serveCmd uses and applies everything serve validates after parsing:
// the positional-address normalisation and the extra-argument rejection.
// The raw tokens are what install bakes; this parse exists only so install
// never accepts a syntax the runtime would reject.
func validateServeArgs(args []string) error {
	fs, _, err := newServeFlagSet()
	if err != nil {
		return err
	}
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	rest := fs.Args()
	if err := rejectExtraArgs("serve", rest, 1); err != nil {
		return err
	}
	addr := ""
	if len(rest) > 0 {
		addr = rest[0]
	}
	if _, _, err := serveListenAddr(addr); err != nil {
		return err
	}
	return nil
}

// validateConnectArgs parses connect's raw argument tokens with the same
// flag set connectCmd uses and applies everything connect validates after
// parsing: a non-empty parsed --trigger (one inside a switch-command argv
// does not count), the VID:PID list, and the trailing switch-command
// chunks.
func validateConnectArgs(args []string) error {
	fs, v := newConnectFlagSet()
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if *v.trigger == "" {
		return errors.New("connect: --trigger is required (run `soft-kvm detect` to list VID:PIDs)")
	}
	if _, err := detect.NewDetector(*v.trigger); err != nil {
		return err
	}
	_, err := splitSwitchCommands(fs.Args())
	return err
}

// validateServeInstall is pure — everything it reads is a parameter — so
// the tests exercise it as any user, with no environment.
func validateServeInstall(euid int, token string, args []string) error {
	if euid != 0 {
		return fmt.Errorf("service install serve must run as root: it writes %s, %s/env and %s", serveBinPath, serveEnvDir, serveUnitPath)
	}
	if _, err := checkToken(token); err != nil {
		return err
	}
	return validateServeArgs(args)
}

// validateConnectInstall is pure — see validateServeInstall.
func validateConnectInstall(euid int, token string, args []string) error {
	if euid == 0 {
		return errors.New("service install connect must run as your regular user, not root: it writes a user unit and ~/.config/soft-kvm/env")
	}
	if _, err := checkToken(token); err != nil {
		return err
	}
	return validateConnectArgs(args)
}

// serviceInstall runs `soft-kvm service install [serve|connect] [args]`.
// All validation happens before any write or systemctl call.
func serviceInstall(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return printInstallGuidance()
	}
	kind, rawArgs := args[0], args[1:]
	switch kind {
	case "serve":
		return installServe(ctx, rawArgs)
	case "connect":
		return installConnect(ctx, rawArgs)
	default:
		serviceUsage()
		return errUsage
	}
}

// installGuidance is the two-step install walkthrough, shaped as a shell
// transcript: the prose is #-comments and the commands sit at column 0,
// so the output survives a pipe into a file and can be edited into a
// setup script. %s is the resolved path of the running binary — sudo's
// secure_path would not find a freshly built ./soft-kvm.
const installGuidance = `# soft-kvm runs as two services: one coordinator, one agent per host.
# Both read the shared secret from SOFTKVM_TOKEN.

# GENERATE A SHARED SECRET once, then copy this value to your other
# hosts — re-running this line there would generate a different one.
export SOFTKVM_TOKEN=$(openssl rand -hex 32)

# CHOOSE YOUR SETTINGS AND TRY THEM before baking them into units;
# soft-kvm serve --help and soft-kvm connect --help list the flags.
soft-kvm detect                      # lists the VID:PID for --trigger
soft-kvm serve                       # then, in one terminal
soft-kvm connect --trigger VID:PID   # and in another; Ctrl-C both after

# INSTALL COORDINATOR on an always-on host of your LAN, as root; hosts
# that only run an agent skip it. This command installs
# /usr/local/bin/soft-kvm, /etc/soft-kvm/env and the system unit.
sudo --preserve-env=SOFTKVM_TOKEN \
  %s service install serve

# INSTALL AGENT on every host, as your regular user. This command writes
# ~/.config/soft-kvm/env and a user unit that runs the binary you invoke
# it with.
/usr/local/bin/soft-kvm service install connect --trigger VID:PID

# Optional, once the services are installed:

# START THE AGENT AT BOOT, before anyone logs in.
loginctl enable-linger

# CHECK THAT BOTH SERVICES CAME UP.
systemctl status soft-kvm-serve
systemctl --user status soft-kvm
`

func printInstallGuidance() error {
	self, err := selfExecutable()
	if err != nil {
		return err
	}
	fmt.Print(detect.Dim(os.Stdout, fmt.Sprintf(installGuidance, self)))
	return nil
}

func installServe(ctx context.Context, rawArgs []string) error {
	token, err := requireToken()
	if err != nil {
		self, selfErr := selfExecutable()
		if selfErr != nil {
			return err
		}
		return fmt.Errorf("%w\nhint: run `sudo --preserve-env=SOFTKVM_TOKEN %s service install serve ...` so the token reaches the install", err, self)
	}
	if err := validateServeInstall(os.Geteuid(), token, rawArgs); err != nil {
		return err
	}

	self, err := selfExecutable()
	if err != nil {
		return err
	}
	if filepath.Clean(self) != serveBinPath {
		if err := copyExecutable(self, serveBinPath); err != nil {
			return fmt.Errorf("copying the binary to %s: %w", serveBinPath, err)
		}
		slog.Info("installed binary", "path", serveBinPath)
	} else {
		slog.Info("binary already installed", "path", serveBinPath)
	}

	if err := os.MkdirAll(serveEnvDir, 0755); err != nil {
		return err
	}
	envTpl, err := serviceFiles.ReadFile("deploy/env.serve.example")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(serveEnvDir, "env"), []byte(renderEnv(string(envTpl), token)), 0600); err != nil {
		return err
	}
	slog.Info("wrote env file", "path", filepath.Join(serveEnvDir, "env"), "mode", "0600", "note", "re-running install re-renders it from the template and discards hand edits (e.g. SOFTKVM_LOG)")

	unitTpl, err := serviceFiles.ReadFile("units/soft-kvm-serve.service")
	if err != nil {
		return err
	}
	unit, err := renderUnit(string(unitTpl), serveBinPath, rawArgs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(serveUnitPath, []byte(unit), 0644); err != nil {
		return err
	}

	if _, err := runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if _, err := runSystemctl(ctx, "enable", "--now", "soft-kvm-serve"); err != nil {
		return err
	}
	return verifyUnit(ctx, nil, "soft-kvm-serve")
}

func installConnect(ctx context.Context, rawArgs []string) error {
	token, err := requireToken()
	if err != nil {
		return err
	}
	if err := validateConnectInstall(os.Geteuid(), token, rawArgs); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	self, err := selfExecutable()
	if err != nil {
		return err
	}

	envTpl, err := serviceFiles.ReadFile("deploy/env.connect.example")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, connectEnvDir), 0755); err != nil {
		return err
	}
	envPath := filepath.Join(home, connectEnvDir, "env")
	if err := os.WriteFile(envPath, []byte(renderEnv(string(envTpl), token)), 0600); err != nil {
		return err
	}
	slog.Info("wrote env file", "path", envPath, "mode", "0600")

	unitTpl, err := serviceFiles.ReadFile("units/soft-kvm.service")
	if err != nil {
		return err
	}
	unit, err := renderUnit(string(unitTpl), self, rawArgs)
	if err != nil {
		return err
	}
	unitPath := filepath.Join(home, connectUnitPath)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return err
	}

	if _, err := runSystemctl(ctx, "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := runSystemctl(ctx, "--user", "enable", "--now", "soft-kvm"); err != nil {
		return err
	}
	if err := verifyUnit(ctx, []string{"--user"}, "soft-kvm"); err != nil {
		return err
	}
	fmt.Println("To start the agent at boot without logging in: loginctl enable-linger")
	return nil
}

// servicePrint renders the unit install would write — same argument
// validation and rendering — but performs no euid, token or on-disk checks
// and writes nothing: the unit goes to stdout for review or piping.
func servicePrint(args []string) error {
	if len(args) == 0 {
		serviceUsage()
		return errUsage
	}
	kind, rawArgs := args[0], args[1:]
	var (
		tplName string
		bin     string
	)
	switch kind {
	case "serve":
		if err := validateServeArgs(rawArgs); err != nil {
			return err
		}
		tplName = "units/soft-kvm-serve.service"
		bin = serveBinPath
	case "connect":
		if err := validateConnectArgs(rawArgs); err != nil {
			return err
		}
		tplName = "units/soft-kvm.service"
		self, err := selfExecutable()
		if err != nil {
			return err
		}
		bin = self
	default:
		serviceUsage()
		return errUsage
	}
	unitTpl, err := serviceFiles.ReadFile(tplName)
	if err != nil {
		return err
	}
	unit, err := renderUnit(string(unitTpl), bin, rawArgs)
	if err != nil {
		return err
	}
	fmt.Print(unit)
	return nil
}

// serviceUninstall disables and removes the unit; the env files and
// /usr/local/bin/soft-kvm are left untouched.
func serviceUninstall(ctx context.Context, args []string) error {
	if len(args) == 0 {
		serviceUsage()
		return errUsage
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	switch args[0] {
	case "serve":
		if _, err := runSystemctl(ctx, "disable", "--now", "soft-kvm-serve"); err != nil {
			return err
		}
		if err := removeIfExists(serveUnitPath); err != nil {
			return err
		}
		_, err := runSystemctl(ctx, "daemon-reload")
		return err
	case "connect":
		if _, err := runSystemctl(ctx, "--user", "disable", "--now", "soft-kvm"); err != nil {
			return err
		}
		if err := removeIfExists(filepath.Join(home, connectUnitPath)); err != nil {
			return err
		}
		_, err := runSystemctl(ctx, "--user", "daemon-reload")
		return err
	default:
		serviceUsage()
		return errUsage
	}
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// copyExecutable copies src to dest via a temp file in the same directory
// + rename, mode 0755, so a failed copy never leaves a partial binary at
// dest.
func copyExecutable(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".soft-kvm-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}
