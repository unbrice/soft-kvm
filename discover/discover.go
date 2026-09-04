// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// discover.go: mDNS advertise/browse and server address resolution (SPEC §5.1,
// §8).

package discover

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// serviceType is the mDNS service type advertiser and browsers must agree on.
const serviceType = "_soft-kvm._tcp"

// Advertise registers the server under _soft-kvm._tcp.local. with instance
// name `instance` on `port` and the TXT record from txtRecord. The returned
// func deregisters.
func Advertise(instance string, port int, fp string) (stop func(), err error) {
	srv, err := zeroconf.Register(instance, serviceType, "local.", port, txtRecord(instance, fp), nil)
	if err != nil {
		return nil, err
	}
	return func() { srv.Shutdown() }, nil
}

// txtRecord builds the advertised TXT record: the protocol version, the
// instance id and the key fingerprint — NEVER the token, it is broadcast to
// the whole LAN (SPEC §5.1, §9).
func txtRecord(instance, fp string) []string {
	return []string{"proto=1", "id=" + instance, "kh=" + fp}
}

// browseRound runs one mDNS browse round (the caller supplies the timeout via
// ctx, SPEC §5.1 uses 3 s) and yields every address of every matching entry as
// "host:port", ranked per rankAddrs. Entries whose kh= fingerprint is missing
// or differs from wantFP are warned about once per distinct fingerprint and
// skipped; an empty wantFP disables the check. Iteration stops when yield
// returns false, ctx ends, or an entry error occurs.
//
// grandcat/zeroconf is unmaintained (v1.0.0, 2021; last commit 2023) and two
// of its known bugs shape this function: reusing a resolver or entries
// channel across Browse calls races into close-of-closed-channel panics
// (#118, #113), so every round builds both fresh; and the mainloop drops an
// entry whose first response carries no A/AAAA record (#124), which the retry
// loop in Resolver.Resolve absorbs.
func browseRound(ctx context.Context, wantFP string, yield func(string) bool) error {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return err
	}

	// Buffered: zeroconf's mainloop sends with no ctx select, so an
	// unbuffered channel would strand that goroutine on a send racing the
	// round's timeout (SPEC §5.1).
	entries := make(chan *zeroconf.ServiceEntry, 1)
	if err := resolver.Browse(ctx, serviceType, "local.", entries); err != nil {
		return err
	}

	warned := make(map[string]bool) // fingerprints already warned about
	for {
		select {
		case entry := <-entries:
			if entry == nil {
				continue
			}
			if wantFP != "" {
				fp := entryFingerprint(entry)
				if fp != wantFP {
					if !warned[fp] {
						warned[fp] = true
						slog.Warn("ignoring soft-kvm server advertising a different token",
							"instance", entry.Instance, "fingerprint", fp)
					}
					continue
				}
			}
			for _, ip := range rankAddrs(entry) {
				if !yield(net.JoinHostPort(ip.String(), strconv.Itoa(entry.Port))) {
					return nil
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// entryFingerprint extracts the kh= value from an entry's TXT record, or "".
func entryFingerprint(entry *zeroconf.ServiceEntry) string {
	for _, kv := range entry.Text {
		if fp, ok := strings.CutPrefix(kv, "kh="); ok {
			return fp
		}
	}
	return ""
}

// rankAddrs returns the entry's addresses ordered for dialling: routable IPv4,
// routable IPv6, link-local IPv4, link-local IPv6; loopback is excluded. A
// multi-homed server advertises every interface it has — Docker bridges, VPN
// tunnels, self-assigned 169.254/16 — and zeroconf does not filter them
// (grandcat/zeroconf#43, fixed only by the unmerged PR #125), so any single
// record may be unreachable. The ranking only orders the *attempts*; it
// cannot recognise an unroutable *private* address (a bridge IP looks like a
// LAN IP), so reachability is proven by the caller's real request.
func rankAddrs(entry *zeroconf.ServiceEntry) []net.IP {
	var ranked []net.IP
	for _, linkLocal := range []bool{false, true} {
		for _, addrs := range [][]net.IP{entry.AddrIPv4, entry.AddrIPv6} {
			for _, ip := range addrs {
				if ip.IsLoopback() || ip.IsLinkLocalUnicast() != linkLocal {
					continue
				}
				ranked = append(ranked, ip)
			}
		}
	}
	return ranked
}

// Resolver resolves server addresses, caching the last working one.
type Resolver struct {
	cachePath string
}

// NewResolver returns a Resolver that reads and writes its cache at cachePath.
func NewResolver(cachePath string) *Resolver {
	return &Resolver{cachePath: cachePath}
}

// Resolve implements the SPEC §5.1 resolution order: explicit
// override → SOFTKVM_SERVER env → cached address → mDNS browse. It
// streams "host:port" candidates instead of resolving to one: the caller
// tries each with the real TLS-verified request until one connects. Explicit
// and env yield once; the cached address yields first, then resolution falls
// through to mDNS so a stale entry is no longer terminal. The browse yields
// every ranked address of every fingerprint-matching entry, re-browsing in 3 s
// rounds with exponential backoff capped at 60 s, until ctx is cancelled.
// wantFP filters mDNS entries by their kh= fingerprint; empty disables the
// check.
func (r *Resolver) Resolve(ctx context.Context, explicit, wantFP string) iter.Seq[string] {
	return func(yield func(string) bool) {
		if explicit != "" {
			yield(explicit)
			return
		}
		if addr := os.Getenv("SOFTKVM_SERVER"); addr != "" {
			yield(addr)
			return
		}
		if addr := r.load(); addr != "" {
			if !yield(addr) {
				return
			}
			// Fall through to mDNS: the cached address may be stale.
		}

		backoff := time.Second
		for ctx.Err() == nil {
			// 3 s per browse round (SPEC §5.1): re-browsing retransmits the
			// query, which is what survives a dropped multicast on WiFi.
			bctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			yielded, stopped := false, false
			err := browseRound(bctx, wantFP, func(addr string) bool {
				yielded = true
				if !yield(addr) {
					stopped = true
					return false
				}
				return true
			})
			cancel()
			if stopped || ctx.Err() != nil {
				return
			}
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				slog.Debug("browse failed, retrying", "error", err, "backoff", backoff)
			} else if !yielded {
				slog.Debug("browse round found no server, retrying", "backoff", backoff)
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 60*time.Second {
				backoff *= 2
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
			}
		}
	}
}

// Save writes "host:port\n" to the cache path. It is best-effort: failures are
// logged but never fatal.
func (r *Resolver) Save(addr string) {
	dir := filepath.Dir(r.cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("failed to create state directory", "dir", dir, "error", err)
		return
	}
	if err := os.WriteFile(r.cachePath, []byte(addr+"\n"), 0o600); err != nil {
		slog.Error("failed to cache server address", "path", r.cachePath, "error", err)
	}
}

// load reads the last successfully used server address from the cache path.
// Any problem (missing file, bad permissions, etc.) returns "" so resolution
// falls through to mDNS (SPEC §5.1).
func (r *Resolver) load() string {
	data, err := os.ReadFile(r.cachePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
