// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// main.go: CLI dispatch and flag sets (SPEC §5, §11).

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/unbrice/soft-kvm/agent"
	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/detect"
	"github.com/unbrice/soft-kvm/discover"
	"github.com/unbrice/soft-kvm/identity"
	"github.com/unbrice/soft-kvm/model"
	"github.com/unbrice/soft-kvm/platform"
	"github.com/unbrice/soft-kvm/server"
)

// errUsage marks flag-parse failures: the FlagSet has already printed the
// error and its defaults, so main only sets the exit code.
var errUsage = errors.New("usage")

// errToken marks the missing-or-weak-SOFTKVM_TOKEN configuration error.
var errToken = errors.New("SOFTKVM_TOKEN is required")

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = serveCmd(ctx)
	case "activate":
		err = activateCmd(ctx)
	case "connect":
		err = connectCmd(ctx)
	case "detect":
		err = detectCmd(ctx)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, errToken) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: soft-kvm <command> [args]

  serve [IP:]PORT [--state PATH] [--instance NAME] [--no-advertise]
  activate ID [--server HOST:PORT] [--force]
  connect [flags] [-- SWITCH-CMD ARGS...]
  detect

Flags precede positional arguments. SOFTKVM_TOKEN (env) is required for
serve, activate and connect; detect needs no token. SOFTKVM_SERVER overrides
server discovery (SPEC §5).`)
}

// requireToken reads the shared secret from the environment — never a flag
// (SPEC §5). The TLS identity and the mDNS kh= fingerprint are both
// deterministic, salt-free functions of the token, so a weak token is one
// offline dictionary pass away from compromise (SPEC §9): refuse clearly
// weak ones, warn on short ones.
const (
	tokenMinLen  = 16
	tokenGoodLen = 32
)

func requireToken() (string, error) {
	token := os.Getenv("SOFTKVM_TOKEN")
	if token == "" {
		return "", errToken
	}
	if len(token) < tokenMinLen {
		return "", fmt.Errorf("%w: %d chars is too short, need %d+ (generate one, e.g. `openssl rand -hex 32`)", errToken, len(token), tokenMinLen)
	}
	if len(token) < tokenGoodLen {
		slog.Warn("SOFTKVM_TOKEN is short; prefer a generated token of 32+ chars (SPEC §9)", "length", len(token))
	}
	return token, nil
}

func serveCmd(ctx context.Context) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	statePath := fs.String("state", "/var/lib/soft-kvm/state.json", "persisted owner/epoch/since path")
	instance := fs.String("instance", "", "mDNS instance name (default: hostname)")
	noAdvertise := fs.Bool("no-advertise", false, "skip mDNS advertisement")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: soft-kvm serve [IP:]PORT [--state PATH] [--instance NAME] [--no-advertise]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[2:]); err != nil {
		return errUsage
	}
	token, err := requireToken()
	if err != nil {
		return err
	}

	addr := fs.Arg(0)
	if addr == "" {
		addr = "8700"
	}
	if _, err := strconv.Atoi(addr); err == nil {
		addr = ":" + addr
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %q", portStr)
	}
	if host == "" {
		addr = ":" + portStr
	}

	if *instance == "" {
		hn, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("hostname: %w", err)
		}
		*instance = hn
	}

	srv := server.NewServer(*statePath, token)
	if !*noAdvertise {
		stopAdv, err := discover.Advertise(*instance, port, identity.KeyFingerprint(token))
		if err != nil {
			// e.g. Avahi owning UDP 5353 despite SO_REUSEPORT (SPEC §6.4).
			slog.Warn("mDNS advertisement failed, continuing without it", "error", err)
		} else {
			defer stopAdv()
		}
	}
	slog.Info("serving", "addr", addr, "instance", *instance, "state", *statePath)
	return srv.Run(ctx, addr)
}

func activateCmd(ctx context.Context) error {
	fs := flag.NewFlagSet("activate", flag.ContinueOnError)
	serverFlag := fs.String("server", "", "server address HOST:PORT")
	force := fs.Bool("force", false, "claim even if the target has no live agent")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: soft-kvm activate ID [--server HOST:PORT] [--force]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[2:]); err != nil {
		return errUsage
	}
	token, err := requireToken()
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	if id == "" {
		fs.Usage()
		return errUsage
	}

	c, err := client.NewClient(token)
	if err != nil {
		return err
	}

	stateDir, err := platform.StateDir()
	if err != nil {
		return err
	}
	resolver := discover.NewResolver(filepath.Join(stateDir, "server"))

	// Try each candidate until one answers: a stale cache entry or an
	// unroutable advertised address should not end the attempt (SPEC §5.1).
	var base string
	var changed bool
	var lastErr error
	for candidate := range resolver.Resolve(ctx, *serverFlag, identity.KeyFingerprint(token)) {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ch, err := c.Claim(cctx, candidate, id, *force)
		cancel()
		if err != nil {
			if errors.Is(err, client.ErrNoLiveAgent) && !*force {
				return fmt.Errorf("no live agent for %q; re-run with --force to claim anyway", id)
			}
			slog.Debug("activate: candidate failed", "base", candidate, "error", err)
			lastErr = err
			continue
		}
		base, changed = candidate, ch
		resolver.Save(base) // the connection worked — cache the address (SPEC §5.1)
		break
	}
	if base == "" {
		if lastErr != nil {
			return lastErr
		}
		return ctx.Err()
	}

	st, err := c.State(ctx, base)
	if err != nil {
		return err
	}
	fmt.Printf("owner=%s epoch=%d changed=%v\n", st.Owner, st.Epoch, changed)
	return nil
}

func connectCmd(ctx context.Context) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	id := fs.String("id", platform.DefaultID, "claimed identity")
	serverFlag := fs.String("server", "", "server address HOST:PORT")
	trigger := fs.String("trigger", "", "comma-separated VID:PID filters for the trigger detector (USB receiver, optional Bluetooth keyboard); required — run `soft-kvm detect` to list VID:PIDs")
	settle := fs.Duration("settle", 2*time.Second, "attach must persist this long before claiming")
	confirm := fs.Duration("confirm", 4*time.Second, "how long check-cmd may keep succeeding before the switch counts as failed")
	switchRetries := fs.Int("switch-retries", 3, "re-runs of the switch command before giving up")
	checkTimeout := fs.Duration("check-timeout", 10*time.Second, "bound on one check-cmd run")
	switchTimeout := fs.Duration("switch-timeout", agent.DefaultSwitchTimeout, "bound on one SWITCH-CMD run — a hung I²C write must not freeze the agent (§4.3)")
	checkCmd := fs.String("check-cmd", platform.DefaultCheckCmd, "veto before the switch, receipt after it")
	notifyCmd := fs.String("notify-cmd", platform.DefaultNotifyCmd, "command run when the switch cannot be confirmed")

	var noGuards bool
	var displayMatch string
	if runtime.GOOS == "darwin" {
		fs.BoolVar(&noGuards, "no-guards", false, "disable macOS AC-power and display guards")
		fs.StringVar(&displayMatch, "display-match", "LG", "display name substring for the macOS guard")
	}

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: soft-kvm connect [flags] [-- SWITCH-CMD ARGS...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[2:]); err != nil {
		return errUsage
	}
	if *trigger == "" {
		fmt.Fprintln(os.Stderr, "connect: --trigger is required (run `soft-kvm detect` to list VID:PIDs)")
		return errUsage
	}
	token, err := requireToken()
	if err != nil {
		return err
	}

	detector, err := detect.NewHIDDetector(*trigger)
	if err != nil {
		return err
	}

	// The Linux desktop has no guards: NewGuard there ignores displayMatch and
	// is always ok (SPEC §6.1-6.2).
	guard := platform.NewGuard(displayMatch, noGuards)

	// The trailing SWITCH-CMD is an argv slice, never a shell string (SPEC §9).
	switchArgv := fs.Args()
	if len(switchArgv) == 0 {
		switchArgv = platform.DefaultSwitchCmd
	}

	c, err := client.NewClient(token)
	if err != nil {
		return err
	}

	machineCfg := model.MachineConfig{
		ID:             *id,
		Settle:         *settle,
		Confirm:        *confirm,
		SwitchRetries:  *switchRetries,
		RetrySpacing:   1 * time.Second,
		Cooldown:       5 * time.Second,
		BreakerWindow:  30 * time.Second,
		BreakerMax:     3,
		BreakerOpenFor: 60 * time.Second,
	}

	stateDir, err := platform.StateDir()
	if err != nil {
		return err
	}
	resolver := discover.NewResolver(filepath.Join(stateDir, "server"))

	cfg := agent.Config{
		ID:             *id,
		ExplicitServer: *serverFlag,
		KeyFP:          identity.KeyFingerprint(token),
		Detector:       detector,
		Guard:          guard,
		Client:         c,
		Runner:         platform.Run,
		Machine:        &machineCfg,
		AgentStatePath: filepath.Join(stateDir, "agent.json"),
		SwitchArgv:     switchArgv,
		CheckArgv:      platform.ShellArgv(*checkCmd),
		NotifyArgv:     platform.ShellArgv(*notifyCmd),
		CheckTimeout:   *checkTimeout,
		SwitchTimeout:  *switchTimeout,
		Resolver:       resolver,
	}

	return agent.Run(ctx, cfg)
}

func detectCmd(ctx context.Context) error {
	fs := flag.NewFlagSet("detect", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: soft-kvm detect")
	}
	if err := fs.Parse(os.Args[2:]); err != nil {
		return errUsage
	}
	if fs.NArg() > 0 {
		return errUsage
	}
	return detect.Run(ctx, os.Stdout)
}
