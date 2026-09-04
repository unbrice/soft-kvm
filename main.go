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
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/unbrice/soft-kvm/agent"
	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/detect"
	"github.com/unbrice/soft-kvm/discover"
	"github.com/unbrice/soft-kvm/hidpp"
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

// errHelp marks -h/--help. The FlagSet has printed the usage, and asking
// for help is not a failure: main exits 0.
var errHelp = errors.New("help requested")

// parseArgs parses with fs, separating a help request from a real usage
// error — flag.Parse reports both as an error, and only one of them is one.
func parseArgs(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, flag.ErrHelp):
		return errHelp
	default:
		return errUsage
	}
}

// newLogHandler picks the log format for w: the console format when a
// person is watching, logfmt otherwise, so a pipe and journald keep the
// machine-readable form (SPEC §11).
func newLogHandler(w *os.File, level slog.Leveler) slog.Handler {
	if platform.IsTerminal(w) {
		return newConsoleHandler(w, level, platform.WantsColor(w))
	}
	return slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
}

// printFlags renders a FlagSet's flags with the double-dash spelling every
// example uses; flag.PrintDefaults prints a single dash and disagrees with
// the documentation.
func printFlags(fs *flag.FlagSet, w io.Writer) {
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		decl := "  --" + f.Name
		if name != "" {
			decl += " " + name
		}
		_, _ = fmt.Fprintln(w, decl)
		if f.DefValue != "" && f.DefValue != "false" {
			usage += fmt.Sprintf(" (default %s)", f.DefValue)
		}
		_, _ = fmt.Fprintln(w, "        "+usage)
	})
}

func main() {
	var level slog.LevelVar // Info by default
	if s := os.Getenv("SOFTKVM_LOG"); s != "" {
		if err := level.UnmarshalText([]byte(s)); err != nil {
			fmt.Fprintf(os.Stderr, "soft-kvm: unknown SOFTKVM_LOG level %q (debug, info, warn, error)\n", s)
		}
	}
	slog.SetDefault(slog.New(newLogHandler(os.Stderr, &level)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "help", "--help", "-h":
		usageTo(os.Stdout)
	case "serve":
		err = serveCmd(ctx)
	case "activate":
		err = activateCmd(ctx)
	case "connect":
		err = connectCmd(ctx)
	case "detect":
		err = detectCmd(ctx)
	case "hid-switch":
		err = hidSwitchCmd(ctx)
	case "service":
		err = serviceCmd(ctx)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		if errors.Is(err, errHelp) {
			return
		}
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

func usage() { usageTo(os.Stderr) }

func usageTo(w io.Writer) {
	_, _ = fmt.Fprintln(w, `usage: soft-kvm <command> [args]

  serve [IP:]PORT [--state PATH] [--instance NAME] [--no-advertise]
  activate ID [--server HOST:PORT] [--force]
  connect [flags] [-- CMD ARGS... [-- CMD ARGS...]]
  detect
  hid-switch VID:PID [DEVICE_INDEX|keyboard|mouse] HOST_INDEX
  service <install|print|uninstall> [serve|connect] [args]

Flags precede positional arguments. SOFTKVM_TOKEN (env) is required for
serve, activate and connect; detect and hid-switch need no token.
SOFTKVM_SERVER overrides server discovery (SPEC §5); SOFTKVM_LOG=debug turns
on debug logs. A connect switch command named "hid-switch" is built in, not
exec'd (SPEC §5.5).`)
}

// checkToken applies the shared-secret length rules. The TLS identity and
// the mDNS kh= fingerprint are both deterministic, salt-free functions of
// the token, so a weak token is one offline dictionary pass away from
// compromise (SPEC §9): refuse clearly weak ones, warn on short ones.
// Kept pure — no environment reads — so the service-install validators and
// their tests can call it directly.
const (
	tokenMinLen  = 16
	tokenGoodLen = 32
)

func checkToken(token string) (string, error) {
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

// requireToken reads the shared secret from the environment — never a flag
// (SPEC §5).
func requireToken() (string, error) {
	return checkToken(os.Getenv("SOFTKVM_TOKEN"))
}

// rejectExtraArgs fails a subcommand that expects at most want positional
// arguments when Parse left more in args. Stdlib flag stops parsing at the
// first positional, so a flag placed after one would be silently ignored;
// a flag-looking extra gets a hint (SPEC §11).
func rejectExtraArgs(name string, args []string, want int) error {
	if len(args) <= want {
		return nil
	}
	extra := args[want]
	if strings.HasPrefix(extra, "-") {
		return fmt.Errorf("%s: %q follows a positional argument and would be ignored; flags must come first", name, extra)
	}
	return fmt.Errorf("%s: unexpected extra argument %q", name, extra)
}

// serveFlags holds the values bound by newServeFlagSet.
type serveFlags struct {
	statePath   *string
	instance    *string
	noAdvertise *bool
}

// newServeFlagSet builds serve's FlagSet and its bound values. serveCmd and
// the service-install validator share it, so install never accepts a syntax
// the runtime would reject.
func newServeFlagSet() (*flag.FlagSet, *serveFlags, error) {
	defaultStatePath, err := platform.DefaultServeStatePath()
	if err != nil {
		return nil, nil, err
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	v := &serveFlags{
		statePath:   fs.String("state", defaultStatePath, "persisted owner/epoch/since path"),
		instance:    fs.String("instance", "", "mDNS instance name (default: hostname)"),
		noAdvertise: fs.Bool("no-advertise", false, "skip mDNS advertisement"),
	}
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: soft-kvm serve [IP:]PORT [--state PATH] [--instance NAME] [--no-advertise]")
		printFlags(fs, fs.Output())
	}
	return fs, v, nil
}

// serveListenAddr normalises serve's positional address: empty means the
// default port, a bare port or an empty host binds all interfaces. serveCmd
// and the service-install validator share it.
func serveListenAddr(arg string) (addr string, port int, err error) {
	addr = arg
	if addr == "" {
		addr = "8700"
	}
	if _, err := strconv.Atoi(addr); err == nil {
		addr = ":" + addr
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid address %q: %w", addr, err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	if host == "" {
		addr = ":" + portStr
	}
	return addr, port, nil
}

func serveCmd(ctx context.Context) error {
	fs, flags, err := newServeFlagSet()
	if err != nil {
		return err
	}
	if err := parseArgs(fs, os.Args[2:]); err != nil {
		return err
	}
	if err := rejectExtraArgs("serve", fs.Args(), 1); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return errUsage
	}
	token, err := requireToken()
	if err != nil {
		return err
	}

	addr, port, err := serveListenAddr(fs.Arg(0))
	if err != nil {
		return err
	}

	if *flags.instance == "" {
		hn, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("hostname: %w", err)
		}
		*flags.instance = hn
	}

	srv := server.NewServer(*flags.statePath, token)
	if !*flags.noAdvertise {
		stopAdv, err := discover.Advertise(*flags.instance, port, identity.KeyFingerprint(token))
		if err != nil {
			// e.g. Avahi owning UDP 5353 despite SO_REUSEPORT (SPEC §6.4).
			slog.Warn("mDNS advertisement failed, continuing without it", "error", err)
		} else {
			defer stopAdv()
		}
	}
	slog.Info("serving", "addr", addr, "instance", *flags.instance, "state", *flags.statePath)
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
	if err := parseArgs(fs, os.Args[2:]); err != nil {
		return err
	}
	if err := rejectExtraArgs("activate", fs.Args(), 1); err != nil {
		fmt.Fprintln(os.Stderr, err)
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

// connectFlags holds the values bound by newConnectFlagSet.
type connectFlags struct {
	id            *string
	serverFlag    *string
	trigger       *string
	settle        *time.Duration
	confirm       *time.Duration
	switchRetries *int
	checkTimeout  *time.Duration
	switchTimeout *time.Duration
	checkCmd      *string
	notifyCmd     *string
	noGuards      bool
	displayMatch  string
}

// newConnectFlagSet builds connect's FlagSet and its bound values. connectCmd
// and the service-install validator share it, so install never accepts a
// syntax the runtime would reject. The darwin-only guard flags are gated on
// runtime.GOOS, as before.
func newConnectFlagSet() (*flag.FlagSet, *connectFlags) {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	v := &connectFlags{
		id:            fs.String("id", platform.DefaultID, "claimed identity"),
		serverFlag:    fs.String("server", "", "server address HOST:PORT"),
		trigger:       fs.String("trigger", "", "required: comma-separated `VID:PID` filters — run soft-kvm detect to list them"),
		settle:        fs.Duration("settle", 2*time.Second, "attach must persist this long before claiming"),
		confirm:       fs.Duration("confirm", 4*time.Second, "how long check-cmd may keep succeeding before the switch has failed"),
		switchRetries: fs.Int("switch-retries", 3, "re-runs of the switch command before giving up"),
		checkTimeout:  fs.Duration("check-timeout", 10*time.Second, "bound on one check-cmd run"),
		switchTimeout: fs.Duration("switch-timeout", agent.DefaultSwitchTimeout, "bound on one SWITCH-CMD run, so a hung I²C write cannot freeze the agent"),
		checkCmd:      fs.String("check-cmd", platform.DefaultCheckCmd, "veto before the switch, receipt after it"),
		notifyCmd:     fs.String("notify-cmd", platform.DefaultNotifyCmd, "command run when the switch cannot be confirmed"),
	}
	if runtime.GOOS == "darwin" {
		fs.BoolVar(&v.noGuards, "no-guards", false, "disable macOS AC-power and display guards")
		fs.StringVar(&v.displayMatch, "display-match", "", "display name substring for the macOS guard (default: any external)")
	}
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: soft-kvm connect [flags] [-- CMD ARGS... [-- CMD ARGS...]]")
		printFlags(fs, fs.Output())
	}
	return fs, v
}

func connectCmd(ctx context.Context) error {
	fs, flags := newConnectFlagSet()
	if err := parseArgs(fs, os.Args[2:]); err != nil {
		return err
	}
	if *flags.trigger == "" {
		fmt.Fprintln(os.Stderr, "connect: --trigger is required (run `soft-kvm detect` to list VID:PIDs)")
		return errUsage
	}
	token, err := requireToken()
	if err != nil {
		return err
	}

	detector, err := detect.NewHIDDetector(*flags.trigger)
	if err != nil {
		return err
	}

	// The Linux desktop has no guards: NewGuard there ignores displayMatch and
	// is always ok (SPEC §6.1-6.2).
	guard := platform.NewGuard(flags.displayMatch, flags.noGuards)

	// The trailing commands are argv slices, never shell strings (SPEC §9).
	switchCommands, err := splitSwitchCommands(fs.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return errUsage
	}
	if len(switchCommands) == 0 {
		switchCommands = [][]string{platform.DefaultSwitchCmd}
	}

	c, err := client.NewClient(token)
	if err != nil {
		return err
	}

	machineCfg := model.MachineConfig{
		ID:             *flags.id,
		Settle:         *flags.settle,
		Confirm:        *flags.confirm,
		SwitchRetries:  *flags.switchRetries,
		RetrySpacing:   1 * time.Second,
		Cooldown:       5 * time.Second,
		BreakerWindow:  30 * time.Second,
		BreakerMax:     3,
		BreakerOpenFor: 60 * time.Second,
		NoCheck:        *flags.checkCmd == "",
	}

	stateDir, err := platform.StateDir()
	if err != nil {
		return err
	}
	resolver := discover.NewResolver(filepath.Join(stateDir, "server"))

	var checkArgv []string
	if *flags.checkCmd != "" {
		checkArgv = platform.ShellArgv(*flags.checkCmd)
	}

	cfg := agent.Config{
		ID:             *flags.id,
		ExplicitServer: *flags.serverFlag,
		KeyFP:          identity.KeyFingerprint(token),
		Detector:       detector,
		Guard:          guard,
		Client:         c,
		Runner:         platform.Run,
		Machine:        &machineCfg,
		AgentStatePath: filepath.Join(stateDir, "agent.json"),
		SwitchCommands: switchCommands,
		CheckArgv:      checkArgv,
		NotifyArgv:     platform.ShellArgv(*flags.notifyCmd),
		CheckTimeout:   *flags.checkTimeout,
		SwitchTimeout:  *flags.switchTimeout,
		Resolver:       resolver,
	}

	return agent.Run(ctx, cfg)
}

// splitSwitchCommands splits the trailing arguments on bare "--" tokens: each
// chunk is one switch command's argv (SPEC §5.4). flag.Parse has already
// consumed the first "--"; later ones arrive literally. An empty chunk is a
// user error.
func splitSwitchCommands(args []string) ([][]string, error) {
	var cmds [][]string
	var cur []string
	for _, arg := range args {
		if arg == "--" {
			if len(cur) == 0 {
				return nil, errors.New("connect: empty switch command (lone `--`)")
			}
			cmds = append(cmds, cur)
			cur = nil
			continue
		}
		cur = append(cur, arg)
	}
	if len(cur) > 0 {
		cmds = append(cmds, cur)
	} else if len(cmds) > 0 {
		return nil, errors.New("connect: empty switch command (trailing `--`)")
	}
	return cmds, nil
}

func detectCmd(ctx context.Context) error {
	fs := flag.NewFlagSet("detect", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: soft-kvm detect")
	}
	if err := parseArgs(fs, os.Args[2:]); err != nil {
		return err
	}
	if err := rejectExtraArgs("detect", fs.Args(), 0); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return errUsage
	}
	return detect.Run(ctx, os.Stdout)
}

func hidSwitchCmd(ctx context.Context) error {
	fs := flag.NewFlagSet("hid-switch", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: soft-kvm hid-switch VID:PID [DEVICE_INDEX|keyboard|mouse] HOST_INDEX")
		fs.PrintDefaults()
	}
	if err := parseArgs(fs, os.Args[2:]); err != nil {
		return err
	}
	sw, err := hidpp.Parse(fs.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return errUsage
	}
	return sw.Do(ctx)
}
