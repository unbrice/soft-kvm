# Display-Follows-Keyboard — System Spec

**Status:** design settled, ready to implement **Date:** 2026-08-14

Both directions are automatic. Switching the LG away has been done by hand from
Linux and from macOS, so the one mechanism the whole design rests on is known to
work; §10 is the integration pass, run at the end.

---

## 1. Context

| Item                  | Facts                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Hosts                 | Linux desktop `1b-nix0` (NixOS, Hyprland/Wayland, Intel Arc A770 on `xe`, monitor on `card0-DP-3`, always-on, home LAN); macOS **corp laptop** (locked down, different trust domain, frequently away)                                                                                                                                                                                                                                                                                                                                  |
| Display               | One 4K ultrawide LG, shared. Built-in KVM unusable (USB hub bound to a single upstream port)                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| DDC/CI constraint     | Monitor answers DDC **only on the currently active video input** (observed in practice)                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| Input-switch commands | Linux: `ddcutil setvcp 0xF4 0xD0 --i2c-source-addr=0x50 --noverify` · macOS: BetterDisplay (`"/Applications/BetterDisplay.app/Contents/MacOS/BetterDisplay" set -ddc -vcp=inputSelect -value=<code>`) — both are defaults baked into the binary, overridable via CLI args                                                                                                                                                                                                                                                              |
| Peripherals           | Logitech multi-device keyboard + mouse on a **Bolt** receiver, `046d:c548`. A Unifying receiver `046d:c52b` is also plugged into the desktop — the detector filter must not match it. Keyboard and mouse stay on Bolt channel 1 permanently; the Easy-Switch keys retire                                                                                                                                                                                                                                                               |
| Trigger hardware      | Cheap USB 3.0 sharing switch, UGREEN / ATEN class. Not bought                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| Coordination host     | Whichever host runs `soft-kvm serve`, found over mDNS — no host holds another's address (§5.1). The desktop by default; a Raspberry Pi on the LAN (the HA Pi; HA itself is **not** used) if a neutral one is wanted                                                                                                                                                                                                                                                                                                                    |
| Policy                | Corp device: outbound-only networking, minimal/no installs, must hold **no powerful credentials**                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| Implementation        | One Go binary, `soft-kvm`, five subcommands (`serve` / `activate` / `connect` / `detect` / `hid-switch` — `detect` enumerates HID devices to help write `--trigger`, `hid-switch` is the standalone form of the built-in switch command). Same artifact everywhere; the host carrying the bit runs `serve` alongside its `connect`. `CGO_ENABLED=0`, Go ≥ 1.25, stdlib + `golang.org/x/sync` (errgroup supervision, §11.1) + `grandcat/zeroconf` (mDNS discovery, §5.1) + `telesma-app/hid` (USB attach events on both OSes, §6.1-6.2) |

## 2. Goals / non-goals

**Goals**

- Display input follows the keyboard in both directions, one gesture, no human
  step.
- Nothing happens when the laptop is away, on battery, undocked, or off the home
  network.
- Manual monitor OSD switching always remains possible.
- Exactly one deliverable: a static Go binary per host (`linux/amd64`,
  `darwin/arm64`, plus `linux/arm64` if a Pi carries the bit). No scripts, no
  curl plumbing, no runtime deps on the corp laptop beyond stock tools and
  BetterDisplay.

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
   from Linux, BetterDisplay from macOS. Not every LG does: at least one recent
   UltraGear silently ignores VCP `0x60` writes from macOS through every
   protocol BetterDisplay exposes,[^4^] with `m1ddc` as the fallback there.[^6^]
   This one does not need it.
10. **A DDC write that returns success may still not take effect.** The bus is
    unacknowledged in practice: the monitor can NAK, arrive busy, or be waking
    from standby, and `--noverify` means nothing reads the result back. Working
    by hand is not the same as working every time, so the switch is confirmed
    locally and retried, and the human is told when it does not land (§4.3).
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
3. Server: owner changed → `epoch++` → wake all `/wait` long-polls.
4. Linux agent wakes, reconciles via `GET /state`. It switches because **it was
   the owner and no longer is** (`last_owner == linux`, `owner == mac`), the
   winner is live, and its DDC veto passes.
5. Linux runs its switch commands; the monitor moves to the Mac's input.
6. The Mac executes nothing display-related — by design, it can't (finding 1).

### 4.2 Data flow (switch Mac → Linux)

Symmetric: the receiver attaches on Linux, Linux claims, the Mac is the loser
and runs BetterDisplay, the monitor moves to Linux's input.

The two hosts do not send the same thing. Linux writes the LG-specific VCP
`0xF4` with source address `0x50`; macOS writes the standard `0x60` through
BetterDisplay. Different features, same effect on this monitor — so neither
command tells you anything about the other, and both are verified separately.

### 4.3 What authorises a switch

A switch is a one-way door: after running it, the host that ran it can no longer
reach the monitor. Three conditions gate it, all required.

- **Ownership transition.** The agent persists `last_owner` locally
  (`$XDG_STATE_HOME/soft-kvm/agent.json`,
  `~/Library/Application
  Support/soft-kvm/agent.json` on macOS) and acts only
  on `last_owner == me && owner != me`. An agent that starts with no local
  record adopts the server's current owner and **never switches on that first
  reconcile** — it waits for the next transition. Without this, a desktop that
  boots while the bit says `mac` hands the display to a laptop that is not
  there.
- **Winner liveness.** The server counts open `/wait` connections per host and
  reports them in `/state` as `live`. The loser refuses to act when the winner
  has no live agent. `soft-kvm activate` against a host with no live agent
  requires `--force`.
- **DDC veto.** When `--check-cmd` is set, it must succeed. Non-zero exit means
  the input is already inactive: skip, and resynchronise `last_owner`. When
  `--check-cmd` is empty (the macOS default), the veto is skipped and the switch
  operates in fire-and-forget mode.

**Circuit breaker:** 5 s cooldown after any switch, and 3 switches within 30 s
disables switching for 60 s with a log line. A misbehaving detector costs one
manual OSD press, not a loop.

**Confirming it landed, and what happens when it does not.** When `--check-cmd`
is set, the write is fire-and-forget (finding 10), so the loser confirms
locally, using the probe it already has: a switch that worked makes
`--check-cmd` start *failing*, because this host's input is no longer active.
The falling edge is the receipt. When `--check-cmd` is empty, exit 0 from the
switch commands confirms the switch immediately; a non-zero exit retries up to
`--switch-retries` (1 s apart) and then notifies. The switch command is bounded
twice: glue-side by `--switch-timeout` (default 30 s, SIGTERM then SIGKILL after
`WaitDelay`), and machine-side by a 60 s watchdog. Either failure feeds the same
retry/notify path and counts toward the circuit breaker like any other failure.
Results are accepted only while their effect is outstanding — a late
`SwitchExit` or `ProbeExit` is ignored and logged — and a confirm window with a
probe still in flight waits for it instead of closing on absent evidence.

1. Run the switch commands, in order.
2. Poll `--check-cmd` every 500 ms for `--confirm 4s`. It goes non-zero ⇒ done.
3. Still succeeding, or the command died or never reported ⇒ the monitor did not
   move. Retry the switch, up to `--switch-retries 3`, 1 s apart.
4. Still succeeding after the last retry ⇒ run `--notify-cmd` and stop. The user
   presses the OSD Input button, which is one press and always available (§2),
   and §13 makes the keyboard follow that press.

The notification fires on the losing host, which is the one still on screen —
the only host the user can read at that moment. Nothing re-claims and nothing
reverts: the shared bit is already right, the keyboard is already where the user
put it, and only the pixels are behind.

This is the loser's own probe rather than a winner-side check, because the
winner's DDC reads are the least trustworthy thing available — on macOS
BetterDisplay may not read the value back at all, and on a display that just
became active the read races the link coming up.

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

The bit and nothing else. `IP` optional — omitted binds all interfaces
(`soft-kvm serve 8700` ≡ `:8700`).

| Flag / env        | Default   | Meaning                                                        |
| ----------------- | --------- | -------------------------------------------------------------- |
| `--state PATH`    | see below | Persisted owner/epoch/since                                    |
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

Forces `ID` active — one-shot `POST /claim/ID`, exits non-zero on failure.
Server resolution as §5.1, so it works from any LAN host with only the token.
`--force` is required when `ID` has no live agent, because the resulting switch
points the monitor at a host that cannot switch it back. Use cases: recovery
from a mis-flip, testing, "just switch now".

### 5.4 `soft-kvm connect [flags] [-- CMD ARGS... [-- CMD ARGS...]]`

The host agent: detector, claimer, watcher, and the switch commands, in one
long-running process. Everything has a per-OS default; with discovery there are
no required arguments.

| Flag / arg              | Default (Linux)                                              | Default (macOS)                                                                                                      | Meaning                                                                                                                                                                                                                                                                                        |
| ----------------------- | ------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--id ID`               | `linux`                                                      | `mac`                                                                                                                | Claimed identity (from `GOOS`; override for testing)                                                                                                                                                                                                                                           |
| `-- SWITCH-CMD ARGS...` | `ddcutil setvcp 0xF4 0xD0 --i2c-source-addr=0x50 --noverify` | `"/Applications/BetterDisplay.app/Contents/MacOS/BetterDisplay" set -ddc -vcp=inputSelect -value=<linux-input-code>` | Command that points the monitor at the **other** host; run by the *losing* agent. Repeat after a bare `--` for each additional command (e.g. display, then a USB device) — they run in order, the display probe stays the receipt. A command named `hid-switch` is built in, not exec'd (§5.5) |
| `--check-cmd CMD`       | `ddcutil getvcp 60`                                          | —                                                                                                                    | Veto before the switch, receipt after it (§4.3); empty on macOS (fire-and-forget switch)                                                                                                                                                                                                       |
| `--check-timeout 10s`   | 10s                                                          | 10s                                                                                                                  | Bound on one `--check-cmd` run — a hung I²C read must not stall the confirm loop (§4.3)                                                                                                                                                                                                        |
| `--switch-timeout 30s`  | 30s                                                          | 30s                                                                                                                  | Bound on one `SWITCH-CMD` run — a hung I²C write must not freeze the agent (§4.3)                                                                                                                                                                                                              |
| `--trigger LIST`        | required                                                     | required                                                                                                             | Comma-separated VID:PID filters for the trigger detector — the USB receiver, plus optionally a Bluetooth keyboard (§6.3); `soft-kvm detect` lists candidates                                                                                                                                   |
| `--settle 2s`           | 2s                                                           | 2s                                                                                                                   | Attach must persist this long before claiming                                                                                                                                                                                                                                                  |
| `--confirm 4s`          | 4s                                                           | 4s                                                                                                                   | How long `--check-cmd` may keep succeeding before the switch counts as failed                                                                                                                                                                                                                  |
| `--switch-retries 3`    | 3                                                            | 3                                                                                                                    | Re-runs of the switch commands, 1 s apart, before giving up                                                                                                                                                                                                                                    |
| `--notify-cmd CMD`      | `notify-send 'soft-kvm' 'Press Input on the monitor'`        | `osascript -e 'display notification "Press Input on the monitor" with title "soft-kvm"'`                             | Run when the switch cannot be confirmed after the last retry                                                                                                                                                                                                                                   |
| `--no-guards`           | implicit                                                     | —                                                                                                                    | macOS guards: AC power + dock present (§6.2)                                                                                                                                                                                                                                                   |
| `SOFTKVM_TOKEN`         | required                                                     | required                                                                                                             | Shared secret                                                                                                                                                                                                                                                                                  |

`-productNameLike=<name>` (or the monitor's actual name substring) can be added
to `-- SWITCH-CMD` if multiple external displays are attached. The two switch
commands take codes from different namespaces. BetterDisplay writes the standard
VCP `0x60` and uses its values — DP=15, DP2=16, HDMI=17, HDMI2=18, USB-C/TB≈25 —
but the monitor's real per-port codes are whatever §10 records, and vendors
deviate.[^5^] `ddcutil` writes the LG-specific VCP `0xF4`, whose codes come from
this monitor's firmware: DisplayPort `0xD0` (fallback `0x06`), TB/USB-C `0xD1`,
HDMI 1 `0x90`, HDMI 2 `0x91`. `--noverify` is load-bearing, not a shortcut:
read-back verification fails on this firmware, so a verifying write reports
failure after succeeding.

**Behavior:**

1. On start: read `last_owner`, `GET /state`, reconcile per §4.3. First run
   after a state-file loss never switches.
2. Detector sees receiver attach → `--settle` stable → guards pass →
   `POST /claim/<id>` (3 retries, each attempt bounded to 5 s, exponential
   backoff with full jitter to 30 s; failure logged, never fatal).
3. Watcher: `GET /wait?epoch=N&id=<me>` (client timeout 60 s) → on wake,
   reconcile as in (1). A connection error is a reconcile trigger, not a fatal.
4. Guards re-evaluated every cycle; off-guard ⇒ fully dormant, zero traffic,
   re-checking the guards every 15 s (macOS).
5. **Sleep detection:** after each `/wait` long-poll, compare wall-clock
   progress against monotonic elapsed time. A wall-clock jump greater than twice
   the client's internal `/wait` timeout means the machine slept through the
   poll: reconcile immediately rather than waiting out the dead socket, which
   can hang for minutes after a lid-open.

### 5.5 The `hid-switch` virtual command

A switch command whose first word is `hid-switch` is never exec'd: it runs
in-process and speaks Logitech HID++ 2.0 `changeHost` (`0x1814`) directly. The
peripheral itself is told to move to another host — so in a setup without the
USB switch, an Easy-Switch key (keyboard) or host button (mouse) press remains
the gesture (the moved device's attach is the §6.1 trigger) and the losing
host's action makes the other peripheral follow.

```
hid-switch VID:PID [DEVICE_INDEX|keyboard|mouse] HOST_INDEX
```

- `HOST_INDEX` is the target's Easy-Switch slot minus one (0-2).
- Two arguments address the directly attached device (Bluetooth pairing or its
  own dongle) — device index `0xFF`.
- Behind a Bolt/Unifying receiver the host only sees the receiver, so give the
  receiver's VID:PID and either the device's pairing slot (1-6) or a kind. The
  kind form moves **every** paired device of that kind that supports changeHost.
  Slots persist in the receiver's flash until re-pairing, so a hardcoded slot is
  stable — but re-pairing can renumber, and the command then silently addresses
  the wrong slot. The §4.3 retry/notify path absorbs it, and `detect` prints the
  current slot map.

Mechanics: the command probes the device's HID interfaces for one that answers
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
their own flash, so even an error reply proves Logitech framing. Only the 7-byte
short report (`0x10`) is sent: every HID++ device carries it, our requests never
exceed its three parameter bytes, and Bolt receivers STALL the 20-byte
`SET_REPORT` on their control pipe. Replies may still arrive long. Kinds come
from `getDeviceType` (feature `0x0005`, function 2) — one byte, no name reads. A
receiver's slot map is its local pairing table (HID++ 1.0 register `0x2B5`,
sub-registers `0x20+slot-1` on Unifying, `0x50+slot` on Bolt): on-chip flash, no
RF, so it is right whether a paired device is awake, asleep, off, or switched to
the other host. A kind target resolves to the directly attached device when it
matches — a Bluetooth device answers pairing-slot queries as itself, once per
slot — else to every table slot of the kind, each confirmed with a (nudged)
changeHost query over the air. **No reply to `setHost` counts as success**: a
device that switched drops the link to this host mid-ACK, which over Bluetooth
surfaces as a read error (EIO on Linux hidraw), not a timeout; an explicit HID++
error reply is the failure. On Linux the agent needs write access to the hidraw
node (§6.1).

Typical pair — keyboard and mouse on receiver channel 1 and BT channel 2:

- macOS, mouse over BT: `-- hid-switch 046d:b034 0`
- Linux, through the Bolt receiver: `-- hid-switch 046d:c548 mouse 1`

## 6. Host components

### 6.1 Linux host

| Component          | Spec                                                                                                                                                                                                    |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Detector (primary) | HID device add/remove events via `telesma-app/hid` (netlink kernel uevents on `hidraw`, VID:PID from sysfs) — the same library and code path as macOS (§6.2). No udev rule, no forked processes, no cgo |
| Guards             | None (always-on desktop)                                                                                                                                                                                |
| Switch command     | Baked-in `ddcutil` default; user in `i2c` group — no sudo                                                                                                                                               |
| Packaging          | NixOS: `systemd.user.services.soft-kvm` in the flake, `Restart=always`, `RestartSec=5`                                                                                                                  |

**Detector specifics** — the parts that are easy to get wrong:

- **One physical attach fires one event per HID interface** — the receiver
  exposes keyboard, mouse and raw interfaces, each reported as a separate device
  (hidraw node on Linux, IOHIDDevice on macOS) with the same VID:PID. The
  detector deduplicates by device path and emits a single attach edge
  (`detect_hid.go`); `--settle` covers the rest.
- **Unprivileged operation.** On Linux the library binds netlink multicast group
  1 (raw kernel uevents) — readable without root on current kernels, so a
  systemd *user* unit suffices. VID:PID metadata comes from
  `/sys/class/hidraw/*/device/uevent`, world-readable: no udev rule.
- The Unifying receiver `046d:c52b` is on the same bus. Filter exactly.
- **`hid-switch` needs hidraw write access** (§5.5). The detector only reads
  uevents, but the virtual command opens the receiver's hidraw node `O_RDWR`:
  add a udev rule (`TAG+="uaccess"` or a group), as Solaar's packaging does.

**NixOS prerequisites** (none of which are in place today, finding 12):

```nix
hardware.i2c.enable = true;              # i2c-dev + i2c group + udev rules
users.users.brice.extraGroups = [ "i2c" ];
users.users.brice.linger = true;         # user units die at logout otherwise
environment.systemPackages = [ pkgs.ddcutil ];
```

`--i2c-source-addr` is a recent ddcutil flag; check `ddcutil --version` before
trusting the baked-in default. On a DisplayPort link the DDC channel rides DP
AUX, so the `/dev/i2c-N` number is created by the `xe` driver and **changes on
monitor hotplug and across reboots**. Do not pin `--bus`; pin `--model` or
`--sn`, or let ddcutil scan and accept the ~1–3 s cost. If reads are flaky, try
`--sleep-multiplier 2`.

### 6.2 macOS host (corp laptop)

| Component          | Spec                                                                                                                                                                                                                                                                                            |
| ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Guards             | `pmset -g ps` contains `AC Power`, and the LG is present                                                                                                                                                                                                                                        |
| Detector (primary) | `IOHIDManager` device-matching events filtered to VID:PID, via `telesma-app/hid` (purego — dynamic FFI, no cgo, static binary preserved); the same library and code path as Linux (§6.1). No `IOHIDManagerOpen`: enumeration callbacks need no device access, so no Input-Monitoring TCC prompt |
| Switch command     | Baked-in BetterDisplay default (§5). BetterDisplay must be running. **Verify against findings 9 and 10 first**                                                                                                                                                                                  |
| Packaging          | `~/Library/LaunchAgents/local.soft-kvm.plist` (`RunAtLoad`, `KeepAlive`), token in a `0600` env file, log to `~/Library/Logs/`                                                                                                                                                                  |
| Installs           | One binary + one plist. `pmset` is stock; BetterDisplay is at `/Applications/BetterDisplay.app/Contents/MacOS/BetterDisplay`                                                                                                                                                                    |

- **`hid-switch` needs Input Monitoring** (§5.5). Enumeration and the attach
  events need no TCC grant, but the virtual command — like `detect`'s scan —
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
  GPU path. Do not call it on a 2 s cadence. Use
  `"/Applications/BetterDisplay.app/Contents/MacOS/BetterDisplay" get -identifiers`
  — it is already a dependency and returns in milliseconds — and keep
  `system_profiler` for `activate`-time diagnostics.
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
detector sees it — add the keyboard's own VID:PID to `--trigger`. Only
**connect** events are used (finding 2); disconnects are ignored. Two caveats:

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
  `EnvironmentFile=`, mode `0600`.
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
derived from it (§9). The secret is scoped by construction: it can read/flip one
display bit and nothing else.

| Endpoint                    | Behavior                                                                                                                         |
| --------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `POST /claim/{id}`          | Idempotent. Increments `epoch` and wakes waiters **only on actual change**. → `200 {owner, epoch, changed}` · `400` unknown host |
| `GET /state`                | → `200 {owner, epoch, since, live, server_id}`                                                                                   |
| `GET /wait?epoch=N&id=<me>` | Long-poll. Returns `200 {owner, epoch}` immediately when `epoch ≠ N`; `204` after 50 s. Registers `id` as live for the duration  |

- `since` is RFC 3339. `live` is `{"linux": true, "mac": false}`, derived from
  currently-open `/wait` connections. `server_id` is a UUID regenerated on every
  process start, so an agent can tell a restart from a state change.
- **Clients treat `epoch` as opaque.** Adopt whatever the server returns; never
  compare numerically. A restored-from-backup or reset `state.json` moves the
  epoch backwards, and a client that only waits for `epoch > N` spins at line
  rate.
- LAN bind, HTTPS only. TLS 1.3, identity derived from the shared secret (§9).

## 8. Edge cases

| Case                                                              | Behavior                                                                                                                                                                                                                                                                                                                                                   |
| ----------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Claimant executes no display command                              | By design — DDC unreachable from inactive input                                                                                                                                                                                                                                                                                                            |
| Simultaneous claims                                               | Serialized; last wins; both agents reconcile                                                                                                                                                                                                                                                                                                               |
| Loser asleep/off at claim                                         | Nobody switches; loser self-heals at resume, but only if it was the owner (§4.3)                                                                                                                                                                                                                                                                           |
| Agent starts with no local `last_owner`                           | Adopts the server's owner, switches nothing                                                                                                                                                                                                                                                                                                                |
| Coordinator unreachable                                           | Claims logged-and-lost; agents back off exponentially (full jitter, 1 s base, 30 s cap — jitter keeps the two agents from retrying in lockstep after a shared outage); OSD fallback                                                                                                                                                                        |
| Mac away / on battery / undocked                                  | Guards suppress everything; bit may change without the Mac — harmless, because the loser also requires the winner to be live                                                                                                                                                                                                                               |
| Receiver flapping                                                 | 2 s settle + change-only notification + idempotent claims + circuit breaker                                                                                                                                                                                                                                                                                |
| Monitor already on claimant's input                               | Loser's `--check-cmd` fails → no-op, `last_owner` resynced                                                                                                                                                                                                                                                                                                 |
| Token mismatch after rotation                                     | Connections fail TLS verification (the certificate derives from the token); nothing switches until the env files and the running units agree again (§9)                                                                                                                                                                                                    |
| Server restarted                                                  | Long-polls reset; agents reconcile on error; `server_id` change is not by itself a state change                                                                                                                                                                                                                                                            |
| **Mis-flip: monitor points at a host that cannot switch it back** | Unrecoverable over the network. `soft-kvm activate` does *not* help: the newly-designated loser is the absent host, and it is the one that would have to run DDC. With Auto Input Switch off, the OSD button is the only recovery — and unlike the failed-write case, no agent is left able to notify. The §4.3 gates exist to make this state unreachable |
| Switch command exits 0 but the monitor does not move              | `--check-cmd` keeps succeeding past `--confirm`; retry up to `--switch-retries`, then notify and stop. The bit stays correct and the user presses OSD (§4.3)                                                                                                                                                                                               |
| Switch command hangs / `SwitchExit` is lost                       | `--switch-timeout` SIGTERMs the child; if the result still never arrives, the 60 s machine watchdog counts the attempt as failed, retries, then notifies and logs "no SwitchExit within …" (§4.3)                                                                                                                                                          |
| Effect result arrives after its sequence ended                    | Ignored and logged as "ignoring late SwitchExit/ProbeExit"; the machine only accepts results while the matching effect is outstanding (§4.3)                                                                                                                                                                                                               |
| Switch fails while the monitor is in standby                      | Same path, and the retries usually cover it — a monitor waking from standby answers DDC a second or two late                                                                                                                                                                                                                                               |
| Deep Sleep left on                                                | Every switch hot-unplugs the monitor on the losing host: Hyprland collapses workspaces onto nothing, macOS rearranges windows. Set it Off                                                                                                                                                                                                                  |
| mDNS browse returns nothing                                       | Cached address is tried first and usually still valid; otherwise back off to 60 s and keep browsing. No claims are lost that a `SOFTKVM_SERVER` override would not also have lost                                                                                                                                                                          |
| mDNS returns a stale or rogue record                              | Connection fails TLS verification — a rogue record points at a host that does not hold the secret and cannot present the derived certificate (§5.1, §9). Agents re-browse on any connection error rather than pinning the first answer                                                                                                                     |
| Server moves host                                                 | Nothing is reconfigured; the new instance advertises, caches expire on first failure                                                                                                                                                                                                                                                                       |

## 9. Security notes

- **TLS identity is derived from the shared secret.** Every instance runs
  `HKDF-SHA256(SOFTKVM_TOKEN, info="soft-kvm tls v1")` into an Ed25519 seed. The
  same key and seed self-sign a fixed-template CA certificate (serial 1) and
  sign a fixed-template client certificate (serial 2). Both templates are
  constant and Ed25519 signing is deterministic, so all holders of the secret
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
- Token blast radius: read/flip one display bit. Rotating = editing the env
  files and restarting the server and both agents — the token is read at process
  start, and handshakes fail until they agree (§8). The token never appears in
  `ps` or shell history.
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
- The agent executes the switch commands as given. Each is an argv slice, never
  a shell string — no `sh -c`, no interpolation of anything received from the
  server. The server can flip a bit; it can never choose what runs.

## 10. Integration tests

Run at the end, against the assembled system. Both switch commands already work
by hand (finding 9), so nothing here blocks writing the code — these check the
seams, and every one of them needs hardware that is not on the desk yet or a
binary that does not exist yet.

- [ ] Four-combo test with the USB switch (`lsusb` /
      `ioreg -r -c IOUSBHostDevice`: switch-to-me / away × receiver present /
      absent) — clean attach/detach, < 2 s, on **both** hosts. If this fails
      there is no detector, and no detector means no trigger
- [ ] Per-port VCP codes recorded for every input in use, from both hosts
- [ ] Switch fails on purpose (unplug the LG's other input, or point the command
      at a code no port uses): `--check-cmd` keeps succeeding, retries run,
      notification appears on the losing host, no reclaim loop
- [ ] `ddcutil getvcp 60` succeeds **iff** the Linux input is active — and
      confirm it still succeeds after the monitor sleeps and wakes, since the
      probe is both the veto and the receipt (§4.3)
- [ ] Fifty switches back and forth: count how many needed a retry. If retries
      are common rather than rare, `--confirm` and `--switch-retries` need
      different numbers than the guessed 4 s and 3
- [ ] Re-claim the current owner fifty times: the epoch never moves and no agent
      wakes (idempotent claims, §7)
- [ ] Claim-to-pixels latency on the end-to-end pass: ≤ 10 s — settle and
      long-poll wake dominate
- [ ] LG OSD: Deep Sleep Off on both ports; confirm the losing host keeps the
      display connected after a switch (no Hyprland workspace collapse)
- [ ] USB detector fires once per physical attach, not once per HID interface,
      and does not fire for the Unifying receiver `046d:c52b`
- [ ] `hid-switch` (§5.5) moves the mouse from each host: over Bluetooth from
      the Mac (two-argument form), through the Bolt receiver from Linux (kind
      form and an explicit slot). `detect`'s slot map matches `solaar show`
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

## 11. Implementation notes

**No CLI framework.** Five subcommands, one `flag.NewFlagSet` each, a switch on
`os.Args[1]`. Cobra costs two modules and actively hurts here: pflag parses
flags interspersed with positional arguments, so `-vcp=inputSelect` inside the
trailing switch command is read as an unknown flag unless interspersal is turned
off. Stdlib `flag` stops at the first non-flag argument and hands the rest to
`Args()` — which *is* the `-- SWITCH-CMD ARGS...` convention, for free. Bare
`--` tokens past the first survive into `Args()` and separate one switch command
from the next (§5.4). The same stop-at-first-positional rule would silently
ignore a flag placed after a positional in the subcommands that take no trailing
command, so `serve`, `activate` and `detect` reject any argument past their
positionals, with a hint when the extra looks like a flag.

**Package layout.** One module, one binary, ten packages under the root `main`:
`state` (the `/state` wire type and atomic JSON persistence), `model` (§11.3),
`identity` (TLS identity and `kh=` fingerprint from the shared secret),
`discover` (mDNS advertise/browse; the address cache sits behind a `Resolver`
whose cache path is injected), `platform` (the argv runner, the per-OS defaults,
and the per-OS `Guard` — a concrete type: no-op on Linux, pmset+display on
macOS), `detect` (HID enumeration for the subcommand and the attach detector
both OSes share), `hidpp` (Logitech HID++ changeHost — the `hid-switch` virtual
command, §5.5), `client`, `server`, and `agent` (supervisor, generations,
decision loop, claims; the `Detector`, `Guard` and `Runner` seams live here, in
their only consumer). `main` keeps flag parsing and wiring. Layering is a DAG:
the leaves (`state`, `identity`, `discover`, `platform`, `hidpp`) import nothing
internal; `detect` imports `hidpp`; `model` imports `state`; `client` and
`server` import `state` and `identity`; `agent` imports `model`, `state`,
`client`, `discover` and `hidpp`. Build tags in filenames, not in `//go:build`
lines, wherever the split is per-OS (`platform`).

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
run-level, cancelled only by process shutdown: the action worker (so a switch
that started guards-up finishes guards-up — a guard flap must not SIGTERM a
switch mid-I2C) and a `SetLimit(1)` group holding claims, so a redundant claim
trigger coalesces instead of overlapping an in-flight retry. The guard poll no
longer blocks the decision loop.

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

- **The display.** There is one implementation — run this argv slice — for the
  switch, the probe, the notification, and later the flip. The seam tests need
  is the runner, so it is a func type, not an interface:
  `type Runner func(ctx context.Context, argv []string) error` — defined in
  `agent`, implemented by `platform.Run`. A fake runner records calls and
  returns canned exit codes.
- **The server client.** Tests point the real client at `httptest.NewServer`
  wrapping the real handler, which exercises the actual protocol. A mocked
  client would test the mock.
- **State persistence.** One implementation, and `t.TempDir()` makes the real
  one testable — including the corruption case, by writing garbage bytes.

### 11.3 The agent is a pure state machine

Every rule in §4.3 is a decision about whether to open a one-way door, and each
of them is time-dependent: settle, confirm, retry spacing, cooldown, circuit
breaker, sleep detection. Testing that through goroutines and real clocks gives
slow, flaky tests for exactly the logic that must not be wrong.

So the decisions live in a function with no I/O, no goroutines, and no clock:

```go
type Event struct {          // exactly one field set, plus Now
    Now         time.Time
    Attach      bool
    State       *ServerState // from /state or a /wait wake
    SwitchExit  *error       // result of the switch command
    ProbeExit   *error       // result of --check-cmd
}

type Action struct {
    Claim     string        // non-empty: POST /claim/<id>
    Switch    bool          // run SWITCH-CMD
    Probe     bool          // run --check-cmd
    Notify    bool          // run --notify-cmd
    WakeAt    time.Time     // emitted at most once per Step batch, as its own final action: the glue rearms its timer from it
    SaveOwner *string       // non-nil: persist this as last_owner
    Log       string        // non-empty: glue logs at Info
}

func (m *Machine) Step(e Event) []Action
```

`agent/agent.go` is the supervisor: it waits for guards up, runs one generation,
and recycles on `errGuardsDown`. The decision loop stamps `Now` at processing
time for every event, dispatches the `Action`s to a run-level worker in
`agent/actions.go`, and posts results back as ordinary events. Effects are no
longer executed inline: one action worker runs Switch/Probe/Notify one at a time
and posts `SwitchExit`/`ProbeExit` to the loop. Channels are scoped by freshness
— `attachCh` and `stateCh` are created per generation, while `results` and the
action worker are run-level. It contains no policy.

### 11.4 Tests

- **`Step` table tests are the bulk of the suite.** Every row of §8 is a case,
  and so is every combination of the §4.3 gates. Cheap enough to be exhaustive:
  no clock, no processes, no sockets.
- **Server tests run the real handler** under `httptest`, including two
  concurrent long-polls woken by one claim, an epoch that moves backwards, a bad
  token, and an unknown host id.
- The HID detector consumes library events on both OSes; it has no parser to
  unit-test, so its event-driven path is covered only by the §10 checklist.
- `go test -race ./...`; the long-poll broadcast channel is the one place a race
  is plausible.
- **Not tested: the switch commands themselves.** They need the monitor, and
  they are §10.

## 12. Build & packaging

- Module: `github.com/<user>/soft-kvm`; layout in §11.
- Deps: stdlib, `golang.org/x/sync`, `github.com/grandcat/zeroconf` (which pulls
  `miekg/dns` and `golang.org/x/net`), and `github.com/telesma-app/hid` (which
  pulls `ebitengine/purego`, `golang.org/x/sys` and `golang.org/x/text`). All
  pure Go — purego does FFI through `dlopen`, not cgo — so `CGO_ENABLED=0`,
  `-trimpath`, `-ldflags="-s -w"` still hold. Discovery costs three modules
  where the rest of the binary costs two — that is the price of not editing an
  IP address in three places.
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

**Detector: the rising edge of `--check-cmd`.** The probe already distinguishes
"my input is active" from "my input is not". Polled every 10 s, a fail→success
transition is the user handing Linux the display. It works with Deep Sleep off,
which DRM hotplug does not (finding 11): with the link held up on the inactive
input, `card0-DP-3` never goes disconnected, so the kernel sees nothing. DDC is
the only witness.

Act on the edge only when `owner != me`. A rising edge while `owner == me` is
the monitor coming out of standby, which happens far more often than an OSD
press. Debounce 5 s, and suppress for 15 s after this host runs its own switch
command.

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
