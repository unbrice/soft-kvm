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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/unbrice/soft-kvm/agent"
	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/detect"
	"github.com/unbrice/soft-kvm/discover"
	"github.com/unbrice/soft-kvm/hidpp"
	"github.com/unbrice/soft-kvm/identity"
	"github.com/unbrice/soft-kvm/model"
	"github.com/unbrice/soft-kvm/platform"
	"github.com/unbrice/soft-kvm/screen"
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
		dash := "--"
		if len(f.Name) == 1 {
			dash = "-"
		}
		decl := "  " + dash + f.Name
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
  connect --trigger NAME[,...] [--declare-screen DECL]... [--declare-hid DECL]...
          [-A COMMAND]... [-D COMMAND]... [flags]
  detect
  service <install|print|uninstall> [serve|connect] [args]

Flags precede positional arguments. SOFTKVM_TOKEN (env) is required for
serve, activate and connect; detect needs no token. SOFTKVM_SERVER overrides
server discovery (SPEC §5); SOFTKVM_LOG=debug turns on debug logs. connect's
-A (--on-activation) and -D (--on-deactivation) commands run when this host
becomes, or stops being, the owner; each is one quoted string, split into an
argv without a shell (SPEC §5.4, §9). "take-screen" and "take-hid" are
commands of the -A list, run at their place in the order; connect runs them
itself instead of exec'ing them. They post this host's --declare-screen and
--declare-hid declarations so the other hosts move the monitor and the
peripherals here (SPEC §5.5, §5.6). -D refuses them: only the owner takes.
detect prints the connect line that fits this host.`)
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
		slog.Warn("SOFTKVM_TOKEN is short; prefer a generated token of 32+ chars (openssl rand -hex 32)", "length", len(token))
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
		ch, err := c.Claim(cctx, candidate, id, *force, true)
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
	fmt.Printf("owner=%s epoch=%d activation=%d changed=%v\n", st.Owner, st.Epoch, st.Activation, changed)
	return nil
}

// connectFlags holds the values bound by newConnectFlagSet.
type connectFlags struct {
	id             *string
	serverFlag     *string
	trigger        *string
	settle         *time.Duration
	switchRetries  *int
	switchTimeout  *time.Duration
	notifyCmd      *string
	declareScreen  *commandList
	declareHID     *commandList
	onActivation   *commandList
	onDeactivation *commandList
	noGuards       bool
	displayMatch   string
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
		trigger:       fs.String("trigger", "", "required: comma-separated `NAME`s of --declare-hid devices to watch — run soft-kvm detect to list them"),
		settle:        fs.Duration("settle", 2*time.Second, "attach must persist this long before claiming"),
		switchRetries: fs.Int("switch-retries", 3, "re-runs of the commands or takes that failed before giving up"),
		switchTimeout: fs.Duration("switch-timeout", agent.DefaultSwitchTimeout, "bound on one --on-activation or --on-deactivation command, and on one write of a take, so a hung I²C write cannot freeze the agent"),
		notifyCmd:     fs.String("notify-cmd", platform.DefaultNotifyCmd, "command run by the previous holder when a take still fails after the retries"),

		declareScreen:  &commandList{},
		declareHID:     &commandList{},
		onActivation:   &commandList{},
		onDeactivation: &commandList{},
	}
	fs.Var(v.declareScreen, "declare-screen", "this host shows the monitor `'NAME INPUT [TABLE]'` on INPUT; repeat per monitor; required by take-screen")
	fs.Var(v.declareHID, "declare-hid", "the device `'NAME VID:PID[:SLOT] [host=N]'` reaches this host, on Easy-Switch channel N; repeat per device; take-hid takes those with a host=N")
	fs.Var(v.onActivation, "on-activation", "run `COMMAND` when this host becomes the owner; repeat for several, they run in order; take-screen and take-hid are commands of this list only")
	fs.Var(v.onActivation, "A", "short for --on-activation `COMMAND`")
	fs.Var(v.onDeactivation, "on-deactivation", "run `COMMAND` when this host stops being the owner; repeat for several, they run in order")
	fs.Var(v.onDeactivation, "D", "short for --on-deactivation `COMMAND`")
	if runtime.GOOS == "darwin" {
		fs.BoolVar(&v.noGuards, "no-guards", false, "disable macOS AC-power and display guards")
		fs.StringVar(&v.displayMatch, "display-match", "", "display name substring for the macOS guard (default: any external)")
	}
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: soft-kvm connect --trigger NAME[,...] [--declare-screen DECL]... [--declare-hid DECL]... [-A COMMAND]... [-D COMMAND]... [flags]")
		printFlags(fs, fs.Output())
	}
	return fs, v
}

func connectCmd(ctx context.Context) error {
	fs, flags := newConnectFlagSet()
	if err := parseArgs(fs, os.Args[2:]); err != nil {
		return err
	}
	// The commands are argv slices, never shell strings (SPEC §9).
	wiring, err := validateConnect(fs, flags)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return errUsage
	}
	token, err := requireToken()
	if err != nil {
		return err
	}

	// The Linux desktop has no guards: NewGuard there ignores its arguments
	// and is always ok (SPEC §6.1-6.2). On macOS it lists the monitors
	// through screen, which platform cannot import.
	guard := platform.NewGuard(flags.displayMatch, flags.noGuards, screen.DisplayNames)

	if !wiring.takes {
		slog.Warn("no take-screen or take-hid in -A: this host gives its screens and devices away and never gets them back")
	}

	c, err := client.NewClient(token)
	if err != nil {
		return err
	}

	machineCfg := model.MachineConfig{
		ID:             *flags.id,
		Settle:         *flags.settle,
		SwitchRetries:  *flags.switchRetries,
		RetrySpacing:   1 * time.Second,
		ArrivalWindow:  10 * time.Second,
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
		ID:             *flags.id,
		ExplicitServer: *flags.serverFlag,
		KeyFP:          identity.KeyFingerprint(token),
		Detector:       wiring.detector,
		Guard:          guard,
		Client:         c,
		Runner:         platform.Run,
		Machine:        &machineCfg,
		AgentStatePath: filepath.Join(stateDir, "agent.json"),
		OnActivation:   flags.onActivation.argv,
		OnDeactivation: flags.onDeactivation.argv,
		Screens:        wiring.screens,
		HIDs:           wiring.hids,
		WriteScreen: func(ctx context.Context, w screen.Write, name string) error {
			return w.Do(ctx, platform.Output, name)
		},
		SendHID: func(ctx context.Context, t hidpp.Target, channel uint8) (bool, error) {
			return t.Send(ctx, channel)
		},
		HIDPresent: func(ctx context.Context, t hidpp.Target) (bool, error) {
			return t.Present(ctx)
		},
		NotifyArgv:    platform.ShellArgv(*flags.notifyCmd),
		SwitchTimeout: *flags.switchTimeout,
		Resolver:      resolver,
	}

	return agent.Run(ctx, cfg)
}

// wiring is what connect's flags declare about this host once they parse.
type wiring struct {
	screens []screen.Declaration
	hids    []hidpp.Declaration
	// detector watches --trigger, whose entries may name a declared device.
	detector *detect.Detector
	// takes reports whether -A holds a take-screen or take-hid.
	takes bool
}

// validateConnect is what connect and the service-install validator both
// apply once the flags are parsed (SPEC §5.4, §5.7): no stray positional
// arguments — the old trailing `-- CMD` form gets a pointer to -D —, a
// --trigger list of declared names, declarations that parse
// with names unique within their kind, and the
// built-in take commands used consistently: only in -A, with no argument, and
// each with the declarations it posts. `--declare-hid` without take-hid is
// fine: this host still gives the device away.
func validateConnect(fs *flag.FlagSet, v *connectFlags) (wiring, error) {
	var w wiring
	if args := fs.Args(); len(args) > 0 {
		return w, fmt.Errorf("connect: unexpected argument %q: commands are given with -A/--on-activation and -D/--on-deactivation, one quoted string each (e.g. -A take-screen)", args[0])
	}
	if *v.trigger == "" {
		return w, errors.New("connect: --trigger is required (run `soft-kvm detect` to list candidates)")
	}

	seen := map[string]bool{}
	for _, argv := range v.declareScreen.argv {
		d, err := screen.ParseDeclaration(argv)
		if err != nil {
			return w, fmt.Errorf("connect: --declare-screen: %w", err)
		}
		key := strings.ToLower(d.Name)
		if seen[key] {
			return w, fmt.Errorf("connect: --declare-screen: the monitor %q is declared twice", d.Name)
		}
		seen[key] = true
		w.screens = append(w.screens, d)
	}
	seen = map[string]bool{}
	for _, argv := range v.declareHID.argv {
		d, err := hidpp.ParseDeclaration(argv)
		if err != nil {
			return w, fmt.Errorf("connect: --declare-hid: %w", err)
		}
		if seen[d.Name] {
			return w, fmt.Errorf("connect: --declare-hid: the device %q is declared twice", d.Name)
		}
		seen[d.Name] = true
		w.hids = append(w.hids, d)
	}
	named := make(map[string]hidpp.Target, len(w.hids))
	for _, d := range w.hids {
		named[d.Name] = d.Target
	}
	var err error
	if w.detector, err = detect.NewDetector(*v.trigger, named); err != nil {
		return w, fmt.Errorf("connect: %w", err)
	}

	for _, argv := range v.onDeactivation.argv {
		if isTake(argv[0]) {
			return w, fmt.Errorf("connect: %s belongs in -A: the server accepts a take from the owner only", argv[0])
		}
	}
	var takeScreen, takeHID bool
	for _, argv := range v.onActivation.argv {
		if !isTake(argv[0]) {
			continue
		}
		if len(argv) > 1 {
			return w, fmt.Errorf("connect: %s takes no argument, got %q", argv[0], argv[1:])
		}
		takeScreen = takeScreen || argv[0] == agent.TakeScreen
		takeHID = takeHID || argv[0] == agent.TakeHID
	}
	switch {
	case takeScreen && len(w.screens) == 0:
		return w, errors.New("connect: take-screen needs at least one --declare-screen (run `soft-kvm detect` to print one)")
	case !takeScreen && len(w.screens) > 0:
		return w, errors.New("connect: --declare-screen does nothing without -A take-screen")
	case takeHID && !slices.ContainsFunc(w.hids, func(d hidpp.Declaration) bool { return d.Channel != 0 }):
		return w, errors.New("connect: take-hid needs at least one --declare-hid with its host=N (run `soft-kvm detect` to print one)")
	}
	w.takes = takeScreen || takeHID
	return w, nil
}

// isTake reports whether name is one of the built-in take commands.
func isTake(name string) bool { return name == agent.TakeScreen || name == agent.TakeHID }

// commandList is a repeatable flag: each use is one string — a command or a
// declaration — split into an argv by splitCommand.
type commandList struct{ argv [][]string }

func (c *commandList) String() string {
	if c == nil {
		return ""
	}
	parts := make([]string, len(c.argv))
	for i, argv := range c.argv {
		parts[i] = strings.Join(argv, " ")
	}
	return strings.Join(parts, "; ")
}

func (c *commandList) Set(v string) error {
	argv, err := splitCommand(v)
	if err != nil {
		return err
	}
	c.argv = append(c.argv, argv)
	return nil
}

// splitCommand splits one command string into its argv: whitespace separates
// words; single quotes keep everything literal, double quotes keep it too bar
// \" and \\, and a backslash outside quotes escapes the next character. There
// is no expansion of any kind — no variables, globs, substitution or
// operators — so the result is exactly what is exec'd (SPEC §9).
func splitCommand(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inWord := false
	var quote rune // 0, '\'' or '"'
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\'):
				i++
				cur.WriteRune(rs[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '\\':
			if i+1 == len(rs) {
				return nil, errors.New("trailing backslash in command")
			}
			i++
			cur.WriteRune(rs[i])
			inWord = true
		case unicode.IsSpace(r):
			if inWord {
				argv = append(argv, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in command", quote)
	}
	if inWord {
		argv = append(argv, cur.String())
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("empty command")
	}
	return argv, nil
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
