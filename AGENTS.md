# soft-kvm

Display-Follows-Keyboard coordination daemon and client in Go: the shared
monitor's video input follows the keyboard between the NixOS desktop and the
macOS laptop.

`SPEC.md` is the design. Read §11 *Implementation notes* before writing code —
it fixes the package layout, the cancellation rules and the state machine, and
§4.3 fixes what may authorise a switch.

- **Stack.** Go ≥ 1.24 (`crypto/hkdf` derives the TLS identity from
  `SOFTKVM_TOKEN`, SPEC §9), `CGO_ENABLED=0`, stdlib + `golang.org/x/sys/unix` +
  `golang.org/x/sync` (errgroup supervision, SPEC §11.1) +
  `github.com/grandcat/zeroconf`. No CLI framework: stdlib `flag`, one `FlagSet`
  per subcommand, because pflag's interspersed parsing eats the trailing
  `-- SWITCH-CMD ARGS...` (SPEC §11).
- **Deliverable.** One binary, `soft-kvm`, three subcommands: `serve`,
  `activate`, `connect`. Same artifact on every host.

## Toolchain

Nothing is on `PATH` outside the dev shell — not `go`, not `just`. Enter it
first:

```sh
direnv allow            # or: nix develop ./nix/dev
```

It carries go 1.26, gopls, golangci-lint, just, dprint, reuse, and — on Linux —
ddcutil. The `go 1.24` in `go.mod` is the language floor, not the toolchain.

## Commands

`just` lists the recipes. The gate is `just check` (`lint` + `fmt-check` +
`test-unit`); `just ship` runs it before pushing.

| Recipe                        | Runs                                    |
| ----------------------------- | --------------------------------------- |
| `just test-unit`              | `go test -v ./...`                      |
| `just lint`                   | `golangci-lint run ./...`, `reuse lint` |
| `just fmt`                    | `go fmt ./...`, `dprint fmt`            |
| `just fmt-check`              | `gofmt -l`, `dprint check`              |
| `just build` / `just release` | `CGO_ENABLED=0 go build`                |

Three of those surprise people:

- **`reuse lint` fails on any file with no SPDX header.** Every new file opens
  with the three-line header `main.go` carries. `REUSE.toml` blankets the tree,
  but the closest annotation wins, so a file's own header is what shows up.
- **`dprint fmt` reflows Markdown to 80 columns** (`textWrap: always`). Touch
  `SPEC.md`, run only `go fmt`, and `just check` fails on formatting.
- **golangci-lint findings are advisory today** — the recipe ends it with
  `|| true`, so only `reuse lint` can fail `just lint`. Read the findings
  anyway; a change that adds any does not ship.

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
  `Signed-off-by: Brice Arnould <brice@vleu.net>`.
- **AI attribution:** `AI_POLICY.md` binds whoever triggers the ship — read the
  full diff, be able to explain it, flag a substantially AI-generated change in
  the PR description. Naming the models in the message is optional.
- **Code style:** Idiomatic Go, zero CGO, `go fmt`. Terse doc comments. An
  interface needs two real implementations to exist; one implementation plus a
  test fake is a func type (SPEC §11.2).
- **Docs:** 80 columns, sentence case in headings, `dprint fmt` before
  committing. No sections that collect deferred or rejected content — "Open
  items", "Alternatives considered", "Ideas discarded". Each fact goes where it
  is read: a rejected mechanism into Non-goals or into the finding that kills
  it, a footgun beside the component it bites, a pending measurement into the
  §10 test checklist.
