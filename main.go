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
)

// errUsage marks flag-parse failures: the FlagSet has already printed the
// error and its defaults, so main only sets the exit code.
var errUsage = errors.New("usage")

// errToken marks the missing-SOFTKVM_TOKEN configuration error.
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

Flags precede positional arguments. SOFTKVM_TOKEN (env) is always required;
SOFTKVM_SERVER overrides server discovery (SPEC §5).`)
}

// requireToken reads the shared secret from the environment — never a flag
// (SPEC §5).
func requireToken() (string, error) {
	if token := os.Getenv("SOFTKVM_TOKEN"); token != "" {
		return token, nil
	}
	return "", errToken
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

	srv := NewServer(*statePath, token)
	if !*noAdvertise {
		stopAdv, err := advertise(*instance, port)
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

	base, err := resolveServer(ctx, *serverFlag)
	if err != nil {
		return err
	}

	client := NewClient(token)
	changed, err := client.Claim(ctx, base, id, *force)
	if err != nil {
		if errors.Is(err, ErrNoLiveAgent) && !*force {
			return fmt.Errorf("no live agent for %q; re-run with --force to claim anyway", id)
		}
		if errors.Is(err, ErrUnauthorized) {
			return errors.New("token rejected")
		}
		return err
	}
	saveCachedServer(base) // the connection worked — cache the address (SPEC §5.1)

	state, err := client.State(ctx, base)
	if err != nil {
		return err
	}
	fmt.Printf("owner=%s epoch=%d changed=%v\n", state.Owner, state.Epoch, changed)
	return nil
}

func connectCmd(ctx context.Context) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	id := fs.String("id", defaultID, "claimed identity")
	serverFlag := fs.String("server", "", "server address HOST:PORT")
	usb := fs.String("usb", "046d:c548", "USB VID:PID filter for the primary detector")
	settle := fs.Duration("settle", 2*time.Second, "attach must persist this long before claiming")
	confirm := fs.Duration("confirm", 4*time.Second, "how long check-cmd may keep succeeding before the switch counts as failed")
	switchRetries := fs.Int("switch-retries", 3, "re-runs of the switch command before giving up")
	checkTimeout := fs.Duration("check-timeout", 10*time.Second, "bound on one check-cmd run")
	checkCmd := fs.String("check-cmd", defaultCheckCmd, "veto before the switch, receipt after it")
	notifyCmd := fs.String("notify-cmd", defaultNotifyCmd, "command run when the switch cannot be confirmed")

	var btMac string
	if btFallbackOK {
		fs.StringVar(&btMac, "bt-mac", "", "Bluetooth fallback detector MAC (Linux only)")
	}

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
	token, err := requireToken()
	if err != nil {
		return err
	}

	detector, err := pickDetector(btMac, *usb)
	if err != nil {
		return err
	}

	// The Linux desktop has no guards: newGuard there ignores displayMatch and
	// is always ok (SPEC §6.1-6.2).
	guard := newGuard(displayMatch)
	if noGuards {
		guard = alwaysOK{reason: "guards disabled"}
	}

	// The trailing SWITCH-CMD is an argv slice, never a shell string (SPEC §9).
	switchArgv := fs.Args()
	if len(switchArgv) == 0 {
		switchArgv = defaultSwitchCmd
	}

	machineCfg := MachineConfig{
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

	cfg := agentConfig{
		id:             *id,
		explicitServer: *serverFlag,
		detector:       detector,
		guard:          guard,
		client:         NewClient(token),
		runner:         run,
		machine:        &machineCfg,
		agentStatePath: filepath.Join(stateDir(), "agent.json"),
		switchArgv:     switchArgv,
		checkArgv:      shellArgv(*checkCmd),
		notifyArgv:     shellArgv(*notifyCmd),
		checkTimeout:   *checkTimeout,
	}

	ag := &agent{cfg: cfg}
	return ag.run(ctx)
}
