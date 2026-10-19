# Display-Follows-Keyboard — System Spec

**Status:** design settled, ready to implement **Date:** 2026-10-11

Both directions are automatic. Switching the LG away has been done by hand from
Linux and from macOS, so the one mechanism the whole design rests on is known to
work; §10 is the integration pass, run at the end.

---

## 1. Context

| Item                  | Facts                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Hosts                 | Linux desktop `1b-nix0` (NixOS, Hyprland/Wayland, Intel Arc A770 on `xe`, monitor on `card0-DP-3`, always-on, home LAN); macOS **corp laptop** (locked down, different trust domain, frequently away)                                                                                                                                                                                                                                                                                                                                             |
| Display               | One 4K ultrawide LG, shared. Built-in KVM unusable (USB hub bound to a single upstream port)                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| DDC/CI constraint     | Monitor answers DDC **only on the currently active video input** (observed in practice)                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| Input-switch commands | Giving a screen away (§5.6), per OS. Linux: `ddcutil setvcp 0xF4 0xD0 --i2c-source-addr=0x50 --noverify --model NAME` for this LG · macOS: `IOAVServiceWriteI2C` over `CoreFoundation` and `IOKit` (`purego` FFI, no external helper). Nothing is baked in as a default: `detect` prints the declaration for this host                                                                                                                                                                                                                            |
| Peripherals           | Logitech multi-device keyboard + mouse on a **Bolt** receiver, `046d:c548`. A Unifying receiver `046d:c52b` is also plugged into the desktop — the detector filter must not match it. Keyboard and mouse stay on Bolt channel 1 permanently; the Easy-Switch keys retire                                                                                                                                                                                                                                                                          |
| Trigger hardware      | Cheap USB 3.0 sharing switch, UGREEN / ATEN class. Not bought                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| Coordination host     | Whichever host runs `soft-kvm serve`, found over mDNS — no host holds another's address (§5.1). The desktop by default; a Raspberry Pi on the LAN (the HA Pi; HA itself is **not** used) if a neutral one is wanted                                                                                                                                                                                                                                                                                                                               |
| Policy                | Corp device: outbound-only networking, minimal/no installs, must hold **no powerful credentials**                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| Implementation        | One Go binary, `soft-kvm`, five subcommands (`serve` / `activate` / `connect` / `detect` / `service` — `detect` enumerates HID devices and probes the monitor to print the `connect` line). Same artifact everywhere; the host carrying the bit runs `serve` alongside its `connect`. `CGO_ENABLED=0`, Go ≥ 1.25, stdlib + `golang.org/x/sync` (errgroup supervision, §11.1) + `grandcat/zeroconf` (mDNS discovery, §5.1) + `telesma-app/hid` (USB attach events on both OSes, §6.1-6.2) + `ebitengine/purego` (macOS `IOAVService` DDC/CI, §5.6) |

## 2. Goals / non-goals

**Goals**

- Display input follows the keyboard in both directions, one gesture, no human
  step.
- Nothing happens when the laptop is away, on battery, undocked, or off the home
  network.
- Manual monitor OSD switching always remains possible.
- Exactly one deliverable: a static Go binary per host (`linux/amd64`,
  `darwin/arm64`, plus `linux/arm64` if a Pi carries the bit). No scripts, no
  curl plumbing, no runtime deps on the corp laptop beyond stock tools.

**Non-goals**

- Hardware video switching (4K ultrawide exceeds affordable KVM video paths).
- Cross-host clipboard/files, WAN operation, enterprise-grade security (LAN
  threat model).
- Inbound listeners on the laptop. It is in a different trust domain, so there
  is no SSH and no host-to-host path — it only ever makes outbound connections.
- Surviving a wrong switch without touching the monitor. Once the display points
  at a host that cannot drive DDC, the OSD is the only way back (§8).

## 3. Key findings (context collected)

1. **The host being *left* must execute the display switch** — the gaining
   host's DDC is ignored (inactive input). This inverts the naive "run the
   command where the keyboard arrives" design.
2. **"Keyboard connected to me" is the only locally unambiguous "switched to me"
   signal.** Disconnect is ambiguous: switched, powered off, out of range, and
   BT hiccup look identical.
3. **No "connected but inactive" state exists over Bluetooth.** Paired device
   lists are binary connected/not-connected; no host can see *where* the
   keyboard is active.
4. **Keyboard channel selection is firmware-owned.** A host cannot pull the
   keyboard; only the Easy-Switch key (or software on the *current* host, Bolt +
   Solaar on Linux only) moves it.
5. **Solaar rules trigger only on HID++ notifications** (diverted
   keys/wheels/gestures); connect/disconnect events never reach the rule engine,
   and `Active` is a condition, not a trigger.[^1^]
6. **Cheap bus-powered USB switches are electrically invisible when switched
   away** — no "present but pointing elsewhere" witness state. A witness
   requires an emulating KVM (e.g., TESmart E23/P23 generation).[^2^]
7. **HA long-lived tokens are unscoped and full-permission**, identical across
   REST, SSE, and WebSocket; scoped tokens do not exist.[^3^]
8. **A LAN coordination host makes the witness unnecessary**: any clean local
   attach signal + a shared bit is a complete solution.
9. **Switching the LG away works from both hosts, verified by hand** — `ddcutil`
   from Linux, Apple's `IOAVService` (`IOAVServiceWriteI2C` /
   `IOAVServiceReadI2C`, the same private IOKit entry points `m1ddc`[^6^] and
   BetterDisplay[^4^][^5^] call) from macOS. Because `IOAVService` takes the raw
   7-bit I²C source address and VCP feature byte directly, the LG side-channel
   write (`0xF4` from source `0x50`) works on macOS too without an external
   helper, even on firmware that ignores standard `0x60` writes.[^4^]
10. **A DDC write that returns success may still not take effect.** The bus is
    unacknowledged in practice: the monitor can NAK, arrive busy, or be waking
    from standby. Nothing tells: the read-back from the host being left is not a
    usable receipt in practice, so a screen write has none, and one that does
    not land costs an OSD press (§4.3).
11. **Switching inputs drops the video link on the input being left**, unless
    the LG's *Deep Sleep* setting is off. A dropped link means the losing host
    sees a monitor hot-unplug: Hyprland collapses workspaces, macOS rearranges
    windows, and the macOS "LG present" guard goes false while the Mac is still
    docked. **DisplayPort/HDMI Deep Sleep must be set to Off in the LG OSD.**
12. **DDC on this desktop is not currently wired up.** No `/dev/i2c-*`, no
    `i2c_dev` module, no `i2c` group, no `ddcutil` installed. NixOS needs
    `hardware.i2c.enable = true` (loads `i2c-dev`, creates the `i2c` group,
    installs the udev rules) plus `ddcutil` in `systemPackages`, and the group
    membership only takes effect after re-login.

## 4. Final architecture

```
 keyboard+mouse ──Bolt── receiver ── [USB switch] ──► Linux box / Mac dock
                                           │
                            local attach/detach events (unambiguous)
                                           │
   ┌────────────────────────────┐          │          ┌────────────────┐
   │  LINUX HOST                │          │          │  macOS HOST    │
   │                            │          │          │                │
   │  soft-kvm connect          │          │          │ soft-kvm       │
   │    detector ─┐             │          │          │ connect        │
   │    switch  ◄─┤             │          │          │  detector ─┐   │
   │              │ loopback    │          │          │  switch  ◄─┤   │
   │  ┌───────────▼──────────┐  │          │          └──────┬─────┘   │
   │  │ soft-kvm serve :8700 │◄─┼──────────┴─────────────────┘         │
   │  │ owner bit + epoch    │  │   POST /claim/mac · GET /wait        │
   │  │ liveness, mDNS SRV   │  │   found by mDNS, outbound only       │
   │  └──────────────────────┘  │                                      │
   └────────────────────────────┘──────────────────────────────────────┘
              ▲
    soft-kvm activate ID     (manual override, any LAN host)

  `serve` is one unit, not one host: the desktop runs it next to its own
  `connect`, which reaches it over loopback like any other client. A Pi
  running `serve` alone works identically.
```

### 4.1 Data flow (switch Linux → Mac)

1. Receiver attach detected on the Mac; guards pass (AC power + dock).
2. Mac's `soft-kvm connect`: `POST /claim/mac` (idempotent).
3. Server: owner changed → `activation++`, `epoch++` → wake all `/wait`
   long-polls. Linux runs its `--on-deactivation` commands, if it has any.
4. The Mac's agent wakes and sees the ownership come to it
   (`handled_owner != mac`, `owner == mac`). It runs its `--on-activation`
   commands: `take-screen` posts the Mac's screen declaration, `take-hid` its
   device declarations (`POST /take/mac`).
5. Server: one **assignment** recorded per name → `epoch++` → wake.
6. Every other agent wakes and gives away what the assignments name, where it
   can. Linux finds the LG among its monitors and writes `F4:50=D1` to it — the
   monitor moves to the Mac's input — then sends the mouse it still holds to the
   Mac's channel.
7. The mouse links on the Mac, whose agent posts `/arrived`; Linux reads it on
   the next wake and stops.

The Mac writes nothing to the monitor — by design, it can't (finding 1).

### 4.2 Data flow (switch Mac → Linux)

Symmetric: the receiver attaches on Linux, Linux claims and takes, the Mac
writes `F4:50=D0` through `IOAVService`, the monitor moves to Linux's input.

Each host declares its own port and the table that reaches it (§5.6). The host
that writes reads the three bytes off the wire: it needs no table for another
host's port, so a third host joins without any existing host being reconfigured.

### 4.3 What authorises a switch

A **take** is the owner's request that one named screen or device come to it.
The server keeps the last take per name as an **assignment**: the **assignee**
that took it, the **previous assignee**, what to write, and — for a device —
whether it `arrived`. Every other host **gives away** what the assignment names:
it moves its screen or device to the assignee. Giving a screen away is a one-way
door: after the write, the host that wrote can no longer reach the monitor.
Three conditions gate an assignment, all required.

- **The taker is the owner.** The server accepts `POST /take/{id}` only from the
  current owner, and only under the `activation` its ownership dates from (§7).
  A host that lost the ownership while its `-A` commands ran gets a 409. Without
  this, a slow activation lands its take after the flip back and pulls the
  monitor away from the keyboard.
- **The assignment is new to this host.** The agent persists the `seq` of the
  last assignment it handled per name (`$XDG_STATE_HOME/soft-kvm/agent.json`,
  `~/Library/Application Support/soft-kvm/agent.json` on macOS) and gives an
  assignment away only when its `seq` differs. An agent that starts with no
  local record adopts every assignment as handled and **never switches on that
  first reconcile**: an assignment older than the agent says nothing about where
  the monitor is now. A name the agent meets after that first reconcile is new,
  and is given away.
- **Assignee liveness.** The server counts open `/wait` connections per host and
  reports them in `/state` as `live`. A host stays live for a grace period after
  its last `/wait` closes (`server.LiveGraceTime`): every wake and every 204
  closes the long-poll, and the agent re-opens it one `/state` round trip later,
  so without the grace a host reads `/state` inside the assignee's gap. No host
  gives away an assignment whose assignee has no live agent. It stays pending
  and is judged again at the next state change: liveness alone wakes nobody.

**Every host but the assignee gives an assignment away.** Only the host on the
monitor's active input can move it (finding 1), the server's record of which
host that is goes stale with one OSD press, and a device may be linked to any
host. So each host tries, and a host with nothing to move does nothing: no
monitor of that name attached, no declaration for that device, or the device not
linked here.

Nothing probes the monitor before the write. A read that says "my input is
already inactive" cannot be told from a read that failed because DDC is not
wired up, and treating the second as the first makes a broken setup look like a
finished handover. A write aimed at an input the monitor already shows costs a
write the monitor ignores.

**Rate limit, per name.** Two things hold an assignment back, never a command: a
5 s cooldown after an assignment of that name was given away, and a circuit
breaker — 3 give-aways of one name within 30 s stop every give-away for 60 s,
with a log line. A misbehaving detector costs one manual OSD press, not a loop.
The assignments of one handover carry different names, so the mouse does not
wait behind the screen, and an assignment with nothing to move here counts for
neither: the keyboard that carried the gesture is absent on every handover.

**Screens have no receipt.** The read-back from the host being left misreports
in practice, and on the display that just became active the read races the link
coming up. A screen assignment is written once; only a write that fails locally
— `ddcutil` missing, no `i2c-dev`, no permission, a hung I²C transaction — is
retried. A write the monitor ignores is silent and costs one press of the OSD
Input button (§2).

**A device's receipt is its arrival on the assignee.** The assignee polls each
device it took — every 1 s, for 60 s after the take — and posts `/arrived` once
it holds the link. A device already there when `take-hid` runs is posted as
arrived, and no host moves it: the peripheral that carried the gesture never
costs a write. A host that sent a device waits 10 s for `arrived`.

The decider tracks the work in flight as a **job**: **tasks** it **issues**
together — the `-A` or `-D` commands of an ownership change, or the assignments
a reconcile gives away — then, once every task is settled, success or not, a
**record** of what the job handled: the ownership the commands ran for, or the
seq of each assignment. A restart before the record runs the tasks again. Once
the record is saved, the job logs what failed and notifies what still failed
after the last retry. `--switch-timeout` bounds each write of an issue (default
30 s, SIGTERM then SIGKILL after `WaitDelay`), so every issue reports, and the
decider waits for that report with no deadline of its own: an issue is as long
as its writes added up, and a decider-side bound would retry while the worker is
still busy, then take the late result for the retry's. A result is accepted only
while its issue is outstanding — a late one is ignored and logged.

1. Give away every pending assignment, in the order `/state` lists them. One
   failing does not skip the rest: they target independent devices.
2. Screen written, device arrived, or nothing to move here ⇒ that assignment is
   done.
3. A write that failed, died or never reported, or a device that did not arrive
   within 10 s ⇒ retry **only those assignments**, up to `--switch-retries 3`,
   each at least 1 s after its last attempt. An assignment superseded by a newer
   take of the same name is dropped.
4. Still failing after the last retry ⇒ the previous assignee runs
   `--notify-cmd`; every other host logs and stops.

The previous assignee is the host the thing just left, which for a screen is the
one still showing — the only host the user can read at that moment. A third
host's failure is expected (its input is inactive) and its notification would
sit unread until the next switch. The record is wrong after an OSD press: the
host on screen then fails silently. The first assignment of a name has no
previous assignee and never notifies.

Nothing re-claims and nothing reverts: the shared bit is already right, the
keyboard is already where the user put it, and only the pixels or the mouse are
behind.

**Activation and deactivation.** The edge `handled_owner != me && owner == me`
runs the `--on-activation` commands; `handled_owner == me && owner != me` runs
the `--on-deactivation` commands. Neither is a door, so no liveness gate,
cooldown, breaker or notification applies. They share the `handled_owner` record
— an agent with no record adopts the owner and runs nothing — and the retry
path: commands run in order, each bounded by `--switch-timeout`, and only those
that failed re-run, up to `--switch-retries`, so a take the server already
accepted is not posted again. A failure after the last retry is logged.
`take-screen` and `take-hid` exit 0 when the server accepted the take; a 409 is
a failure like any other. Once the commands are over, success or not, the job
records the ownership they ran for — `handled_owner` and `handled_activation`,
from the `/state` that crossed the edge — so an agent restarted mid-run runs the
commands again and one that ran them does not repeat them for the same
transition.

**Re-activation.** `soft-kvm activate ID` against the current owner bumps
`activation` without changing the owner (§5.3). The owner's agent compares it
with the `handled_activation` it recorded and runs its `-A` commands again when
the two differ. Its takes carry the new activation, so the server records the
assignments anew and every other host gives them away again. This is the
recovery for a mouse stranded on the wrong host and for a monitor moved by OSD.
An attach-driven re-claim bumps nothing: a receiver flap or an agent restart
moves no screen.

One job is in flight at a time — an activation, a deactivation, or a batch of
assignments. Whenever none is, the decider reconciles the newest state it has
seen, ownership edge first: a state that arrives during a job is reconciled when
it ends — a flip away during an activation deactivates right after it — and an
assignment the rate limit deferred is given away once the limit lifts.
Reconciling a state again changes nothing once its job is over, since every job
records what it handled. Why an assignment is held back is logged once, and
again only when the reason changes. A claim waits out `--settle` from the last
attach, then for the job in flight to end.

## 5. CLI surface

Config is shared: `SOFTKVM_TOKEN` (shared secret), `SOFTKVM_SERVER`
(`HOST:PORT`, optional override). The token is never a flag.

### 5.1 Finding the server

The server advertises `_soft-kvm._tcp.local.` over mDNS
(`github.com/grandcat/zeroconf`); clients browse for it. No host in the system
holds another host's address, so the bit moves between hosts by moving one flag.

**Resolution order, every time a connection is needed:** `--server` →
`SOFTKVM_SERVER` → cached address from the last successful connection → mDNS
browse (3 s rounds, retried with backoff to 60 s). Resolution streams candidate
addresses rather than returning one: each source's candidates are tried in turn
with the real TLS-verified request until one connects. The cache
(`$XDG_STATE_HOME/soft-kvm/server`) is what makes a boot survive a flaky browse;
it is tried first because a working address beats a discovery round trip, and a
stale entry only costs one failed request — resolution falls through to mDNS
afterwards. A browse round yields every ranked address of every matching entry
as it arrives.

**The TXT record carries the instance id, the protocol version, and `kh=`, a
truncated, domain-separated fingerprint of the token (§9). Never the token
itself.** It is broadcast to every device on the LAN, guests included. A
matching fingerprint proves nothing — a rogue can copy a broadcast value, and
TLS still decides (§9). It exists so a client holding the wrong token can say so
and skip the server instead of failing the TLS handshake against it.

Sharp edges, in the order they will bite:

- **Multicast does not reliably cross WiFi.** Consumer APs drop or rate-limit
  multicast to save airtime, and client isolation kills it outright. The roaming
  laptop is both the host that needs discovery most and the one least likely to
  get it. `SOFTKVM_SERVER` on the Mac is the answer if the browse proves
  unreliable, which costs exactly the config this feature removes.
- **macOS 15 gates local network access per-binary.** Sequoia prompts on the
  first connection to a LAN address — multicast *and* unicast — and a denial is
  silent: connections simply never complete. A launchd agent may get no prompt
  at all, and MDM can deny it outright. This applies to the HTTPS long-poll too,
  not only to mDNS, so it must be cleared before anything else on the Mac (§10).
- **Any LAN host can advertise the same service** and point agents at an
  impostor — but the impostor cannot complete the TLS handshake, because the
  certificate is derived from the secret it does not hold (§9). The agent
  re-browses on the failure (§8) and never reveals the token.
- `grandcat/zeroconf` is unmaintained (pinned v1.0.0, 2021; upstream's last
  commit was 2023). The known bugs and the shapes they force:

  - Reusing a resolver or entries channel across `Browse` calls races into
    close-of-closed-channel panics (issues #118, #113). Every browse round
    builds both fresh, and the channel is buffered because the mainloop sends
    without selecting on ctx.
  - An entry whose first response carries no A/AAAA record is dropped (#124);
    the 3 s retry loop absorbs it.
  - A multi-homed server advertises every interface, junk included (#43; the
    fix, PR #125, was never merged). Ranking — routable over link-local — only
    orders the attempts; it cannot recognise an unroutable private address (a
    bridge IP looks like a LAN IP). The client tries each advertised address
    with the real TLS-verified request until one connects.

  `github.com/libp2p/zeroconf/v2` is the live fork — but barely (last release
  2022, kept alive for go-libp2p's own discovery), and not a drop-in: `Browse`
  is a blocking package function with no `Resolver` type, and `ServiceEntry.TTL`
  became `Expiry`. It fixes the channel-reuse panics this code already avoids,
  and leaves #124 and #43 unfixed — so switching is API churn, not reliability.
  Re-evaluate only if the browse loop misbehaves in the field.

### 5.2 `soft-kvm serve [IP:]PORT`

The bit, the assignments (§7), and nothing else. `IP` optional — omitted binds
all interfaces (`soft-kvm serve 8700` ≡ `:8700`).

| Flag / env        | Default   | Meaning                                                        |
| ----------------- | --------- | -------------------------------------------------------------- |
| `--state PATH`    | see below | Persisted owner, epoch, activation and assignments (§7)        |
| `--instance NAME` | hostname  | mDNS instance name under `_soft-kvm._tcp`                      |
| `--no-advertise`  | off       | Skip the mDNS registration; clients must be given an address   |
| `SOFTKVM_TOKEN`   | required  | Shared secret; derives the TLS identity and client certificate |

The `--state` default is `$STATE_DIRECTORY/state.json` when systemd hands the
unit a `StateDirectory=` (created with the right owner whatever `User=` the unit
runs as, root or dedicated), `StateDir/state.json` otherwise —
`$XDG_STATE_HOME/soft-kvm` on Linux, `~/Library/Application Support/soft-kvm` on
macOS, where launchd sets no equivalent (§6.4).

The advertised port is the listening port. Bind a fixed 8700 rather than port 0
— a changing port turns every stale cache entry into a failed connection.

The host carrying the bit runs this next to its own `connect`, as a second unit.
`connect` reaches it over loopback with no special case: it browses, or finds
`127.0.0.1:8700` in its cache, and long-polls `/wait` like any other client,
which is also how its liveness gets registered (§4.3).

### 5.3 `soft-kvm activate ID [--server HOST:PORT] [--force]`

Makes `ID` the owner and has it take — one-shot `POST /claim/ID?retake=true`,
exits non-zero on failure. Server resolution as §5.1, so it works from any LAN
host with only the token. When `ID` already owns, its agent runs its `-A`
commands again and every other host gives the assignments away again (§4.3).
`--force` is required when `ID` has no live agent: the bit flips and nothing
moves until that agent starts, sees the ownership come to it and takes. Use
cases: a mouse left on the wrong host, a monitor moved by OSD, testing, "just
switch now".

### 5.4 `soft-kvm connect --trigger LIST [--declare-screen DECL]... [--declare-hid DECL]... [-A COMMAND]... [-D COMMAND]... [flags]`

The host agent: detector, claimer, watcher, the declarations, and the activation
and deactivation commands, in one long-running process. `--trigger` and the
declarations have no default, because they are this host's wiring and nothing
else knows it: `soft-kvm detect` prints them.

| Flag / arg                        | Default (Linux)                                            | Default (macOS)                                                                               | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| --------------------------------- | ---------------------------------------------------------- | --------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--id ID`                         | `linux`                                                    | `mac`                                                                                         | Claimed identity (from `GOOS`; override for testing, and for a third host)                                                                                                                                                                                                                                                                                                                                                                          |
| `--trigger LIST`                  | required                                                   | required                                                                                      | Comma-separated `NAME`s for the trigger detector, each one a `--declare-hid` declares: an address is spelled once, in its declaration (§5.5). A device declared by `VID:PID` is watched as a HID node that genuinely comes and goes — a moved receiver, a Bluetooth keyboard (§6.3); one declared by `VID:PID:SLOT` as one pairing slot of a receiver that stays plugged in (§6.1). `soft-kvm detect` prints the candidates with their declarations |
| `--declare-screen DECL`           | none                                                       | none                                                                                          | `NAME INPUT [TABLE]`: the monitor named `NAME` shows this host on `INPUT` (§5.6). Repeat per monitor. Required by `take-screen`, and useless without it                                                                                                                                                                                                                                                                                             |
| `--declare-hid DECL`              | none                                                       | none                                                                                          | `NAME VID:PID[:SLOT] [host=N]`: the device every host calls `NAME` reaches this host at that address, on Easy-Switch channel `N` (§5.5). Repeat per device. `--trigger` names one; `take-hid` takes those with a `host=N`, and this host gives the others away without taking them                                                                                                                                                                  |
| `-A`, `--on-activation COMMAND`   | none                                                       | none                                                                                          | Run when this host becomes the owner (§4.3). Repeat for several; they run in order. `take-screen` and `take-hid` are commands of this list only                                                                                                                                                                                                                                                                                                     |
| `-D`, `--on-deactivation COMMAND` | none                                                       | none                                                                                          | Run when this host stops being the owner (§4.3). Repeat for several; they run in order                                                                                                                                                                                                                                                                                                                                                              |
| `--switch-timeout 30s`            | 30s                                                        | 30s                                                                                           | Bound on one `-A` or `-D` command, and on one write giving an assignment away — a hung I²C write must not freeze the agent (§4.3)                                                                                                                                                                                                                                                                                                                   |
| `--settle 2s`                     | 2s                                                         | 2s                                                                                            | Attach must persist this long before claiming                                                                                                                                                                                                                                                                                                                                                                                                       |
| `--switch-retries 3`              | 3                                                          | 3                                                                                             | Re-runs of the commands, and new attempts at giving assignments away, that failed, at least 1 s apart, before giving up                                                                                                                                                                                                                                                                                                                             |
| `--notify-cmd CMD`                | `notify-send 'soft-kvm' 'Handover failed: switch by hand'` | `osascript -e 'display notification "Handover failed: switch by hand" with title "soft-kvm"'` | Run by the previous assignee when giving an assignment away still fails after the last retry (§4.3)                                                                                                                                                                                                                                                                                                                                                 |
| `--no-guards`                     | implicit                                                   | —                                                                                             | macOS guards: AC power + dock present (§6.2)                                                                                                                                                                                                                                                                                                                                                                                                        |
| `SOFTKVM_TOKEN`                   | required                                                   | required                                                                                      | Shared secret                                                                                                                                                                                                                                                                                                                                                                                                                                       |

Each `DECL` and each `COMMAND` is one string that `connect` splits into words
itself, never a shell (§9): whitespace separates words, single or double quotes
keep one together, a backslash escapes the next character, and nothing expands —
`$HOME`, globs and `|` are ordinary characters.

`take-screen` and `take-hid` are commands of the `-A` list: each runs at its
place in the order, between the external commands around it. `connect` runs them
itself instead of exec'ing them, and they take no argument: each posts every
declaration of its kind. The server accepts a take from the owner only (§4.3),
so `connect` refuses to start on either in `-D`. It also refuses `take-screen`
without a `--declare-screen` or the reverse, `take-hid` without a
`--declare-hid` that carries its `host=N`, and a `--trigger` name no
`--declare-hid` declares. An agent whose `-A` takes nothing gives screens and
devices away and never gets them back; `connect` warns once at startup.

```
soft-kvm connect --trigger keyboard \
  --declare-screen '"LG HDR WQHD" dp1 lg' \
  --declare-hid 'keyboard 046d:c548:1 host=1' \
  --declare-hid 'mouse 046d:c548:2 host=1' \
  -A take-screen -A take-hid \
  -D "notify-send 'soft-kvm' 'display left'"
```

**Behavior:**

1. On start: read `agent.json`, `GET /state`, reconcile per §4.3. First run
   after a state-file loss never switches.
2. Detector sees receiver attach → `--settle` stable → guards pass →
   `POST /claim/<id>` (3 retries, each attempt bounded to 5 s, exponential
   backoff with full jitter to 30 s; failure logged, never fatal).
3. Watcher: `GET /wait?epoch=N&id=<me>` (client timeout 60 s) → on wake,
   reconcile as in (1). A connection error is a reconcile trigger, not a fatal.
4. Arrival watcher: while a device assignment names this host as assignee and is
   not `arrived`, poll the declared device every 1 s for 60 s and
   `POST /arrived` when it holds the link (§4.3).
5. Guards re-evaluated every cycle; off-guard ⇒ fully dormant, zero traffic,
   re-checking the guards every 15 s (macOS).
6. **Sleep detection:** after each `/wait` long-poll, compare wall-clock
   progress against monotonic elapsed time. A wall-clock jump greater than twice
   the client's internal `/wait` timeout means the host slept through the poll:
   reconcile immediately rather than waiting out the dead socket, which can hang
   for minutes after a lid-open.

### 5.5 HID declarations and `take-hid`

```
--declare-hid 'NAME VID:PID[:SLOT] [host=N]'
```

A declaration says how one device reaches this host. `--trigger` names the ones
to watch (§5.4). `take-hid` posts every one that carries a `host=N`, and each
other host that declares the same `NAME` sends its end of the device over with
HID++ 2.0 `changeHost` (`0x1814`). In a setup without the USB switch, an
Easy-Switch key (keyboard) or host button (mouse) press remains the gesture —
the moved device's attach is the §6.1 trigger — and giving the assignments away
makes the other peripherals follow.

- **`NAME`** is the device's identity across hosts: 1-64 characters of
  `[A-Za-z0-9._-]`, the same on every host. No address can serve: one mouse is
  `046d:c548:2` behind a Bolt receiver on one host and a Bluetooth VID:PID on
  the next. `detect` names a device after its model — the name its receiver
  stores for the slot, or the HID product string of a device attached on its own
  — in lower case with `-` between words: `MX Master 3S` is `mx-master-3s` on
  every host. Two devices of one model are numbered by address, lowest first
  (`mx-master-3s-2`), an order another host need not share: `detect` marks both
  with ♊ and says to check that their names mean the same devices on every
  host.
- **`VID:PID[:SLOT]`** is the device's address on this host, spelled here and
  nowhere else: `--trigger` refers to the declaration by `NAME` (§5.4). A bare
  `VID:PID` is a peripheral whose HID node comes and goes with its link —
  Bluetooth. `:SLOT` (1-6) is one pairing slot of a receiver; the VID:PID is
  then the receiver's, since behind a receiver the host only sees the receiver.
  Slots persist in the receiver's flash until re-pairing, so a hardcoded slot is
  stable — but re-pairing can renumber, and the declaration then silently names
  the wrong device. `detect` prints the current slot map. **A receiver is
  declared per slot:** its bare `VID:PID` is always present, so `take-hid` would
  post it as arrived and nothing would move.
- **`host=N`** is the Easy-Switch channel this host occupies on that device, `N`
  as printed on the key (1-3), matching the 1-based pairing slots. It is static,
  and taking the device requires it. A declaration without it names a device
  this host watches as a trigger and gives away, never one it takes: the form
  for a trigger that is no Easy-Switch device — a plain keyboard, a receiver
  moved by a USB switch — which `detect` prints for those. **No other host can
  derive it** — `changeHost` reports how many hosts a device supports and which
  one it is on, never which of the others is the peer, and a device may report
  three paired hosts for two computers — and the assignee has to say it before
  the device is there to ask. The channel differs per device: one host is 2 on
  the keyboard and 3 on the mouse. `detect` prints it for a device linked here.
  A device paired but linked elsewhere stays out of the suggested `connect`
  line, which runs as printed: `detect` prints its flag below the line as a
  comment marked ⚠️, with `host=N` left to fill in.

A host gives a device away against its own declaration of `NAME`. A device not
linked here — HID node absent, receiver absent, slot off the link — is nothing
to move, not a failure: otherwise the peripheral that carried the gesture would
trip the §4.3 breaker on every switch. A linked one gets `setHost(N-1)`, and the
host waits for its arrival on the assignee (§4.3).

Mechanics: the host probes the device's HID interfaces for one that answers
HID++. Vendor-defined interfaces are tried first (usage page `0xFF43` on
Bolt/eQUAD, `0xFF00` on Unifying), because receivers expose HID++ there. Some
Bluetooth HID++ devices flatten the vendor collection into the same HID node as
the mouse or keyboard collection, and the `telesma-app/hid` enumerator reports
only the primary usage, so interfaces without a vendor page are tried after the
vendor ones. Each candidate must answer a HID++ `getFeature(IRoot)` before it is
used; the handshake nudges a few times before giving up, because a dozing
Bluetooth device can ignore the first requests until its vendor channel wakes.
When the direct index stays silent to HID++ 2.0, one HID++ 1.0 register read
settles it: receivers speak only 1.0 registers at `0xFF`, answered locally from
their own flash, so even an error reply proves Logitech framing. The 7-byte
short report (`0x10`) is preferred: Bolt receivers STALL the 20-byte
`SET_REPORT` on their control pipe. Replies may still arrive long. Bluetooth Low
Energy peripherals (e.g. Ergo K860, Lift) omit `0x10` from their report
descriptor and declare only the 20-byte long report (`0x11`). macOS refuses
`0x10` on them with `kIOReturnNotFound`, so the request is resent as `0x11` and
later ones go out long from the start. Linux never refuses an undeclared report:
hidraw passes it through and BlueZ drops it, so paired with a Linux host these
peripherals stay silent and the handshake times out. Kinds come from
`getDeviceType` (feature `0x0005`, function 2) — one byte, no name reads. A
receiver's slot map is its local pairing table (HID++ 1.0 register `0x2B5`,
sub-registers `0x20+slot-1` on Unifying, `0x50+slot` on Bolt): on-chip flash, no
RF, so it is right whether a paired device is awake, asleep, off, or switched to
the other host. A slot holds the link when a HID++ request addressed to it is
answered; an unlinked slot refuses locally in about 2 ms (§6.1). **No reply to
`setHost` is not a failure**: a device that switched drops the link to this host
mid-ACK, which over Bluetooth surfaces as a read error (EIO on Linux hidraw),
not a timeout; an explicit HID++ error reply is the failure, and the arrival is
the receipt. On Linux the agent needs write access to the hidraw node (§6.1).

### 5.6 Screen declarations and `take-screen`

```
--declare-screen 'NAME INPUT [TABLE]'
```

A declaration says which input of which monitor this host is plugged into.
`take-screen` posts each one as the write that selects that input, and every
other host that has the monitor attached writes it. Only a host that takes
declares: a host that gives writes what the wire carries, through the backend
this OS has — `ddcutil` on Linux, `IOAVService` on macOS.

- **`NAME`** is the monitor's EDID name (descriptor `0xFC`), compared ignoring
  case. Every host attached to a monitor reads the same EDID, so the name
  matches across hosts with nothing to agree on; `detect` prints it. A host with
  two attached monitors of one name fails to give it away rather than pick.
  TODO(edid-serial): tell them apart by EDID serial where the monitor has one —
  not every EDID does.
- **`INPUT`** is the port *this* host is plugged into: one of the table's
  aliases, or a raw value (`0x1B`, `27`) written as is, for a port the table
  lacks. `detect` reads it off the monitor while this host is on screen.
- **`TABLE`** names how this monitor wants its input selected; omitted, it is
  `standard`. `detect` reads the monitor's manufacturer and prints the table
  when the brand needs one.

| Table      | Writes                                               | Aliases                                                                    | `detect` picks it for   |
| ---------- | ---------------------------------------------------- | -------------------------------------------------------------------------- | ----------------------- |
| `standard` | VCP `0x60` from I²C source `0x51`, MCCS Input Source | `dp1` `0x0F`, `dp2` `0x10`, `hdmi1` `0x11`, `hdmi2` `0x12`, `usbc1` `0x1B` | everything else         |
| `lg`       | VCP `0xF4` from I²C source `0x50`                    | `dp1` `0xD0`, `hdmi1` `0x90`, `hdmi2` `0x91`, `usbc1` `0xD1`               | EDID manufacturer `GSM` |

`0xF4` is LG's private side channel: firmware that ignores `0x60` writes honours
it. MCCS defines Input Source values up to HDMI 2 (`0x12`) and nothing for
USB-C: `usbc1` as `0x1B` is a convention, and vendors deviate,[^5^] which is
what a raw `INPUT` is for. A vendor is one more row.

**On the wire: `VCP:SRC=VAL`**, three hex bytes — the feature, the I²C source
address, the value. `usbc1 lg` travels as `F4:50=D1`, `dp1` as `60:51=0F`. The
taker resolves its alias; the writer needs no table for another host's port. A
writer accepts only the `VCP:SRC` pairs its own tables list (`60:51`, `F4:50`),
with any value, and refuses an assignment that carries anything else: the
features next to Input Source include factory reset (`0x04`) and power (`0xD6`),
and the token must not reach them (§9).

**Writing.** A host looks for a monitor named `NAME` among its attached displays
— the EDIDs under `/sys/class/drm/*/edid` on Linux, the IORegistry on macOS,
both in milliseconds and without DDC traffic. None ⇒ nothing to move here. One ⇒
one write, never read back (§4.3):

- **Linux — `ddcutil`.** The write is `ddcutil setvcp <vcp> <value> --noverify`
  with `--model NAME`, plus `--i2c-source-addr` when the source is not `0x51`.
  `--noverify` is load-bearing: ddcutil's own verification fails on firmware
  that goes silent on the input it just left, so a verifying write reports
  failure after succeeding.
- **macOS — `IOAVService`.** `screen` loads `CoreFoundation` and `IOKit` via
  `purego`, matches each external display from the IORegistry
  (`DCPAVServiceProxy` → EDID + `IOAVServiceCreateWithService`) and writes the
  DDC/CI `setVCP` packet (`0x03`) through `IOAVServiceWriteI2C`.

A write that fails — `ddcutil` missing, no `i2c-dev`, no permission, or no
matching `IOAVService` — fails to give the screen away with the backend's own
error and is retried (§4.3).

**What `detect` contributes.** It runs the backend once and prints the
`--declare-screen` flag inside each suggested `connect` line. On both OSes it
names each external monitor the backend reaches (leaving out Apple displays
whose EDID vendor is `APP` on macOS, since neither the built-in panel nor
Apple's monitors have an input to select), the table its EDID manufacturer maps
to, and the port this host is on — Input Source (VCP `0x60`) read once and
mapped back to an alias. The read only answers while this host is on screen, and
the `lg` table is assumed to report the standard value for each port (measured
for `dp1`, which reads `0x0F`): when either fails, `detect` takes the flag and
`-A take-screen` out of the `connect` line and prints them below it as a comment
marked ⚠️, with `DP1_HDMI_ETC` in place of `INPUT`, and says to re-run it once
the monitor shows this host. When the backend fails it prints the backend's
error instead: "`ddcutil` requires module `i2c-dev`" at setup time, not on the
first switch. A monitor `ddcutil` lists as an "Invalid display" — the I²C bus is
not this user's to open, or the monitor does not answer DDC — is a failure too,
though `ddcutil` exits 0: `detect` says which, and still names the monitor and
the table from its EDID.

### 5.7 `soft-kvm service <install|print|uninstall> [serve|connect] [args]`

Systemd install/print/uninstall for both roles, Linux only; on macOS the
subcommand refuses with a pointer to launchd (§6.2). Bare `service install`
prints the walkthrough as a shell transcript: the shared secret, a by-hand trial
of `detect`/`serve`/`connect`, then the two install commands. Each step runs
independently, reading `SOFTKVM_TOKEN` from the environment (never a flag, §9):

- `service install serve [PORT] [--state PATH] [--instance NAME]
  [--no-advertise]`
  (root): copies the running binary to `/usr/local/bin/soft-kvm` (`0755`, temp
  file + rename; skipped when the running binary already is that path), writes
  `/etc/soft-kvm/env` (`0600
  root:root`) from the embedded template, and
  installs/enables the canonical coordinator unit (§6.4). The unit's
  `ProtectHome=yes` rules out a binary under home, which is why the binary is
  copied rather than referenced. Re-running refreshes the binary and re-renders
  the env file from the template — hand edits are discarded.
- `service install connect [connect args]` (regular user; refuses root): writes
  `~/.config/soft-kvm/env` (`0600`) and installs/enables the canonical agent
  user unit, then points at `loginctl enable-linger` for boot-before-login. A
  host running only the agent skips the serve step entirely.

Both install steps validate their arguments with the same flag sets the
`serve`/`connect` subcommands use — and everything those validate after parsing
(the address normalisation, `--trigger`, the declarations, the `-A`/`-D`
commands and which of them may carry a take) — before writing anything, and bake
the *raw* argument tokens, systemd-quoted, into `ExecStart=`. Re-serialising
parsed flags would bake defaults read from the installer's environment —
`serve`'s `--state` default under sudo would be root's path, defeating
`StateDirectory=` (§6.4). Only the parsed `--trigger` counts: one inside a
command string (`-D 'somecmd --trigger foo'`) is part of that command, not the
trigger.

`service print serve|connect [args]` runs the same validation and rendering but
no euid, token or on-disk checks, and writes nothing — the unit goes to stdout
for review or piping. `service uninstall serve|connect` disables and removes the
unit; the env files and `/usr/local/bin/soft-kvm` stay.

## 6. Host components

### 6.1 Linux host

| Component          | Spec                                                                                                                                                                                                                                                                                                                                                 |
| ------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Detector (primary) | Two sources, fanned into one attach channel. `VID:PID`: HID device add/remove events via `telesma-app/hid` (netlink kernel uevents on `hidraw`, VID:PID from sysfs) — the same library and code path as macOS (§6.2). `VID:PID:SLOT`: the receiver's own device-connection notifications, read from its HID++ interface (needs the §5.5 hidraw rule) |
| Guards             | None (always-on desktop)                                                                                                                                                                                                                                                                                                                             |
| Screen write       | A screen assignment's write through `ddcutil` (§5.6); user in `i2c` group — no sudo                                                                                                                                                                                                                                                                  |
| Packaging          | Canonical units in `units/`, rendered and installed by `soft-kvm service install` (§5.7); the agent's env file is `~/.config/soft-kvm/env` (§6.4)                                                                                                                                                                                                    |

**Detector specifics** — the parts that are easy to get wrong:

- **One physical attach fires one event per HID interface** — the receiver
  exposes keyboard, mouse and raw interfaces, each reported as a separate device
  (hidraw node on Linux, IOHIDDevice on macOS) with the same VID:PID. The
  detector deduplicates by device path and emits a single attach edge
  (`detect_hid.go`); `--settle` covers the rest.
- **A permanently attached target must not mask the others.** The receiver is
  itself a useful `--trigger` and stays plugged in for months, so presence is
  tracked per target: one global count of attached interfaces would never return
  to zero, and every attach edge after the first would be lost.
- **Easy-Switch through a plugged-in receiver produces no HID event at all**, so
  a device declared by `VID:PID` never fires. `hid-logitech-dj` creates one
  child HID node per *pairing slot*, and that node lives as long as the pairing
  does, not as long as the device is linked: Easy-Switch moves the radio link,
  never the pairing, so nothing is added or removed. Measured on an ERGO K860
  through a `046d:c52b`: switching hosts and back produced zero `hidraw` uevents
  while `/dev/hidraw3` stayed put. Declare the device by `VID:PID:SLOT` for this
  topology — the receiver announces the link change itself.
- **A receiver cannot be polled for link state.** Register `0x02` returns the
  paired-device *count*, and the "announce every device" write (`0x02` ← `0x02`)
  replays a fabricated arrival whose link bit does not track reality — it
  reported a keyboard as linked while that keyboard was demonstrably on another
  host. Two things do tell the truth: the unsolicited `0x41` notification the
  receiver sends the instant a link changes (this is where the kernel's own
  `hidpp_battery_*/online` view comes from, ~160 ms later), and a HID++ request
  addressed to a slot, which an unlinked slot refuses locally in about 2 ms with
  no RF. The detector listens for the first; `detect` uses the second to show
  which slots hold the link.
- **Unprivileged operation.** On Linux the library binds netlink multicast group
  1 (raw kernel uevents) — readable without root on current kernels, so a
  systemd *user* unit suffices. VID:PID metadata comes from
  `/sys/class/hidraw/*/device/uevent`, world-readable: no udev rule.
- The Unifying receiver `046d:c52b` is on the same bus. Filter exactly.
- **Sending a device needs hidraw write access** (§5.5). The detector only reads
  uevents, but giving a device away, `take-hid`, the arrival poll and `detect`
  open the receiver's hidraw node `O_RDWR`: add a udev rule (`TAG+="uaccess"` or
  a group), as Solaar's packaging does.

**NixOS prerequisites** (none of which are in place today, finding 12):

```nix
hardware.i2c.enable = true;              # i2c-dev + i2c group + udev rules
users.users.brice.extraGroups = [ "i2c" ];
users.users.brice.linger = true;         # user units die at logout otherwise
environment.systemPackages = [ pkgs.ddcutil ];
```

`--i2c-source-addr` is a recent ddcutil flag; check `ddcutil --version` before
trusting the `lg` table. On a DisplayPort link the DDC channel rides DP AUX, so
the `/dev/i2c-N` number is created by the `xe` driver and **changes on monitor
hotplug and across reboots**. Do not pin `--bus`: the write selects the monitor
with `--model NAME` (§5.6), and ddcutil's scan costs ~1–3 s.

### 6.2 macOS host (corp laptop)

| Component          | Spec                                                                                                                                                                                                                                                                                            |
| ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Guards             | `pmset -g ps` contains `AC Power`, and the LG is present                                                                                                                                                                                                                                        |
| Detector (primary) | `IOHIDManager` device-matching events filtered to VID:PID, via `telesma-app/hid` (purego — dynamic FFI, no cgo, static binary preserved); the same library and code path as Linux (§6.1). No `IOHIDManagerOpen`: enumeration callbacks need no device access, so no Input-Monitoring TCC prompt |
| Screen write       | A screen assignment's write through `IOAVService` (§5.6) — in-process via `purego`, no external helper                                                                                                                                                                                          |
| Packaging          | `~/Library/LaunchAgents/local.soft-kvm.plist` (`RunAtLoad`, `KeepAlive`), token in a `0600` env file, log to `~/Library/Logs/`                                                                                                                                                                  |
| Installs           | One binary + one plist. `pmset` is stock; the guard lists the monitors from the IORegistry in-process (§5.6), with no external helper                                                                                                                                                           |

- **Sending a device needs Input Monitoring** (§5.5). Enumeration and the attach
  events need no TCC grant, but giving a device away — like `detect`'s scan —
  opens the keyboard or mouse HID node for the HID++ write, and macOS gates that
  on the terminal's Input Monitoring permission (System Settings → Privacy &
  Security → Input Monitoring). `detect` names the setting when a scan is
  denied.
- **The display-presence guard is a trap.** With Deep Sleep on, the LG drops the
  link on the input it is not showing, so "LG present" is false exactly when the
  Mac is docked but not active — and the guard would make the Mac permanently
  unable to claim. Deep Sleep off (finding 11) fixes it; belt and braces, treat
  the LG as present for 10 minutes after it was last seen.
- **Guards down means receiver absent.** The detector's attach-edge state resets
  whenever the guards fail, so a Mac that slept through the dock sees a fresh
  attach edge when the guards pass again, and claims — even though the USB
  attach itself happened while the agent was suspended.
- **`system_profiler SPDisplaysDataType` costs 1–3 s** and wakes the discrete
  GPU path. Do not call it on the guard's cadence: the guard lists the external
  monitors from the IORegistry instead — `DCPAVServiceProxy` entries named from
  their EDID, Apple's own displays left out — which takes milliseconds, and
  `--display-match` matches a substring of those names. Keep `system_profiler`
  for `activate`-time diagnostics.
- **launchd throttles respawns to one per 10 s.** A crash loop is silent except
  in the log; `KeepAlive` will not tell you.
- **macOS 13+ lists `KeepAlive` agents under Login Items → Allow in
  Background**, and MDM can disable them. Confirm the agent survives a reboot on
  the *corp* profile, not on a personal Mac.
- **Gatekeeper:** Go's linker ad-hoc-signs `darwin/arm64` output even when
  cross-compiled from Linux, so the binary runs. The blocker is the
  `com.apple.quarantine` xattr, which attaches to anything fetched by a browser
  or AirDrop; `scp` does not set it. If it is set,
  `xattr -d com.apple.quarantine soft-kvm`.

### 6.3 Bluetooth trigger

For environments without the USB switch, a keyboard paired directly over
Bluetooth works as a trigger on **both** OSes: BT HID surfaces like any HID
device (a `hidraw` node on Linux, an `IOHIDDevice` on macOS), so the §6.1-6.2
detector sees it — declare the keyboard by its own VID:PID and name it in
`--trigger`. Only **connect** events trigger anything (finding 2); a disconnect
never does, it only re-arms that target's attach edge. Two caveats:

- The identity is VID:PID, not a MAC address: two keyboards of the same model
  are indistinguishable.
- A BT keyboard that reconnects on its own (wake from idle) produces an attach
  edge without user intent.

### 6.4 Carrying the bit

Any always-on host. The desktop is the shortest path — it is always on, and it
is where §13's relay board has to live. The Pi buys only neutrality, which is
worth less than it looks: a desktop that is down cannot be a party to any
switch, since its own DDC is dead, so a claim lost during its reboot costs
nothing.

- State: declare `StateDirectory=soft-kvm` in the unit and the default `--state`
  lands in `/var/lib/soft-kvm/state.json` (system unit) or the user's own state
  directory (user unit), whatever `User=` it runs as. Token via
  `EnvironmentFile=`, mode `0600`. `soft-kvm service install serve` writes it as
  `0600 root:root` and installs the binary to `/usr/local/bin/soft-kvm` — the
  unit's `ProtectHome=yes` rules out a binary under home (§5.7). The agent's
  per-user copy is `~/.config/soft-kvm/env`, also `0600` (§6.1).
- Avahi will fight `grandcat/zeroconf` for UDP 5353 if both try to own it. The
  library sets `SO_REUSEADDR`/`SO_REUSEPORT` and coexists; where it does not,
  register through Avahi with a static `/etc/avahi/services/soft-kvm.service`
  and pass `--no-advertise`.
- `net/http` with Go ≥ 1.24 `ServeMux` patterns (`POST /claim/{id}`) — method
  and wildcard matching without a router dependency. The TLS identity comes from
  `crypto/hkdf` (stdlib since Go 1.24), which is what sets the floor.
- `/wait` uses a **broadcast channel**: state holds a `chan struct{}` that is
  closed on every epoch change and replaced under the same lock. Waiters
  `select` on it, `time.After(50s)`, and `r.Context().Done()`. Not `sync.Cond`,
  which has no timed `Wait` and would need a second goroutine per waiter.
- `WriteTimeout` must exceed 50 s or be zero, otherwise the server kills its own
  long-polls. `ReadHeaderTimeout` 10 s.
- **Atomic state writes:** temp file in the same directory, `fsync`, `rename`. A
  truncated `state.json` after a power cut must not crash-loop the service — on
  parse failure, log and start from `owner=""`, `epoch=0`. A fresh server with
  no state file starts the same way; the §4.3 first-reconcile rule makes the
  empty owner safe.
- Claims serialized under one lock; last claim wins.
- No Home Assistant dependency.

## 7. Service API spec

**Base:** `https://<coordinator>:8700` · **Auth:** mutual TLS proves both peers
hold `SOFTKVM_TOKEN`: the server identity and the client certificate are both
derived from it (§9). The secret is scoped by construction: it can read and flip
one ownership bit, and ask the other hosts for one of two bounded writes (§9).

| Endpoint                                  | Behavior                                                                                                                                                                                                                                                                                      |
| ----------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /claim/{id}?retake=true&force=true` | Idempotent. A change of owner increments `activation` and `epoch` and wakes waiters. With `retake` (sent by `activate` only) an unchanged owner increments both and wakes too. `force` admits an `id` with no live agent. → `200 {owner, epoch, changed}` · `400` invalid id or no live agent |
| `POST /take/{id}`                         | Body `{activation, takes: [{kind, name, input \| channel, arrived}]}`. Records one assignment per take. → `200 {epoch, changed}` · `409` when `id` is not the owner or `activation` is not the current one · `400` on a malformed take                                                        |
| `POST /arrived/{id}`                      | Body `{arrivals: [{kind, name, seq}]}`. Marks an assignment `arrived` when `id` is its assignee and `seq` is its current one; anything else is ignored. → `200 {epoch, changed}`                                                                                                              |
| `GET /state`                              | → `200 {owner, epoch, since, activation, live, server_id, assignments}`                                                                                                                                                                                                                       |
| `GET /wait?epoch=N&id=<me>`               | Long-poll. Returns `200 {owner, epoch}` immediately when `epoch ≠ N`; `204` after 50 s. Registers `id` as live for the duration, plus the grace of §4.3                                                                                                                                       |

- **An assignment** is
  `{kind, name, assignee, prev, seq, input, channel, arrived}`. `kind` is
  `screen` or `hid`, and `(kind, name)` is the key: a monitor and a device may
  share a name. `prev` is the assignee before this one, empty on a first take.
  `input` is a screen's `VCP:SRC=VAL` (§5.6); `channel` is a device's
  Easy-Switch channel, 1-3 (§5.5). `assignments` is sorted by kind, then name,
  and always present.
- **Recording a take.** The same assignee, the same `input` or `channel` and the
  same `activation` as the assignment already there change nothing — a restarted
  agent re-posts its takes for free. Anything else replaces the assignment:
  `prev` becomes the old assignee when the assignee changes and stays otherwise,
  `seq` becomes the new `epoch`, `arrived` is what the take said, and waiters
  wake.
- **What the server validates** is shape, not meaning: `name` is 1-64 printable
  characters, `input` is three hex bytes as `VCP:SRC=VAL`, `channel` is 1-3, a
  request carries at most 16 takes. Which `VCP:SRC` pairs are safe to write is
  the writer's check (§5.6).
- **Clients treat `epoch`, `seq` and `activation` as opaque.** Adopt whatever
  the server returns and compare for equality only. A restored-from-backup or
  reset `state.json` moves them backwards, and a client that only waits for
  `epoch > N` spins at line rate.
- `since` is RFC 3339, the time of the last change of owner. `live` is
  `{"linux": true, "mac": false}`, derived from open `/wait` connections plus
  the grace of §4.3. `server_id` is a UUID regenerated on every process start,
  so an agent can tell a restart from a state change.
- `state.json` persists `owner`, `epoch`, `since`, `activation` and
  `assignments`.
- LAN bind, HTTPS only. TLS 1.3, identity derived from the shared secret (§9).

## 8. Edge cases

| Case                                                              | Behavior                                                                                                                                                                                                                                                                                                                             |
| ----------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Claimant writes nothing to the monitor                            | By design — DDC unreachable from inactive input                                                                                                                                                                                                                                                                                      |
| Simultaneous claims                                               | Serialized; last wins; both agents reconcile. The loser of the race gets a 409 on its take                                                                                                                                                                                                                                           |
| Flip back while the activation commands still run                 | The late take carries a stale `activation` and gets a 409; the activation fails its retries, is logged, and the deactivation runs right after (§4.3)                                                                                                                                                                                 |
| A host asleep/off when a take is recorded                         | It gives away what the assignment names at resume, once, if the assignee is still live — the self-heal for a monitor it was left showing. A device assignment already `arrived` is skipped                                                                                                                                           |
| Agent starts with no `agent.json`                                 | Adopts the owner, the activation and every assignment as handled; runs nothing                                                                                                                                                                                                                                                       |
| Monitor moved by OSD, then a take                                 | The record names the wrong previous assignee. Every host writes, so the host really on screen moves the monitor; if its write fails, it stays silent and the recorded previous assignee notifies where nobody looks (§4.3)                                                                                                           |
| Device linked to a host the record does not name                  | Same: every host that declares it tries, the one that holds the link sends it                                                                                                                                                                                                                                                        |
| Mouse stranded on the other host while this one owns              | No attach re-takes it: an attach-driven claim is idempotent. `soft-kvm activate <owner>` re-posts the takes, and the assignments are given away again (§4.3)                                                                                                                                                                         |
| Peripheral powered off or asleep at the take                      | Not linked anywhere: no host has anything to move, nothing arrives, nothing notifies. It links wherever its own channel selector points when it wakes                                                                                                                                                                                |
| Device sent, never arrives                                        | The sender re-sends after 10 s, up to `--switch-retries`; the previous assignee then notifies. The assignee stops polling 60 s after the take, so a later arrival leaves the assignment un-`arrived` — harmless: a host that gives it away later finds nothing linked                                                                |
| `host=N` wrong in a declaration                                   | The device goes to a channel nobody listens on, never arrives, and the previous assignee notifies. The user brings it back with its own selector                                                                                                                                                                                     |
| Receiver declared without `:SLOT`                                 | Always present, so `take-hid` posts it as arrived and nothing moves (§5.5)                                                                                                                                                                                                                                                           |
| A host has no monitor of the taken name                           | Nothing to move; silent. Also the outcome of a mistyped `NAME` on the taker: no host matches, the monitor stays, nobody notifies — `detect` fills the name in so it is not typed                                                                                                                                                     |
| Two attached monitors share the taken name                        | The write fails rather than pick; retried, then the previous assignee notifies (§5.6)                                                                                                                                                                                                                                                |
| Take carries a `VCP:SRC` no table lists                           | Every writer refuses it and fails to give the screen away; the previous assignee notifies (§5.6)                                                                                                                                                                                                                                     |
| Receiver unplugged while `connect` runs                           | `watchReceiver` fails, which returns from `Detector.Run`; `detectorLoop` restarts every source on backoff. The HID detector's startup snapshot then re-fires one attach edge for targets that were already present, so a receiver flap costs one extra claim — idempotent and coalesced, so it changes no owner and re-takes nothing |
| Coordinator unreachable                                           | Claims and takes logged-and-lost; agents back off exponentially (full jitter, 1 s base, 30 s cap — jitter keeps the agents from retrying in lockstep after a shared outage); OSD fallback                                                                                                                                            |
| Mac away / on battery / undocked                                  | Guards suppress everything; the bit may change without the Mac, and nothing is taken towards it — a take needs its agent                                                                                                                                                                                                             |
| Receiver flapping                                                 | 2 s settle + change-only notification + idempotent claims + circuit breaker                                                                                                                                                                                                                                                          |
| Monitor already on the taker's input                              | Every host writes anyway: nothing probes first (§4.3). The monitor ignores a write from an inactive input; where the backend errors on it instead, the retries run and the previous assignee notifies once                                                                                                                           |
| Token mismatch after rotation                                     | Connections fail TLS verification (the certificate derives from the token); nothing switches until the env files and the running units agree again (§9)                                                                                                                                                                              |
| Server restarted                                                  | Long-polls reset; agents reconcile on error; `server_id` change is not by itself a state change. Assignments are persisted, so no assignment is given away twice                                                                                                                                                                     |
| Server state lost                                                 | `owner=""`, no assignments. Agents keep their records; the next assignment of each name is new to them and is given away — unless its `seq` lands on the one on record, since epochs restart at 0                                                                                                                                    |
| **Mis-flip: monitor points at a host that cannot switch it back** | Unrecoverable over the network: the host on screen is the one that would have to write. With Auto Input Switch off, the OSD button is the only recovery, and no agent is left able to notify. A take only ever comes from a running agent (§4.3), which is what keeps this state out of reach                                        |
| Screen write succeeds but the monitor does not move               | Silent: a screen has no receipt (§4.3). The bit and the assignments stay correct and the user presses OSD                                                                                                                                                                                                                            |
| Write hangs / its result is lost                                  | `--switch-timeout` SIGTERMs the child and the write counts as failed: retried, then notified on the previous assignee and logged (§4.3)                                                                                                                                                                                              |
| Result arrives after its issue ended                              | Ignored and logged; the decider only accepts a result while its issue is outstanding (§4.3)                                                                                                                                                                                                                                          |
| Deep Sleep left on                                                | Every switch hot-unplugs the monitor on the host being left: Hyprland collapses workspaces onto nothing, macOS rearranges windows. Set it Off                                                                                                                                                                                        |
| mDNS browse returns nothing                                       | Cached address is tried first and usually still valid; otherwise back off to 60 s and keep browsing. No claims are lost that a `SOFTKVM_SERVER` override would not also have lost                                                                                                                                                    |
| mDNS returns a stale or rogue record                              | Connection fails TLS verification — a rogue record points at a host that does not hold the secret and cannot present the derived certificate (§5.1, §9). Agents re-browse on any connection error rather than pinning the first answer                                                                                               |
| Server moves host                                                 | Nothing is reconfigured; the new instance advertises, caches expire on first failure                                                                                                                                                                                                                                                 |

## 9. Security notes

- **TLS identity is derived from the shared secret.** Every instance runs
  `HKDF-SHA256(SOFTKVM_TOKEN, info="soft-kvm tls v1")` into an Ed25519 seed. The
  same key and seed self-sign a fixed-template CA certificate (serial 1) and
  sign a fixed-template client certificate (serial 2). Both templates are
  constant and Ed25519 signing is deterministic, so all assignees of the secret
  generate byte-identical certificates. The server presents the CA certificate;
  clients trust exactly it (the derived certificate is their only root,
  `ServerName` pinned to its SAN) and present the client certificate back. No
  fingerprint comparison, no PKI, no config beyond the token. Rotating the token
  rotates the identity for free.
- The certificate is publicly derivable from the secret, so anyone who has seen
  a handshake can test candidate secrets **offline** against the observed
  certificate. Neither derivation carries a salt — they cannot: client and
  server derive the same material from the token alone, so any salt would have
  to be broadcast, where the attacker sees it too. Entropy is the only defence,
  and it is enforced at the edges: the binary refuses tokens under 16 characters
  and warns under 32. `SOFTKVM_TOKEN` must be a generated high-entropy string,
  never something memorizable.
- Token blast radius: read and flip one ownership bit, point a shared monitor at
  any of its inputs, send a declared peripheral to any of its three channels.
  Rotating = editing the env files and restarting the server and both agents —
  the token is read at process start, and handshakes fail until they agree (§8).
  The token never appears in `ps` or shell history.
- No Home Assistant credentials anywhere in the system.
- Corp device surface: one binary, outbound HTTPS to one LAN host, no listeners,
  no credentials of value, no installs. mDNS adds outbound multicast on UDP 5353
  and an inbound multicast socket — a listener in the kernel sense, and a point
  worth checking against the corp policy before it is discovered by someone
  else.
- The service is advertised to the whole LAN. What leaks is "a soft-kvm server
  exists here", never the token (§5.1). The `kh=` TXT value broadcasts only a
  truncated fingerprint — 8 bytes of
  `HKDF-SHA256(SOFTKVM_TOKEN, info="soft-kvm key fingerprint v1")`, hex — safe
  because it is truncated, domain-separated from the TLS identity, and the token
  is high-entropy.
- The agent executes the `-A` and `-D` commands as given. Each is an argv slice
  — `connect` splits the flag's string itself, with no expansion — never a shell
  string: no `sh -c`, no interpolation of anything received from the server.
- What the server relays is data for two fixed writes: a screen take's
  `VCP:SRC=VAL`, of which a writer accepts only the `VCP:SRC` pairs its built-in
  tables list (§5.6), and a channel 1-3 for a peripheral this host declared
  (§5.5). The server can flip a bit and aim those two writes; it can never
  choose what runs.

## 10. Integration tests

Run at the end, against the assembled system. Both writes already work by hand
(finding 9), so nothing here blocks writing the code — these check the seams,
and every one of them needs hardware that is not on the desk yet or a binary
that does not exist yet.

- [ ] Four-combo test with the USB switch (`lsusb` /
      `ioreg -r -c IOUSBHostDevice`: switch-to-me / away × receiver present /
      absent) — clean attach/detach, < 2 s, on **both** hosts. If this fails
      there is no detector, and no detector means no trigger
- [ ] Per-port VCP codes recorded for every input in use, from both hosts
- [ ] Giving a device away fails on purpose (declare a `host=N` no host is
      paired on): the sender retries, the notification appears on the previous
      assignee, no reclaim loop
- [ ] `ddcutil getvcp 60` succeeds **iff** the Linux input is active — and
      confirm it still succeeds after the monitor sleeps and wakes, since §13's
      detector and `detect`'s own-port read rest on it
- [ ] A screen declared with **no table** on the Mac, written from Linux: if the
      LG honours the standard `0x60` write, `GSM` should not map to `lg`
- [ ] `detect` prints the same monitor `NAME` on both hosts, and it is the name
      under `/sys/class/drm/*/edid` and the one `ddcutil --model` accepts
- [ ] `detect` prints the same device `NAME` on a host that reaches the device
      through a receiver and on one that reaches it over Bluetooth (read on a
      Unifying and a Bolt receiver: `MX Master 3`, `MX Master 3S`, `LIFT`; the
      Bluetooth product string is unmeasured)
- [ ] `soft-kvm detect` on each host names the monitor, the table and this
      host's own port; `ddcutil detect --terse` and `getvcp 60 --terse` on Linux
      and `IOAVServiceReadI2C` on macOS match what the parsers were written
      against
- [ ] `soft-kvm detect` on the Mac with the lid open leaves the built-in panel
      out: its EDID manufacturer is `APP`
- [ ] The monitor is already on the other input when a host writes: note whether
      `ddcutil setvcp --noverify` errors or is silently ignored (§8)
- [ ] Fifty switches back and forth: count the screen writes that did not land
      and the device give-aways that needed a retry. If devices commonly arrive
      later than 10 s, the arrival window and the assignee's 60 s poll need
      different numbers than the guessed ones
- [ ] Arrival is seen on the assignee for each kind of declaration: a receiver
      slot taking the link, a Bluetooth HID node appearing, on both OSes
- [ ] `soft-kvm activate <owner>` brings back a mouse moved away by hand, and a
      monitor moved by OSD
- [ ] Re-claim the current owner fifty times: the epoch never moves and no agent
      wakes (idempotent claims, §7)
- [ ] Claim-to-pixels latency on the end-to-end pass: ≤ 10 s — settle and
      long-poll wake dominate
- [ ] LG OSD: Deep Sleep Off on both ports; confirm the losing host keeps the
      display connected after a switch (no Hyprland workspace collapse)
- [ ] USB detector fires once per physical attach, not once per HID interface,
      and does not fire for the Unifying receiver `046d:c52b`
- [ ] A trigger declared by `VID:PID:SLOT`: Easy-Switch away and back fires an
      attach edge every time, not only on the first, with the receiver left
      plugged in
- [ ] Giving a device away (§5.5) moves the mouse from each host: over Bluetooth
      from the Mac (bare `VID:PID`), through the Bolt receiver from Linux
      (`:SLOT`). `detect`'s slot map matches `solaar show`
- [ ] A device given away from the Mac reaches a Bluetooth LE peripheral that
      declares no `0x10` (Ergo K860, Lift): `SOFTKVM_LOG=debug` shows `0x10`
      refused with `0xe00002f0`, then `0x11`. Paired with Linux, the same
      peripheral times out (§5.5)
- [ ] Agent silent on battery; silent with display unplugged; resumes < 30 s
      after dock, including after a multi-hour lid-closed sleep
- [ ] Off-LAN test (laptop on hotspot): zero claims, no error spam
- [ ] Coordinator restart → state recovered; `kill -9` mid-write → state file
      still parses
- [ ] Boot the desktop while the bit says `mac` → **nothing switches**
- [ ] `soft-kvm activate mac` with the Mac powered off → refused without
      `--force`
- [ ] LaunchAgent survives reboot under the corp MDM profile
- [ ] LG OSD: Auto Input Switch **Off** on all ports; confirm the monitor stays
      on a dead input rather than scanning away from it
- [ ] **macOS local network access granted** to the binary under the corp MDM
      profile, for both the mDNS browse and the HTTPS long-poll — a denial is
      silent and looks exactly like a network outage (§5.1)
- [ ] mDNS browse from the Mac **over WiFi** resolves the server in < 3 s,
      repeatedly, including after the AP roams. If not, set `SOFTKVM_SERVER` on
      the Mac and treat discovery as a Linux-only convenience
- [ ] Kill the server, move it to the other host, restart: agents reconnect with
      no config change
- [ ] Two-step `soft-kvm service install` (§5.7): bare `install` prints the
      guidance; `install serve` under sudo installs `/usr/local/bin/soft-kvm`
      and `/etc/soft-kvm/env` (`0600 root:root`); `install connect` as the user
      writes `~/.config/soft-kvm/env` (`0600`)
- [ ] Coordinator runs from the copied binary:
      `systemctl status
      soft-kvm-serve` is active (running) despite
      `ProtectHome=yes` and a mode-0700 home
- [ ] Agent runs from boot: `loginctl enable-linger`, reboot, the agent claims
      before any login

## 11. Implementation notes

**No CLI framework.** Five subcommands, one `flag.NewFlagSet` each, a switch on
`os.Args[1]`. Cobra costs two modules and actively hurts here: pflag parses
flags interspersed with positional arguments, so `-vcp=inputSelect` inside a
switch command is read as an unknown flag unless interspersal is turned off.
Stdlib `flag` has no such problem because a command is the *value* of `-A` or
`-D`, one string, and `connect` splits it itself (§5.4) — a flag with a
`flag.Value` that accumulates, so repeating it adds a command. The
stop-at-first-positional rule would silently ignore a flag placed after a
positional, so no subcommand takes positionals it does not name: `connect`,
`serve`, `activate` and `detect` reject any argument past theirs, with a hint
when the extra looks like a flag — and, for `connect`, a pointer to `-D` when it
is the old trailing `-- CMD`.

**Package layout.** One module, one binary, eleven packages under the root
`main`: `state` (the `/state` wire type and atomic JSON persistence), `model`
(§11.3), `identity` (TLS identity and `kh=` fingerprint from the shared secret),
`discover` (mDNS advertise/browse; the address cache sits behind a `Resolver`
whose cache path is injected), `platform` (the argv runner, the per-OS defaults,
and the per-OS `Guard` — a concrete type: no-op on Linux, pmset+display on
macOS), `detect` (HID enumeration for the subcommand, and the two attach
detectors both OSes share), `hidpp` (Logitech HID++: device declarations,
`changeHost`, link presence, §5.5), `screen` (screen declarations, the mapping
tables, the `VCP:SRC=VAL` wire form and the per-OS write backends, §5.6),
`client`, `server`, and `agent` (supervisor, generations, decision loop, claims,
the worker that runs commands and gives assignments away, the arrival watcher;
the `Detector` and `Guard` interfaces and the func seams of §11.2 live here, in
their only consumer). `main` keeps flag parsing and wiring. Layering is a DAG:
the leaves (`state`, `identity`, `discover`, `platform`, `hidpp`, `screen`)
import nothing internal; `detect` imports `hidpp`, `platform` and `screen`;
`model` imports `state`; `client` imports `state` and `identity`; `server` those
two and `hidpp` (the channel bound it validates on a take); `agent` imports
`model`, `state`, `client`, `discover`, `hidpp` and `screen`. Build tags in
filenames, not in `//go:build` lines, wherever the split is per-OS (`platform`,
`screen`).

**Logging is `log/slog`** to stderr, text handler — journald on Linux, the
LaunchAgent log file on macOS. Every switch decision logs at Info with the
reason it passed or failed each §4.3 gate; nothing else does.

### 11.1 Cancellation

`signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` in `main`, one
context down through everything. `context.Context` is a first parameter, never a
struct field. Three blocking things do not respect it by default:

- **The HID watcher.** Its event loop lives on a library goroutine (a locked OS
  thread running a CFRunLoop on macOS, a netlink poll on Linux), which ctx
  cancellation cannot reach. `detect/detect_hid.go` defers `Watcher.Close()`,
  which stops that loop from outside; on macOS the shutdown latency is bounded
  by the run loop's 1 s wakeup.
- **Child processes.** `exec.CommandContext` sends SIGKILL on cancel, which can
  cut `ddcutil` mid-I2C transaction. Set `cmd.Cancel` to send SIGTERM and
  `cmd.WaitDelay = 2 * time.Second` so SIGKILL is the fallback rather than the
  first move.
- **In-flight long-polls at shutdown.** `srv.Shutdown` waits for active
  requests, and a `/wait` handler is active for up to 50 s. Give the server
  `BaseContext: func(net.Listener) context.Context { return ctx }` so every
  request context is a child of the process context: one cancel returns every
  waiter immediately, and `Shutdown` then completes in milliseconds.

Supervision is `errgroup` (`golang.org/x/sync`): one group per guards-up
generation — the detector, watcher, guard watcher, and decision loop — whose
first error, the `errGuardsDown` sentinel or `ctx.Err()`, cancels the rest, so a
guard flap tears down and rebuilds the generation. Two more groups are
run-level, cancelled only by process shutdown: the worker (so a switch that
started guards-up finishes guards-up — a guard flap must not SIGTERM a switch
mid-I2C) and the claimer, which takes a claim over an unbuffered channel only
while none is in flight, so a redundant claim trigger coalesces instead of
overlapping an in-flight retry. The guard poll no longer blocks the decision
loop.

### 11.2 What is an interface, and what is not

An interface earns its place when two real implementations exist. Two do, both
defined in `agent`, their only consumer:

```go
type Detector interface {  // detect.HIDDetector and a fake
    Run(ctx context.Context, attach chan<- struct{}) error
}
type Guard interface {     // platform.Guard (no-op on Linux), and a fake
    OK(ctx context.Context) (ok bool, reason string)
}
```

`Guard.OK` returns the reason as well as the verdict because "dormant, no AC
power" and "dormant, no LG" are different bug reports.

What does *not* get an interface:

- **The commands, the monitor and the devices.** One implementation each, so
  each seam is a func type in `agent`, not an interface. `Runner` runs an argv
  slice — the `-A`/`-D` commands and the notification — and is implemented by
  `platform.Run`: `type Runner func(ctx context.Context, argv []string) error`.
  A screen write, a device send and a device presence check are one func each,
  implemented by `screen` and `hidpp`. A fake records calls and returns canned
  results. `screen` has the `Runner` seam with the child's output added
  (`screen.Runner`, implemented by `platform.Output`), which the Linux backend
  drives `ddcutil` through.
- **The server client.** Tests point the real client at `httptest.NewServer`
  wrapping the real handler, which exercises the actual protocol. A mocked
  client would test the mock.
- **State persistence.** One implementation, and `t.TempDir()` makes the real
  one testable — including the corruption case, by writing garbage bytes.

### 11.3 The agent is a pure state machine

Every rule in §4.3 is a decision about whether to open a one-way door, and each
of them is time-dependent: settle, retry spacing, the arrival window, cooldown,
circuit breaker, sleep detection. Testing that through goroutines and real
clocks gives slow, flaky tests for exactly the logic that must not be wrong.

So the decisions live in a function with no I/O, no goroutines, and no clock.
The agent observes, the decider decides, the agent carries the decision out:
each `Decision` field is an imperative, and each kind of work it starts comes
back as the `Observation` field of the same name in the past tense.

```go
type Observation struct {    // at most one field set: none when only time passed
    Attach   bool
    State    *state.ServerState // from /state or a /wait wake
    Ran      *CommandsRun          // the commands in flight finished
    GaveAway *AssignmentsGivenAway // the assignments in flight were given away
}

type CommandsRun struct {
    Results []error // one per command, in the order they ran: nil when it exited 0
}

type AssignmentsGivenAway struct {
    Results map[state.AssignmentKey]error // one per assignment: nil moved, ErrNotHere nothing to move
}

type Decision struct {
    Claim    string             // non-empty: POST /claim/<id>
    Run      []Command          // run these commands, in order
    GiveAway []state.Assignment // give away what these assignments name: move each screen or device to its assignee
    Notify   bool               // run --notify-cmd
    Save     *state.AgentState  // non-nil: persist this as agent.json
    Log      string             // non-empty: the decision loop logs it at Info
}

type Command struct {        // exactly one field set
    Argv       []string // exec this argv
    TakeScreen int64    // non-zero: post this host's screen declarations as takes, under this activation
    TakeHID    int64    // non-zero: post this host's device declarations as takes, under this activation
}

// Decide also returns when to call it again with nothing observed: the
// earliest deadline the decider tracks, zero when there is none.
func (m *Decider) Decide(now time.Time, o Observation) (ds []Decision, wake time.Time)
```

A device that was given away is not done at its result: the decider waits for a
`State` whose assignment says `arrived`, or for the 10 s window to close.

`agent/agent.go` is the supervisor: it waits for guards up, runs one generation,
and recycles on `errGuardsDown`. The decision loop reads the clock when it
processes each observation and passes it to `Decide`, rearms its timer for the
wake `Decide` returns, hands each `Decision` with work in it to a run-level
worker in `agent/worker.go`, and posts results back as ordinary observations.
The worker runs commands, gives assignments away and notifies one decision at a
time and posts each result to the loop. It keeps no progress of its own: a retry
is a decision that names only the commands or assignments that failed. The
arrival watcher (§5.4) is part of the agent as well: it polls and posts, and
decides nothing. Channels are scoped by freshness — `attachCh` and `stateCh` are
created per generation, while `results` and the worker are run-level. It
contains no policy.

### 11.4 Tests

- **`Decide` table tests are the bulk of the suite.** Every row of §8 is a case,
  and so is every combination of the §4.3 gates. Cheap enough to be exhaustive:
  no clock, no processes, no sockets.
- **Server tests run the real handler** under `httptest`, including two
  concurrent long-polls woken by one claim, an epoch that moves backwards, a bad
  token, an unknown host id, a take from a host that is not the owner and one
  under a stale activation.
- The HID detector consumes library events on both OSes; it has no parser to
  unit-test, so its event-driven path is covered only by the §10 checklist.
- `go test -race ./...`; the long-poll broadcast channel is the one place a race
  is plausible.
- **Not tested: the writes themselves.** They need the monitor and the
  peripherals, and they are §10.

## 12. Build & packaging

- Module: `github.com/<user>/soft-kvm`; layout in §11.
- Deps: stdlib, `golang.org/x/sync`, `github.com/grandcat/zeroconf` (which pulls
  `miekg/dns` and `golang.org/x/net`), `github.com/telesma-app/hid` (which pulls
  `ebitengine/purego`, `golang.org/x/sys` and `golang.org/x/text`), and
  `github.com/ebitengine/purego` directly on macOS for `CoreFoundation` and
  `IOKit` DDC/CI in `screen`. All pure Go — purego does FFI through `dlopen`,
  not cgo — so `CGO_ENABLED=0`, `-trimpath`, `-ldflags="-s -w"` still hold.
  Discovery costs three modules where the rest of the binary costs two — that is
  the price of not editing an IP address in three places.
- Targets: `linux/amd64` (desktop), `darwin/arm64` (laptop), and `linux/arm64`
  or `linux/arm` for a Pi — check `uname -m`, since `armv7l` means `linux/arm`
  `GOARM=7` even under a 64-bit kernel.
- The desktop build is a Nix derivation in the existing flake, not a copied
  binary; the Mac and any Pi get copied binaries.
- No config files besides the token env; every behavior knob is a CLI flag with
  a per-OS default.

## 13. Planned: keyboard follows display, on the return path

An OSD press is always available and sometimes necessary — deliberately, or to
finish a switch whose write did not land (§4.3). Today the keyboard stays where
it was and the user makes a second gesture. That press is detectable on Linux,
so if the USB switch can be actuated electrically, the keyboard follows it.

**Detector: the rising edge of `ddcutil getvcp 60`.** The read `detect` names
this host's port with (§5.6) answers only while this host's input is active.
Polled every 10 s, a fail→success transition is the user handing Linux the
display. It works with Deep Sleep off, which DRM hotplug does not (finding 11):
with the link held up on the inactive input, `card0-DP-3` never goes
disconnected, so the kernel sees nothing. DDC is the only witness.

Act on the edge only when `owner != me`. A rising edge while `owner == me` is
the monitor coming out of standby, which happens far more often than an OSD
press. Debounce 5 s, and suppress for 15 s after this host gives a screen away.

**Actuator: `--flip-cmd`, run locally on Linux.** An optocoupler or reed relay
across the USB switch's momentary button, driven from a USB relay board on the
desktop — the desktop's own USB is never switched away, so it can always reach
it. No new endpoint, no GPIO on the Pi, no protocol change: the agent runs
`--flip-cmd` on the rising edge, then claims itself.

The button toggles, it does not select. Pulse once, wait for the receiver attach
(the four-combo test measures it, expect < 2 s), and pulse again if it did not
arrive within 3 s. Two failed pulses: stop and log, rather than machine-gun the
switch.

**The falling edge is not usable.** Losing DDC means "someone took the display
away" *or* "the monitor went into standby", and nothing local separates them — a
DPMS blank would throw the keyboard at the Mac while the user is sitting in
front of Linux. So the Linux→Mac gesture stays physical: the switch's own
button, with §4.1 following it.

Not started. Needs: a USB switch whose button is reachable with a soldering
iron, and a USB relay board. Neither is on hand.

## References

[^1^]: Solaar documentation — Rule Processing of HID++ Notifications:
    https://pwr-solaar.github.io/Solaar/rules/

[^2^]: TESmart support — KVM emulation & pass-through FAQ:
    https://support.tesmart.com/

[^3^]: Home Assistant — Authentication & WebSocket API (long-lived access
    tokens): https://developers.home-assistant.io/docs/api/websocket and
    https://www.home-assistant.io/docs/authentication/

[^4^]: BetterDisplay issue #5350 / discussion #5353 — LG UltraGear 45GX950A
    ignores VCP 0x60 writes via every BetterDisplay protocol:
    https://github.com/waydabber/BetterDisplay/discussions/5353

[^5^]: BetterDisplay wiki — Integration features, CLI:
    https://github.com/waydabber/BetterDisplay/wiki/Integration-features,-CLI
    (confirmed syntax:
    `"/Applications/BetterDisplay.app/Contents/MacOS/BetterDisplay" set -ddc -vcp=inputSelect -value=<code>`;
    common codes: DP=15, HDMI=17)

[^6^]: m1ddc — DDC control on Apple Silicon via IOAVService, single binary:
    https://github.com/waydabber/m1ddc
