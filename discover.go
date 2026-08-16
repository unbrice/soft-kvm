// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// discover.go: mDNS advertise/browse and server address resolution (SPEC §5.1,
// §8).

package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// advertise registers the server under _soft-kvm._tcp.local. with instance
// name `instance` on `port`. The TXT record carries the protocol version and
// the instance id — NEVER the token, it is broadcast to the whole LAN (SPEC
// §5.1). The returned func deregisters.
func advertise(instance string, port int) (stop func(), err error) {
	txt := []string{"proto=1", "id=" + instance}
	srv, err := zeroconf.Register(instance, "_soft-kvm._tcp", "local.", port, txt, nil)
	if err != nil {
		return nil, err
	}
	return func() { srv.Shutdown() }, nil
}

// browse returns the first advertised address as "host:port" (net.JoinHostPort
// on the entry's best-ranked IP, per pickAddr, and Port). The caller supplies
// the timeout via ctx (SPEC §5.1 uses 3 s).
//
// grandcat/zeroconf is unmaintained (v1.0.0, 2021; last commit 2023) and two
// of its known bugs shape this function: reusing a resolver or entries
// channel across Browse calls races into close-of-closed-channel panics
// (#118, #113), so every round builds both fresh; and the mainloop drops an
// entry whose first response carries no A/AAAA record (#124), which the 3 s
// retry loop in resolveServer absorbs.
func browse(ctx context.Context) (string, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return "", err
	}

	// Buffered: browse returns after the first entry and zeroconf's mainloop
	// sends with no ctx select, so an unbuffered channel would strand that
	// goroutine on a send racing the timeout (SPEC §5.1).
	entries := make(chan *zeroconf.ServiceEntry, 1)
	if err := resolver.Browse(ctx, "_soft-kvm._tcp", "local.", entries); err != nil {
		return "", err
	}

	select {
	case entry := <-entries:
		if entry == nil {
			return "", ctx.Err()
		}
		ip, err := pickAddr(entry)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(ip.String(), strconv.Itoa(entry.Port)), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// pickAddr ranks an entry's addresses for dialling: IPv4 over IPv6, and
// globally routable over link-local within each family. A multi-homed server
// advertises every interface it has — Docker bridges, VPN tunnels,
// self-assigned 169.254/16 — and zeroconf does not filter them
// (grandcat/zeroconf#43, fixed only by the unmerged PR #125), so the first
// record is not necessarily the reachable one. This ranking cannot recognise
// an unroutable *private* address (a bridge IP looks like a LAN IP); §8's
// re-browse on connection failure is the backstop for that.
func pickAddr(entry *zeroconf.ServiceEntry) (net.IP, error) {
	for _, linkLocal := range []bool{false, true} {
		for _, addrs := range [][]net.IP{entry.AddrIPv4, entry.AddrIPv6} {
			for _, ip := range addrs {
				if ip.IsLoopback() || ip.IsLinkLocalUnicast() != linkLocal {
					continue
				}
				return ip, nil
			}
		}
	}
	return nil, errors.New("mDNS entry has no address")
}

// resolveServer implements the SPEC §5.1 resolution order: explicit
// (--server flag) → SOFTKVM_SERVER env → cached address → mDNS browse. The
// browse retries with exponential backoff capped at 60 s until ctx is
// cancelled; the other sources are tried once each.
func resolveServer(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if addr := os.Getenv("SOFTKVM_SERVER"); addr != "" {
		return addr, nil
	}
	if addr := loadCachedServer(); addr != "" {
		return addr, nil
	}

	backoff := time.Second
	for {
		// 3 s per browse round (SPEC §5.1): re-browsing retransmits the query,
		// which is what survives a dropped multicast on WiFi.
		bctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		addr, err := browse(bctx)
		cancel()
		if err == nil {
			return addr, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		slog.Debug("browse failed, retrying", "error", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if backoff < 60*time.Second {
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
		}
	}
}

// loadCachedServer reads the last successfully used server address from
// stateDir()/server. Any problem (missing file, bad permissions, etc.) returns
// "" so resolution falls through to mDNS (SPEC §5.1).
func loadCachedServer() string {
	data, err := os.ReadFile(filepath.Join(stateDir(), "server"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveCachedServer writes "host:port\n" to stateDir()/server. It is best-effort:
// failures are logged but never fatal.
func saveCachedServer(addr string) {
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("failed to create state directory", "dir", dir, "error", err)
		return
	}
	path := filepath.Join(dir, "server")
	if err := os.WriteFile(path, []byte(addr+"\n"), 0o600); err != nil {
		slog.Error("failed to cache server address", "path", path, "error", err)
	}
}
