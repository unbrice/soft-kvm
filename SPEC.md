# Display-Follows-Keyboard — System Spec

**Status:** design settled, ready to implement **Date:** 2026-08-14

Both directions are automatic. Switching the LG away has been done by hand from
Linux and from macOS, so the one mechanism the whole design rests on is known to
work; §10 is the integration pass, run at the end.

---

## 1. Context

| Item                  | Facts                                                                                                                                                                                                                                                                                |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Hosts                 | Linux desktop `1b-nix0` (NixOS, Hyprland/Wayland, Intel Arc A770 on `xe`, monitor on `card0-DP-3`, always-on, home LAN); macOS **corp laptop** (locked down, different trust domain, frequently away)                                                                                |
| Display               | One 4K ultrawide LG, shared. Built-in KVM unusable (USB hub bound to a single upstream port)                                                                                                                                                                                         |
| DDC/CI constraint     | Monitor answers DDC **only on the currently active video input** (observed in practice)                                                                                                                                                                                              |
| Input-switch commands | Linux: `ddcutil setvcp 0xF4 0xD0 --i2c-source-addr=0x50 --noverify` · macOS: BetterDisplay CLI (`betterdisplaycli set -feature=ddc -vcp=inputSelect -value=<code>`) — both are defaults baked into the binary, overridable via CLI args                                              |
| Peripherals           | Logitech multi-device keyboard + mouse on a **Bolt** receiver, `046d:c548`. A Unifying receiver `046d:c52b` is also plugged into the desktop — the detector filter must not match it. Keyboard and mouse stay on Bolt channel 1 permanently; the Easy-Switch keys retire             |
| Trigger hardware      | Cheap USB 3.0 sharing switch, UGREEN / ATEN class. Not bought                                                                                                                                                                                                                        |
| Coordination host     | Whichever host runs `soft-kvm serve`, found over mDNS — no host holds another's address (§5.1). The desktop by default; a Raspberry Pi on the LAN (the HA Pi; HA itself is **not** used) if a neutral one is wanted                                                                  |
| Policy                | Corp device: outbound-only networking, minimal/no installs, must hold **no powerful credentials**                                                                                                                                                                                    |
| Implementation        | One Go binary, `soft-kvm`, three subcommands (`serve` / `activate` / `connect`). Same artifact everywhere; the host carrying the bit runs `serve` alongside its `connect`. `CGO_ENABLED=0`, Go ≥ 1.22, stdlib + `golang.org/x/sys/unix` + `grandcat/zeroconf` (mDNS discovery, §5.1) |

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
5. Linux runs its switch command; the monitor moves to the Mac's input.
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
- **DDC veto.** `--check-cmd` must succeed. Non-zero exit means the input is
  already inactive: skip, and resynchronise `last_owner`.

**Circuit breaker:** 5 s cooldown after any switch, and 3 switches within 30 s
disables switching for 60 s with a log line. A misbehaving detector costs one
manual OSD press, not a loop.

**Confirming it landed, and what happens when it does not.** The write is
fire-and-forget (finding 10), so the loser confirms locally, using the probe it
already has: a switch that worked makes `--check-cmd` start *failing*, because
this host's input is no longer active. The falling edge is the receipt.

1. Run the switch command.
2. Poll `--check-cmd` every 500 ms for `--confirm 4s`. It goes non-zero ⇒ done.
3. Still succeeding ⇒ the monitor did not move. Retry the switch, up to
   `--switch-retries 3`, 1 s apart.
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
browse (3 s timeout, then retry with backoff to 60 s). The cache
(`$XDG_STATE_HOME/soft-kvm/server`) is what makes a boot survive a flaky browse,
and it is tried before mDNS because a working address beats a discovery round
trip.

**The TXT record carries the instance id and protocol version. Never the
token.** It is broadcast to every device on the LAN, guests included.

Sharp edges, in the order they will bite:

- **Multicast does not reliably cross WiFi.** Consumer APs drop or rate-limit
  multicast to save airtime, and client isolation kills it outright. The roaming
  laptop is both the host that needs discovery most and the one least likely to
  get it. `SOFTKVM_SERVER` on the Mac is the answer if the browse proves
  unreliable, which costs exactly the config this feature removes.
- **macOS 15 gates local network access per-binary.** Sequoia prompts on the
  first connection to a LAN address — multicast *and* unicast — and a denial is
  silent: connections simply never complete. A launchd agent may get no prompt
  at all, and MDM can deny it outright. This applies to the plain HTTP long-poll
  too, not only to mDNS, so it must be cleared before anything else on the Mac
  (§10).
- **Any LAN host can advertise the same service** and harvest the token from
  whichever agent browses first. Under the stated threat model this changes
  little — the token already crosses the LAN in cleartext over plain HTTP — but
  it lowers impersonation from "spoof ARP" to "publish a record". The fix, if
  the threat model tightens: `GET /hello?nonce=N` returning
  `HMAC-SHA256(token, N)`, checked before the agent sends the token. Not
  planned.
- `grandcat/zeroconf` is thinly maintained; `github.com/libp2p/zeroconf/v2` is
  the live fork with the same API. Switch if the browse loop misbehaves.

### 5.2 `soft-kvm serve [IP:]PORT`

The bit and nothing else. `IP` optional — omitted binds all interfaces
(`soft-kvm serve 8700` ≡ `:8700`).

| Flag / env        | Default                        | Meaning                                                      |
| ----------------- | ------------------------------ | ------------------------------------------------------------ |
| `--state PATH`    | `/var/lib/soft-kvm/state.json` | Persisted owner/epoch/since                                  |
| `--instance NAME` | hostname                       | mDNS instance name under `_soft-kvm._tcp`                    |
| `--no-advertise`  | off                            | Skip the mDNS registration; clients must be given an address |
| `SOFTKVM_TOKEN`   | required                       | Shared secret for all endpoints except `/health`             |

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

### 5.4 `soft-kvm connect [flags] [-- SWITCH-CMD ARGS...]`

The host agent: detector, claimer, watcher, and the switch command, in one
long-running process. Everything has a per-OS default; with discovery there are
no required arguments.

| Flag / arg              | Default (Linux)                                              | Default (macOS)                                                                                    | Meaning                                                                                 |
| ----------------------- | ------------------------------------------------------------ | -------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `--id ID`               | `linux`                                                      | `mac`                                                                                              | Claimed identity (from `GOOS`; override for testing)                                    |
| `-- SWITCH-CMD ARGS...` | `ddcutil setvcp 0xF4 0xD0 --i2c-source-addr=0x50 --noverify` | `betterdisplaycli set -productNameLike=LG -feature=ddc -vcp=inputSelect -value=<linux-input-code>` | Command that points the monitor at the **other** host; run by the *losing* agent        |
| `--check-cmd CMD`       | `ddcutil getvcp 60`                                          | `betterdisplaycli get -productNameLike=LG -feature=ddc -vcp=inputSelect`                           | Veto before the switch, receipt after it (§4.3)                                         |
| `--check-timeout 10s`   | 10s                                                          | 10s                                                                                                | Bound on one `--check-cmd` run — a hung I²C read must not stall the confirm loop (§4.3) |
| `--usb VID:PID`         | `046d:c548`                                                  | `046d:c548`                                                                                        | Bolt receiver filter for the primary detector                                           |
| `--settle 2s`           | 2s                                                           | 2s                                                                                                 | Attach must persist this long before claiming                                           |
| `--confirm 4s`          | 4s                                                           | 4s                                                                                                 | How long `--check-cmd` may keep succeeding before the switch counts as failed           |
| `--switch-retries 3`    | 3                                                            | 3                                                                                                  | Re-runs of the switch command, 1 s apart, before giving up                              |
| `--notify-cmd CMD`      | `notify-send 'soft-kvm' 'Press Input on the monitor'`        | `osascript -e 'display notification "Press Input on the monitor" with title "soft-kvm"'`           | Run when the switch cannot be confirmed after the last retry                            |
| `--bt-mac AA:BB…`       | off                                                          | n/a                                                                                                | Bluetooth fallback detector, Linux only (§6.3)                                          |
| `--no-guards`           | implicit                                                     | —                                                                                                  | macOS guards: AC power + dock present (§6.2)                                            |
| `SOFTKVM_TOKEN`         | required                                                     | required                                                                                           | Shared secret                                                                           |

`-productNameLike=LG` (or the LG's actual name substring) avoids hardcoding
display IDs. The two switch commands take codes from different namespaces.
BetterDisplay writes the standard VCP `0x60` and uses its values — DP=15,
DP2=16, HDMI=17, HDMI2=18, USB-C/TB≈25 — but the LG's real per-port codes are
whatever §10 records, and vendors deviate.[^5^] `ddcutil` writes the LG-specific
VCP `0xF4`, whose codes come from this monitor's firmware: DisplayPort `0xD0`
(fallback `0x06`), TB/USB-C `0xD1`, HDMI 1 `0x90`, HDMI 2 `0x91`. `--noverify`
is load-bearing, not a shortcut: read-back verification fails on this firmware,
so a verifying write reports failure after succeeding.

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
5. **Sleep detection:** compare `time.Now()` drift against a monotonic deadline
   each cycle. A wall-clock jump greater than 2× the poll interval means the
   machine slept: reconcile immediately rather than waiting out the dead
   long-poll socket, which can hang for minutes after a lid-open.

## 6. Host components

### 6.1 Linux host

| Component           | Spec                                                                                                                                                |
| ------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| Detector (primary)  | Netlink `NETLINK_KOBJECT_UEVENT` listener (`golang.org/x/sys/unix`), filtered to the receiver's VID:PID — no udev rule, no forked processes, no cgo |
| Detector (fallback) | Bluetooth via shell-out, §6.3                                                                                                                       |
| Guards              | None (always-on desktop)                                                                                                                            |
| Switch command      | Baked-in `ddcutil` default; user in `i2c` group — no sudo                                                                                           |
| Packaging           | NixOS: `systemd.user.services.soft-kvm` in the flake, `Restart=always`, `RestartSec=5`                                                              |

**Netlink specifics** — the parts that are easy to get wrong:

- **Group 2, not group 1.** Multicast group 1 carries raw kernel uevents and is
  conventionally root-only; group 2 carries the udev-processed copy and is
  readable unprivileged, which a systemd *user* unit needs. Group 2 messages are
  framed: 8-byte `libudev\0` magic, then a header whose fields give the
  properties offset, then the same `KEY=VALUE\0` payload. Parse the header,
  don't assume the message starts with `ACTION@DEVPATH`.
- **`PRODUCT=` is unpadded lowercase hex**, formatted `%x/%x/%x` from
  idVendor/idProduct/bcdDevice. The Bolt receiver appears as
  `PRODUCT=46d/c548/…` — matching the string `046d:c548` finds nothing. Match on
  `SUBSYSTEM=usb`, `DEVTYPE=usb_device`, `ACTION=add`, and the first two
  `PRODUCT` fields parsed as integers.
- **One physical attach fires many uevents** — `usb_device`, one `usb_interface`
  per HID interface, then `hid`, `input`, `hidraw`. Filtering on
  `DEVTYPE=usb_device` collapses that to one; `--settle` covers the rest.
- Set `SO_RCVBUF` (256 KiB) — a slow reader loses uevents silently.
- The Unifying receiver `046d:c52b` is on the same bus. Filter exactly.

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

| Component          | Spec                                                                                                                                                                                                                           |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Guards             | `pmset -g ps` contains `AC Power`, and the LG is present                                                                                                                                                                       |
| Detector (primary) | Poll `ioreg -r -c IOUSBHostDevice -l` every 2 s for `"idVendor" = 1133` and `"idProduct" = 50504` (ioreg prints decimal). The legacy `-p IOUSB` plane is empty on Apple Silicon. IOKit notifications would need cgo — rejected |
| Switch command     | Baked-in BetterDisplay default (§5). BetterDisplay must be running. **Verify against findings 9 and 10 first**                                                                                                                 |
| Packaging          | `~/Library/LaunchAgents/local.soft-kvm.plist` (`RunAtLoad`, `KeepAlive`), token in a `0600` env file, log to `~/Library/Logs/`                                                                                                 |
| Installs           | One binary + one plist. `pmset`, `system_profiler`, `ioreg` are stock; `betterdisplaycli` ships with existing BetterDisplay                                                                                                    |

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
  `betterdisplaycli get -identifiers` — it is already a dependency and returns
  in milliseconds — and keep `system_profiler` for `activate`-time diagnostics.
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

### 6.3 Bluetooth fallback detector — Linux only

Fallback for environments without the USB switch. Only **connect** events are
used (finding 2); disconnects are ignored.

Linux: `gdbus monitor --system --dest org.bluez`, parsing `Connected: <true>`
for `--bt-mac`. Zero deps, zero cgo; parsing is brittle but the signal is
simple. Behind a `Detector` interface (`events() <-chan struct{}`) shared with
the USB detectors, so a native `godbus/dbus` BlueZ subscription can replace it
without touching the agent loop.

macOS has no fallback. `system_profiler SPBluetoothDataType` is too slow to poll
and IOBluetooth needs cgo, which breaks the static single binary. **If the USB
switch fails the §10 four-combo test, the Mac side has no detector and the
design does not work.**

### 6.4 Carrying the bit

Any always-on host. The desktop is the shortest path — it is always on, and it
is where §13's relay board has to live. The Pi buys only neutrality, which is
worth less than it looks: a desktop that is down cannot be a party to any
switch, since its own DDC is dead, so a claim lost during its reboot costs
nothing.

- State: `/var/lib/soft-kvm/state.json` for a system unit, or
  `$XDG_STATE_HOME/soft-kvm/state.json` when the desktop serves from its user
  unit. Token via `EnvironmentFile=`, mode `0600`.
- Avahi will fight `grandcat/zeroconf` for UDP 5353 if both try to own it. The
  library sets `SO_REUSEADDR`/`SO_REUSEPORT` and coexists; where it does not,
  register through Avahi with a static `/etc/avahi/services/soft-kvm.service`
  and pass `--no-advertise`.
- `net/http` with Go ≥ 1.22 `ServeMux` patterns (`POST /claim/{id}`) — method
  and wildcard matching without a router dependency.
- `/wait` uses a **broadcast channel**: state holds a `chan struct{}` that is
  closed on every epoch change and replaced under the same lock. Waiters
  `select` on it, `time.After(50s)`, and `r.Context().Done()`. Not `sync.Cond`,
  which has no timed `Wait` and would need a second goroutine per waiter.
- `WriteTimeout` must exceed 50 s or be zero, otherwise the server kills its own
  long-polls. `ReadHeaderTimeout` 10 s.
- Token comparison via `crypto/subtle.ConstantTimeCompare`.
- **Atomic state writes:** temp file in the same directory, `fsync`, `rename`. A
  truncated `state.json` after a power cut must not crash-loop the service — on
  parse failure, log and start from `owner=""`, `epoch=0`. A fresh server with
  no state file starts the same way; the §4.3 first-reconcile rule makes the
  empty owner safe.
- Claims serialized under one lock; last claim wins.
- No Home Assistant dependency.

## 7. Service API spec

**Base:** `http://<coordinator>:8700` · **Auth:** header
`X-Display-Token: <SOFTKVM_TOKEN>` on all endpoints except `/health`. The secret
is scoped by construction: it can read/flip one display bit and nothing else.

| Endpoint                    | Behavior                                                                                                                                           |
| --------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /claim/{id}`          | Idempotent. Increments `epoch` and wakes waiters **only on actual change**. → `200 {owner, epoch, changed}` · `401` bad token · `400` unknown host |
| `GET /state`                | → `200 {owner, epoch, since, live, server_id}`                                                                                                     |
| `GET /wait?epoch=N&id=<me>` | Long-poll. Returns `200 {owner, epoch}` immediately when `epoch ≠ N`; `204` after 50 s. Registers `id` as live for the duration                    |
| `GET /health`               | Unauthenticated liveness → `200 ok`                                                                                                                |

- `since` is RFC 3339. `live` is `{"linux": true, "mac": false}`, derived from
  currently-open `/wait` connections. `server_id` is a UUID regenerated on every
  process start, so an agent can tell a restart from a state change.
- **Clients treat `epoch` as opaque.** Adopt whatever the server returns; never
  compare numerically. A restored-from-backup or reset `state.json` moves the
  epoch backwards, and a client that only waits for `epoch > N` spins at line
  rate.
- LAN bind, plain HTTP. Accepted risk: low-value secret on trusted LAN.

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
| Token mismatch after rotation                                     | Claims and waits fail 401 with a distinct log line; nothing switches until the env files and the running units agree again (§9)                                                                                                                                                                                                                            |
| Server restarted                                                  | Long-polls reset; agents reconcile on error; `server_id` change is not by itself a state change                                                                                                                                                                                                                                                            |
| **Mis-flip: monitor points at a host that cannot switch it back** | Unrecoverable over the network. `soft-kvm activate` does *not* help: the newly-designated loser is the absent host, and it is the one that would have to run DDC. With Auto Input Switch off, the OSD button is the only recovery — and unlike the failed-write case, no agent is left able to notify. The §4.3 gates exist to make this state unreachable |
| Switch command exits 0 but the monitor does not move              | `--check-cmd` keeps succeeding past `--confirm`; retry up to `--switch-retries`, then notify and stop. The bit stays correct and the user presses OSD (§4.3)                                                                                                                                                                                               |
| Switch fails while the monitor is in standby                      | Same path, and the retries usually cover it — a monitor waking from standby answers DDC a second or two late                                                                                                                                                                                                                                               |
| Deep Sleep left on                                                | Every switch hot-unplugs the monitor on the losing host: Hyprland collapses workspaces onto nothing, macOS rearranges windows. Set it Off                                                                                                                                                                                                                  |
| mDNS browse returns nothing                                       | Cached address is tried first and usually still valid; otherwise back off to 60 s and keep browsing. No claims are lost that a `SOFTKVM_SERVER` override would not also have lost                                                                                                                                                                          |
| mDNS returns a stale or rogue record                              | Connection fails on the token check, or succeeds against an impostor (§5.1). Agents re-browse on any connection error rather than pinning the first answer                                                                                                                                                                                                 |
| Server moves host                                                 | Nothing is reconfigured; the new instance advertises, caches expire on first failure                                                                                                                                                                                                                                                                       |

## 9. Security notes

- Token blast radius: read/flip one display bit. Rotating = editing the env
  files and restarting the server and both agents — the token is read at process
  start, and everything 401s until they agree (§8). The token never appears in
  `ps` or shell history.
- No Home Assistant credentials anywhere in the system.
- Corp device surface: one binary, outbound HTTP to one LAN host, no listeners,
  no credentials of value, no installs. mDNS adds outbound multicast on UDP 5353
  and an inbound multicast socket — a listener in the kernel sense, and a point
  worth checking against the corp policy before it is discovered by someone
  else.
- The service is advertised to the whole LAN. What leaks is "a soft-kvm server
  exists here", never the token (§5.1).
- The agent executes `SWITCH-CMD` as given. It is an argv slice, never a shell
  string — no `sh -c`, no interpolation of anything received from the server.
  The server can flip a bit; it can never choose what runs.

## 10. Integration tests

Run at the end, against the assembled system. Both switch commands already work
by hand (finding 9), so nothing here blocks writing the code — these check the
seams, and every one of them needs hardware that is not on the desk yet or a
binary that does not exist yet.

- [ ] Four-combo test with the USB switch (`lsusb` /
      `ioreg -r -c IOUSBHostDevice`: switch-to-me / away × receiver present /
      absent) — clean attach/detach, < 2 s, on **both** hosts. macOS has no
      fallback detector if this fails (§6.3), and no detector means no trigger
- [ ] While the rig is up: record a real Bolt attach into `testdata/` from both
      hosts — raw uevent bytes on Linux, `ioreg` output on the Mac (§11.4)
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
- [ ] Netlink uevent filter fires once per attach, not once per interface, and
      does not fire for the Unifying receiver `046d:c52b`
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
      profile, for both the mDNS browse and the plain HTTP long-poll — a denial
      is silent and looks exactly like a network outage (§5.1)
- [ ] mDNS browse from the Mac **over WiFi** resolves the server in < 3 s,
      repeatedly, including after the AP roams. If not, set `SOFTKVM_SERVER` on
      the Mac and treat discovery as a Linux-only convenience
- [ ] Kill the server, move it to the other host, restart: agents reconnect with
      no config change

## 11. Implementation notes

**No CLI framework.** Three subcommands, one `flag.NewFlagSet` each, a switch on
`os.Args[1]`. Cobra costs two modules and actively hurts here: pflag parses
flags interspersed with positional arguments, so `-vcp=inputSelect` inside the
trailing switch command is read as an unknown flag unless interspersal is turned
off. Stdlib `flag` stops at the first non-flag argument and hands the rest to
`Args()` — which *is* the `-- SWITCH-CMD ARGS...` convention, for free.

**One package.** Everything in `main`, one file per concern: `main.go`
(dispatch, flag sets), `machine.go` + `machine_test.go` (§11.3), `agent.go` (the
loop that feeds it), `server.go`, `state.go` (atomic JSON), `discover.go` (mDNS
advertise, browse, address cache), `run.go` (the argv runner and the per-OS
defaults), `detect_usb_linux.go`, `detect_usb_darwin.go`, `detect_bt_linux.go`,
`guard_darwin.go`. Build tags in filenames, not in `//go:build` lines, wherever
the split is per-OS. Around 2000 lines total does not need `internal/`.

**Logging is `log/slog`** to stderr, text handler — journald on Linux, the
LaunchAgent log file on macOS. Every switch decision logs at Info with the
reason it passed or failed each §4.3 gate; nothing else does.

### 11.1 Cancellation

`signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` in `main`, one
context down through everything. `context.Context` is a first parameter, never a
struct field. Three blocking things do not respect it by default:

- **The netlink socket.** `unix.Recvfrom` blocks and cancellation cannot reach
  it. Set the fd non-blocking before wrapping — `unix.SetNonblock(fd, true)`,
  then `os.NewFile(uintptr(fd), "netlink")` — which registers it with the
  runtime poller, so `SetReadDeadline` works and a goroutine watching
  `ctx.Done()` can unblock the read. Closing the fd from another goroutine also
  works and is racy; the deadline is the version worth writing.
- **Child processes.** `exec.CommandContext` sends SIGKILL on cancel, which can
  cut `ddcutil` mid-I2C transaction. Set `cmd.Cancel` to send SIGTERM and
  `cmd.WaitDelay = 2 * time.Second` so SIGKILL is the fallback rather than the
  first move.
- **In-flight long-polls at shutdown.** `srv.Shutdown` waits for active
  requests, and a `/wait` handler is active for up to 50 s. Give the server
  `BaseContext: func(net.Listener) context.Context { return ctx }` so every
  request context is a child of the process context: one cancel returns every
  waiter immediately, and `Shutdown` then completes in milliseconds.

Supervision is a `sync.WaitGroup` over three goroutines and a
`context.WithCancel` — not `errgroup`, because with three goroutines and no
error to propagate upward it would be a module for fifteen lines.

### 11.2 What is an interface, and what is not

An interface earns its place when two real implementations exist. Two do:

```go
type Detector interface {  // netlink, ioreg poll, bluez, and a fake
    Run(ctx context.Context, attach chan<- struct{}) error
}
type Guard interface {     // none on Linux, pmset+display on macOS, and a fake
    OK(ctx context.Context) (ok bool, reason string)
}
```

`Guard.OK` returns the reason as well as the verdict because "dormant, no AC
power" and "dormant, no LG" are different bug reports.

What does *not* get an interface:

- **The display.** There is one implementation — run this argv slice — for the
  switch, the probe, the notification, and later the flip. The seam tests need
  is the runner, so it is a func type, not an interface:
  `type Runner func(ctx context.Context, argv []string) error`. A fake runner
  records calls and returns canned exit codes.
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
    WakeAt    time.Time     // when to deliver the next timer event
}

func (m *Machine) Step(e Event) []Action
```

`agent.go` is then dumb glue: read from the detector channel, the long-poll, and
a timer; call `Step`; execute whatever comes back; feed the results in as the
next events. It contains no policy, so a bug in it looks like "nothing
happened", not "it switched to a host that is not there".

### 11.4 Tests

- **`Step` table tests are the bulk of the suite.** Every row of §8 is a case,
  and so is every combination of the §4.3 gates. Cheap enough to be exhaustive:
  no clock, no processes, no sockets.
- **Server tests run the real handler** under `httptest`, including two
  concurrent long-polls woken by one claim, an epoch that moves backwards, a bad
  token, and an unknown host id.
- **Parsers are pure functions over captured bytes.** `parseUevent([]byte)` and
  the ioreg parser get real output recorded once into `testdata/` — a Bolt
  attach on this desktop, a Bolt attach on the Mac — and are then testable on
  either OS. This is the only way the macOS parser gets tested from Linux, so
  capture both during the four-combo test (§10), while the hardware is on the
  desk and the switch is being flipped anyway. Recording them afterwards means
  setting the rig up twice.
- `go test -race ./...`; the long-poll broadcast channel is the one place a race
  is plausible.
- **Not tested: the switch commands themselves.** They need the monitor, and
  they are §10.

## 12. Build & packaging

- Module: `github.com/<user>/soft-kvm`; layout in §11.
- Deps: stdlib, `golang.org/x/sys/unix`, `github.com/grandcat/zeroconf` (which
  pulls `miekg/dns` and `golang.org/x/net`). All pure Go; `CGO_ENABLED=0`,
  `-trimpath`, `-ldflags="-s -w"` still hold. Discovery costs three modules
  where the rest of the binary costs one — that is the price of not editing an
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
    `betterdisplaycli set -productNameLike=<name> -feature=ddc -vcp=inputSelect -value=<code>`;
    common codes: DP=15, HDMI=17)

[^6^]: m1ddc — DDC control on Apple Silicon via IOAVService, single binary:
    https://github.com/waydabber/m1ddc
