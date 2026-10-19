<!--
SPDX-FileCopyrightText: 2026 Brice Arnould

SPDX-License-Identifier: MIT OR Apache-2.0
-->

# soft-kvm

Display-Follows-Keyboard coordination daemon and client in Go: the shared
monitor's video input follows the keyboard between a NixOS desktop and a macOS
laptop.

`SPEC.md` is the design; this file is how to work in the tree (`CLAUDE.md` and
`GEMINI.md` are symlinks to it). Read SPEC §11 *Implementation notes* before
writing code — it fixes the package layout, the cancellation rules and the state
machine — and §4.3, which fixes what may authorise a switch. Where SPEC and the
tree disagree on detail, the tree wins; fix the SPEC in the same change.

## How it works

One binary, `soft-kvm` (module `github.com/unbrice/soft-kvm`), same artifact on
every host, five subcommands:

- `serve [IP:]PORT` — the coordinator. Owns `owner/epoch` and one **assignment**
  per taken screen or device, serves `GET /state`, `POST /claim/{id}`,
  `POST /take/{id}`, `POST /arrived/{id}` and `GET /wait?epoch=N&id=ME`
  (long-poll) over mutual TLS: the server identity and the required client
  certificate are both derived from the shared secret. Port defaults to 8700;
  `--state` defaults to `$STATE_DIRECTORY/state.json` under systemd, else
  `StateDir/state.json`. It advertises itself over mDNS unless `--no-advertise`
  (SPEC §6.4, §7).
- `connect --trigger LIST [--declare-screen DECL]... [--declare-hid DECL]... [-A COMMAND]... [-D COMMAND]... [flags]`
  — the per-host agent. An HID attach detector feeds the pure state machine that
  decides when to claim ownership. `--declare-screen 'NAME INPUT [TABLE]'` says
  which input of the monitor named `NAME` (its EDID name) this host is on;
  `--declare-hid 'NAME VID:PID[:SLOT] [host=N]'` says how the device every host
  calls `NAME` reaches this host, and on which Easy-Switch channel; without
  `host=N` the host watches the device and gives it away, and never takes it.
  `-A/--on-activation` runs when the host becomes the owner, in order; two
  command names are not exec'd but run in-process: `take-screen` and `take-hid`
  post the declarations to the server as **takes**, which it records as
  **assignments** (SPEC §5.5, §5.6). Every other host then gives away what each
  assignment names: it writes the `VCP:SRC=VAL` the wire carries to the monitor,
  or sends its end of the device to the assignee's channel with HID++
  `changeHost`. A screen write has no receipt; a device's receipt is its arrival
  on the assignee, and an assignment that still fails to be given away after the
  retries is notified by the previous assignee (SPEC §4.3).
  `-D/--on-deactivation` runs when the host stops being the owner. Each `DECL`
  and `COMMAND` is one quoted string that `connect` splits into words itself.
  `--trigger NAME[,…]` is required and takes only names `--declare-hid` declares
  — a device declared by a bare `VID:PID` is watched as a HID node that comes
  and goes, one declared by `VID:PID:SLOT` as one pairing slot of a receiver
  that stays plugged in; macOS also gets `--no-guards` and `--display-match`
  (SPEC §5.4, §6).
- `activate ID` — makes an identity the owner by hand and has it take, for
  scripts and recovery: against the current owner it re-posts the takes, and
  every other host gives the assignments away again. Fails with a pointer to
  `--force` when the target has no live agent (SPEC §5.3).
- `detect` — prints attached HID devices, suggested `--trigger` names (keyboards
  and HID++ mice), the `--declare-hid` flags for this host's peripherals and the
  `--declare-screen` flag for its monitor: it probes the backend, reports why it
  fails when it does, and names the mapping table when the monitor's brand needs
  one (SPEC §5.5, §5.6, §6.1).
- `service <install|print|uninstall> [serve|connect] [args]` — Linux only:
  validates with the `serve`/`connect` flag sets and installs, prints or removes
  the canonical systemd units from `units/`, baking the raw argv into
  `ExecStart=`; the coordinator step copies the binary to
  `/usr/local/bin/soft-kvm` and writes `/etc/soft-kvm/env`, the agent step
  writes `~/.config/soft-kvm/env` (both `0600`). On macOS it points at launchd
  (SPEC §5.7, §6.2).

`SOFTKVM_TOKEN` (env, never a flag) is required by `serve`, `activate` and
`connect`; `detect` needs no token. The TLS identity and the mDNS `kh=`
fingerprint are deterministic, salt-free functions of the token (SPEC §9).
`SOFTKVM_SERVER` and `--server` override discovery.

Per-host state lives under `platform.StateDir()` — `$XDG_STATE_HOME/soft-kvm` on
Linux, `~/Library/Application Support/soft-kvm` on macOS: `agent.json` (what it
handled: the owner and activation whose commands last ran, the seq of each
assignment) and `server` (last address that answered, the discovery cache).

## Code layout

One module, one binary, eleven packages under the root `main` (SPEC §11). Per-OS
splits use filename build tags (`*_linux.go`, `*_darwin.go`), never `//go:build`
lines. Layering is a DAG: the leaves (`state`, `identity`, `discover`,
`platform`, `hidpp`, `screen`) import nothing internal; `detect` imports
`hidpp`, `platform` and `screen`; `model` imports `state`; `client` imports
`state` and `identity`; `server` those two and `hidpp` (the channel bound it
validates on a take); `agent` imports `model`, `state`, `client`, `discover`,
`hidpp` and `screen`.

| Package    | Files                                               | Concern                                                                                                                                                                                                                                              |
| ---------- | --------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| (root)     | `main.go`, `log.go`, `service_*.go`                 | CLI dispatch, one `flag.FlagSet` per subcommand, token checks, wiring; the terminal log format; the systemd service installer (Linux)                                                                                                                |
| `state`    | `state.go`                                          | the `/state` wire types — assignments and takes among them — and atomic JSON persistence                                                                                                                                                             |
| `model`    | `decider.go`                                        | the pure decision state machine, `Decide(now, Observation) ([]Decision, wake)`: no I/O, no goroutines, no clock                                                                                                                                      |
| `identity` | `tls.go`                                            | TLS server identity, derived client certificate and `kh=` fingerprint from the shared secret, via `crypto/hkdf`                                                                                                                                      |
| `discover` | `discover.go`                                       | mDNS advertise/browse; `Resolver` resolves the server address and caches it (cache path injected)                                                                                                                                                    |
| `platform` | `run.go`, `defaults*`, `guard_*`, `term.go`         | the argv runner and the §11.1 child-process conventions; flag defaults; the per-OS `Guard`; whether a writer is someone's terminal                                                                                                                   |
| `detect`   | `detect.go`, `detect_hid*.go`                       | HID enumeration for the subcommand and its shell-transcript output; the two attach detectors both OSes share                                                                                                                                         |
| `hidpp`    | `hidpp.go`                                          | Logitech HID++: device declarations, the `changeHost` send that gives a device away and the presence check `take-hid` and the arrival watcher run, the `detect` probe, and the receiver link notifications the slot detector reads (SPEC §5.5, §6.1) |
| `screen`   | `screen.go`, `screen_*.go`                          | screen declarations, the standard and vendor (`lg`) mapping tables, the `VCP:SRC=VAL` wire form and the per-OS write backends — `ddcutil` on Linux, `IOAVService` on macOS (SPEC §5.6)                                                               |
| `client`   | `client.go`                                         | the coordinator's HTTP client                                                                                                                                                                                                                        |
| `server`   | `server.go`                                         | the coordinator HTTP service                                                                                                                                                                                                                         |
| `agent`    | `agent.go`, `watcher.go`, `worker.go`, `arrival.go` | supervisor, generations, decision loop, claims, the worker that runs commands and gives assignments away, and the arrival watcher; the `Detector` and `Guard` interfaces and the func seams; no policy                                               |

The invariants header lives at the top of `agent/agent.go`.
`platform.NewGuard(match, disabled, displays)` returns a concrete per-OS
`Guard`: a no-op on Linux (the always-on desktop has no guards), AC-power and
display checks on macOS. `platform` imports nothing internal, so `main` injects
`screen.DisplayNames` as `displays`. The `Guard` satisfies `agent.Guard`
structurally.

## Stack

Go ≥ 1.25 (`go.mod` is the language floor), `CGO_ENABLED=0`, stdlib +
`golang.org/x/sync` (errgroup supervision, SPEC §11.1) +
`github.com/grandcat/zeroconf` (mDNS) + `github.com/telesma-app/hid` (purego
FFI: HID attach events on both OSes, SPEC §6.1-6.2) +
`github.com/ebitengine/purego` (macOS `IOAVService` DDC/CI, SPEC §5.6). No CLI
framework: stdlib `flag`, one `FlagSet` per subcommand, because pflag's
interspersed parsing would read a `-vcp=…` inside a command as a flag (SPEC
§11). Logging is `log/slog` to stderr; `SOFTKVM_LOG=debug` lowers the level. The
handler depends on who is reading: a clock, a level and dimmed attributes when
stderr is a terminal, the stdlib logfmt `TextHandler` everywhere else, so
journald and pipes keep the machine-readable form.

## Toolchain

Nothing is guaranteed on `PATH` outside the dev shell — not `go`, not `just`.
Enter it first:

```sh
direnv allow            # or: nix develop ./nix/dev
```

It carries go 1.26, gopls, golangci-lint, just, dprint, reuse, and — on Linux —
ddcutil. The `go 1.25` in `go.mod` is the language floor, not the toolchain.
Non-Nix systems need Go ≥ 1.25, just, dprint and reuse installed by hand (see
`CONTRIBUTING.md`).

## Commands

`just` lists the recipes. The gate is `just check` (`lint` + `fmt-check` +
`test-unit`); `just ship` runs it before pushing.

| Recipe                        | Runs                                            |
| ----------------------------- | ----------------------------------------------- |
| `just test-unit`              | `go test -v ./...`                              |
| `just lint`                   | `golangci-lint run ./...`, `reuse lint --lines` |
| `just fmt`                    | `go fmt ./...`, `dprint fmt`                    |
| `just fmt-check`              | `gofmt -l`, `dprint check`                      |
| `just build` / `just release` | `CGO_ENABLED=0 go build -o soft-kvm .`          |

Three of those surprise people:

- **`reuse lint` fails on any file with no SPDX header.** Every new file opens
  with the three-line header `main.go` carries. `REUSE.toml` blankets the tree,
  but the closest annotation wins, so a file's own header is what shows up.
- **`dprint fmt` reflows Markdown to 80 columns** (`textWrap: always`). Touch
  `SPEC.md` or this file, run only `go fmt`, and `just check` fails on
  formatting.
- **golangci-lint findings are advisory today** — the recipe ends it with
  `|| true`, so only `reuse lint` can fail `just lint`. Read the findings
  anyway; a change that adds any does not ship.

## Testing

- `just test-unit` (`go test -v ./...`); SPEC §11.4 also asks for
  `go test -race ./...` — the long-poll broadcast channel is the one place a
  race is plausible.
- **`Decider.Decide` table tests are the bulk of the suite**
  (`model/decider_test.go`) — every §8 edge case and every §4.3 gate
  combination, with no clock, no processes, no sockets.
- Server tests run the real handler under `httptest.NewServer` with the real
  client pointed at it: a mocked client would test the mock (SPEC §11.2).
- An interface needs two real implementations to exist. `Detector` and `Guard`
  have them; the commands, the monitor and the devices do not, so their seams
  are func types and the fakes are funcs (SPEC §11.2).
- Not unit-tested: the screen write, the device send and the detector's event
  path — both need the hardware and live in the §10 integration checklist.

## Version control

Colocated **jj (Jujutsu) + git** repo — `.jj/` is the source of truth. Never run
mutating git commands (`git commit/rebase/checkout/branch/reset/switch`); use
jj. Read-only git (`log`, `diff`, `status`, `show`) and `gh` are fine.

Workflow — one change = one PR:

- `jj new 'trunk()'`, hack, `jj describe -m "<type>: <summary>"`.
- `just ship [rev]` — runs `check`, then pushes the change (default `@-`) as
  bookmark `<hostname>/<changeid>`, opens a PR, enables rebase auto-merge.
- `just sync` — after merges: fetch + rebase; merged changes and their bookmarks
  evaporate.

jj sharp edges:

- A PR contains every commit in `trunk()..rev` — move unrelated work off the
  stack before shipping (`jj rebase -r <rev> -d 'trunk()'`).
- `just sync` only rebases the stack holding `@`; rebase other heads explicitly:
  `jj rebase -b <head> -d 'trunk()' --skip-emptied`.
- Any jj operation is undoable: `jj op log`, then `jj undo` or
  `jj op restore <id>`.
- The `.githooks/pre-commit` formatting check hangs off `git commit`, which this
  workflow never runs. `just check` is the only thing that actually guards a
  push.

## Permissions

**Allowed:** read files, `go test`, `just test-unit`, `just lint`, `just fmt`,
`just check`, `nix develop ./nix/dev`.

**Require approval:** anything that pushes or opens PRs (`jj git push`,
`just ship`, `git push`), adding or removing dependencies, `.github/` changes,
`flake.nix` and `flake.lock` changes, destructive ops.

## Conventions

- **Commits:** `<type>: <summary>` (imperative). Types:
  `feat fix refactor docs test chore style perf`. Body: explain *why*, in bullet
  points. Ends with the DCO sign-off
  `Signed-off-by: Brice Arnould <brice@vleu.net>` (see `CONTRIBUTING.md` —
  dual-licensed MIT OR Apache-2.0, DCO sign-off required, no GPG key needed).
- **AI attribution:** `AI_POLICY.md` binds whoever triggers the ship — read the
  full diff, be able to explain it, flag a substantially AI-generated change in
  the commit description. Naming the models is optional.
- **Code style:** idiomatic Go, zero CGO, `go fmt`. Terse doc comments. Each
  file opens with a one-line `// file.go: <concern> (SPEC §x)` comment.
- **Naming:** a name says what the thing is. Define it in one sentence, and grep
  the SPEC and the code for it and for its competitors — a check for clashes,
  not a source: much of the SPEC was AI-drafted too. Prefer the hardware's or
  the standard's own term (Easy-Switch channel, Input Source, `changeHost`),
  then the plain word a user would say. One word, one meaning across code, SPEC
  and logs: the owner *takes*, every other host *gives away*, the server keeps
  *assignments*. Read a name where it is used: `Decision` fields are
  imperatives, `Observation` fields past tense. Plain words over jargon whose
  everyday reading misleads (`pausedUntil`, not `openUntil`). Structure over
  flags: no mutually exclusive bools, no kind inferred by elimination. A
  function name covers everything it does. `now time.Time` is a first parameter,
  after `ctx`.
- **Layering:** state lives with what it describes, and callers ask it rather
  than reach into it — `m.job.busy()`, not `m.job[0]`; no field set as a side
  effect of another function.
- **Cancellation:** one context from `main` down; `context.Context` is a first
  parameter, never a struct field. The three blocking things that need special
  handling — HID watcher, child processes, in-flight long-polls — are fixed in
  SPEC §11.1.
- **Docs:** 80 columns, sentence case in headings, `dprint fmt` before
  committing. No sections that collect deferred or rejected content — "Open
  items", "Alternatives considered", "Ideas discarded". Each fact goes where it
  is read: a rejected mechanism into Non-goals or into the finding that kills
  it, a footgun beside the component it bites, a pending measurement into the
  §10 test checklist.

## Security

- The shared secret comes from the `SOFTKVM_TOKEN` environment variable — never
  a CLI flag, so it stays out of process lists (SPEC §5, §9).
- Tokens under 16 chars are rejected, under 32 warn: the TLS identity and the
  mDNS fingerprint are deterministic salt-free functions of the token, so a weak
  one is one offline dictionary pass from compromise (SPEC §9). Generate with
  `openssl rand -hex 32`.
- The token rests in two `0600` env files, one per role (SPEC §5.7, §6.4):
  `/etc/soft-kvm/env` (root-only, read by PID 1 for the coordinator unit) and
  `~/.config/soft-kvm/env` (per-user, read by the user manager for the agent
  unit). `service install` re-renders them from the embedded templates.
- The `-A`/`-D` commands are argv slices — `connect` splits each flag's string
  with no expansion — never shell strings (SPEC §9). `--notify-cmd` is the
  exception: its per-OS defaults carry shell quoting, so it runs as
  `sh -c <string>`.
- A take relays data, never a command: a host writes only the `VCP:SRC` pairs
  its built-in tables list, and sends only a peripheral it declared, to channel
  1-3 (SPEC §5.6, §9).
- `cmd.Cancel` sends SIGTERM (SIGKILL only after `WaitDelay`) so cancellation
  cannot cut `ddcutil` mid-I2C transaction (SPEC §11.1).
- Pure Go dependencies only, `CGO_ENABLED=0`.
