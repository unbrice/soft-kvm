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
// on the entry's IP — prefer IPv4 — and Port). The caller supplies the timeout
// via ctx (SPEC §5.1 uses 3 s).
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
		var ip net.IP
		if len(entry.AddrIPv4) > 0 {
			ip = entry.AddrIPv4[0]
		} else if len(entry.AddrIPv6) > 0 {
			ip = entry.AddrIPv6[0]
		} else {
			return "", errors.New("mDNS entry has no address")
		}
		return net.JoinHostPort(ip.String(), strconv.Itoa(entry.Port)), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
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
