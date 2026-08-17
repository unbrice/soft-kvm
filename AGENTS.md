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
tree disagree on detail (§11 still folds the per-OS defaults into `run.go`), the
tree wins; fix the SPEC in the same change.

## How it works

One binary, `soft-kvm` (module `github.com/unbrice/soft-kvm`), same artifact on
every host, four subcommands:

- `serve [IP:]PORT` — the coordinator. Owns `owner/epoch`, serves `GET /state`,
  `POST /claim/{id}`, `GET /wait?epoch=N&id=ME` (long-poll) and an
  unauthenticated `GET /health`, over TLS whose identity is derived from the
  shared secret; every other route needs the `X-Display-Token` header. Port
  defaults to 8700, state to `--state /var/lib/soft-kvm/state.json`, and it
  advertises itself over mDNS unless `--no-advertise` (SPEC §6.4, §7).
- `connect [flags] [-- SWITCH-CMD ARGS...]` — the per-host agent. An HID attach
  detector feeds the pure state machine that decides when to claim ownership and
  run the switch command (`ddcutil` on Linux, `betterdisplaycli` on macOS).
  `--trigger VID:PID[,…]` is required; macOS also gets `--no-guards` and
  `--display-match` (SPEC §5.4, §6).
- `activate ID` — claims an identity by hand, for scripts and recovery. Fails
  with a pointer to `--force` when the target has no live agent.
- `detect` — prints attached HID devices and suggested `--trigger` values (SPEC
  §6.1).

`SOFTKVM_TOKEN` (env, never a flag) is required by `serve`, `activate` and
`connect`; `detect` needs no token. The TLS identity and the mDNS `kh=`
fingerprint are deterministic, salt-free functions of the token (SPEC §9).
`SOFTKVM_SERVER` and `--server` override discovery.

Per-host state lives under `stateDir()` — `$XDG_STATE_HOME/soft-kvm` on Linux,
`~/Library/Application Support/soft-kvm` on macOS: `agent.json` (last owner) and
`server` (last address that answered, the discovery cache).

## Code layout

One package `main`, one file per concern (SPEC §11). Per-OS splits use filename
build tags (`*_linux.go`, `*_darwin.go`), never `//go:build` lines.

| File                                      | Concern                                                                                               |
| ----------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `main.go`                                 | CLI dispatch, one `flag.FlagSet` per subcommand, token checks                                         |
| `machine.go`                              | the pure decision machine, `Step(Event) []Action`: no I/O, no goroutines, no clock                    |
| `agent.go`                                | dumb glue feeding the Machine; the `Detector` and `Guard` interfaces; no policy                       |
| `server.go`, `client.go`                  | the coordinator HTTP service and its client                                                           |
| `state.go`                                | the `/state` wire type and atomic JSON persistence                                                    |
| `discover.go`                             | mDNS advertise/browse, address resolution, address cache                                              |
| `tls.go`                                  | TLS identity and `kh=` fingerprint from the shared secret, via `crypto/hkdf`                          |
| `run.go`                                  | the `Runner` argv runner and the §11.1 child-process conventions                                      |
| `detect.go`, `detect_hid.go`              | HID enumeration for the subcommand; the attach detector both OSes share                               |
| `guard_darwin.go`                         | macOS AC-power and display guards                                                                     |
| `defaults_linux.go`, `defaults_darwin.go` | per-OS `defaultID`, `defaultSwitchCmd`, `defaultCheckCmd`, `defaultNotifyCmd`, `stateDir`, `newGuard` |

`newGuard` lives with the defaults on Linux, where it returns `alwaysOK` — the
always-on desktop has no guards.

## Stack

Go ≥ 1.25 (`go.mod` is the language floor), `CGO_ENABLED=0`, stdlib +
`golang.org/x/sync` (errgroup supervision, SPEC §11.1) +
`github.com/grandcat/zeroconf` (mDNS) + `github.com/telesma-app/hid` (purego
FFI: HID attach events on both OSes, SPEC §6.1-6.2). No CLI framework: stdlib
`flag`, one `FlagSet` per subcommand, because pflag's interspersed parsing eats
the trailing `-- SWITCH-CMD ARGS...` (SPEC §11). Logging is `log/slog` to
stderr.

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
- **`Machine.Step` table tests are the bulk of the suite** (`machine_test.go`) —
  every §8 edge case and every §4.3 gate combination, with no clock, no
  processes, no sockets.
- Server tests run the real handler under `httptest.NewServer` with the real
  client pointed at it: a mocked client would test the mock (SPEC §11.2).
- An interface needs two real implementations to exist. `Detector` and `Guard`
  have them; the display does not, so the seam is the `Runner` func type and the
  fake is a func (SPEC §11.2).
- Not unit-tested: the switch commands themselves and the detector's event path
  — both need the hardware and live in the §10 integration checklist.

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
- The switch command is an argv slice taken after `--`, never a shell string
  (SPEC §9). `--check-cmd` and `--notify-cmd` are the exception: their per-OS
  defaults carry shell quoting, so they run as `sh -c <string>`.
- `cmd.Cancel` sends SIGTERM (SIGKILL only after `WaitDelay`) so cancellation
  cannot cut `ddcutil` mid-I2C transaction (SPEC §11.1).
- Pure Go dependencies only, `CGO_ENABLED=0`.
