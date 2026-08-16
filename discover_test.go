// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"iter"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

func TestCacheRoundTrip(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stateDir env override is only verified on Linux")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const addr = "127.0.0.1:8700"
	saveCachedServer(addr)
	if got := loadCachedServer(); got != addr {
		t.Fatalf("loadCachedServer returned %q, want %q", got, addr)
	}
}

func TestCacheMissing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stateDir env override is only verified on Linux")
	}
	dir := t.TempDir()
	// Point at an empty directory so the cache file is missing.
	t.Setenv("XDG_STATE_HOME", dir)
	if got := loadCachedServer(); got != "" {
		t.Fatalf("missing cache returned %q, want empty", got)
	}
}

func TestMDNSRoundTrip(t *testing.T) {
	// Find a free port to advertise on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	const instance = "test-instance"
	const token = "round-trip-token"
	stop, err := advertise(instance, port, keyFingerprint(token))
	if err != nil {
		t.Fatalf("advertise: %v", err)
	}
	defer stop()

	// Give the server time to start its listeners and probe.
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	entries := make(chan *zeroconf.ServiceEntry)
	if err := resolver.Browse(ctx, "_soft-kvm._tcp", "local.", entries); err != nil {
		t.Fatalf("browse: %v", err)
	}

	select {
	case entry := <-entries:
		if entry == nil {
			t.Skip("mDNS browse returned no entries (multicast loopback unavailable)")
		}
		if entry.Instance != instance {
			t.Errorf("instance %q, want %q", entry.Instance, instance)
		}
		if entry.Port != port {
			t.Errorf("port %d, want %d", entry.Port, port)
		}
		wantTXT := []string{"proto=1", "id=" + instance, "kh=" + keyFingerprint(token)}
		for _, w := range wantTXT {
			found := false
			for _, txt := range entry.Text {
				if txt == w {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("TXT record missing %q, got %v", w, entry.Text)
			}
		}
		for _, txt := range entry.Text {
			if strings.Contains(txt, token) {
				t.Errorf("TXT record leaks token: %q", txt)
			}
		}
	case <-ctx.Done():
		t.Skip("mDNS browse returned no entries within timeout (multicast loopback unavailable)")
	}
}

func TestRankAddrsOrdering(t *testing.T) {
	tests := []struct {
		name  string
		entry zeroconf.ServiceEntry
		want  []string
	}{
		{
			name:  "prefers IPv4 over IPv6",
			entry: zeroconf.ServiceEntry{AddrIPv4: []net.IP{net.ParseIP("192.168.1.2")}, AddrIPv6: []net.IP{net.ParseIP("fd00::2")}},
			want:  []string{"192.168.1.2", "fd00::2"},
		},
		{
			name:  "prefers routable over link-local",
			entry: zeroconf.ServiceEntry{AddrIPv4: []net.IP{net.ParseIP("169.254.3.4"), net.ParseIP("192.168.1.2")}},
			want:  []string{"192.168.1.2", "169.254.3.4"},
		},
		{
			name:  "skips loopback",
			entry: zeroconf.ServiceEntry{AddrIPv4: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.2")}},
			want:  []string{"10.0.0.2"},
		},
		{
			name:  "falls back to IPv6",
			entry: zeroconf.ServiceEntry{AddrIPv6: []net.IP{net.ParseIP("fd00::2")}},
			want:  []string{"fd00::2"},
		},
		{
			name:  "falls back to link-local as last resort",
			entry: zeroconf.ServiceEntry{AddrIPv4: []net.IP{net.ParseIP("169.254.3.4")}, AddrIPv6: []net.IP{net.ParseIP("fe80::1")}},
			want:  []string{"169.254.3.4", "fe80::1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, ip := range rankAddrs(&tc.entry) {
				got = append(got, ip.String())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRankAddrsEmpty(t *testing.T) {
	if got := rankAddrs(&zeroconf.ServiceEntry{}); len(got) != 0 {
		t.Fatalf("expected no addresses, got %v", got)
	}
}

func TestBrowseSkipsForeignToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	stop, err := advertise("test-instance", port, keyFingerprint("token-a"))
	if err != nil {
		t.Fatalf("advertise: %v", err)
	}
	defer stop()
	time.Sleep(100 * time.Millisecond)

	// Control: the matching fingerprint must find the server. If even it finds
	// nothing, multicast loopback is unavailable and the test proves nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	found := false
	_ = browseRound(ctx, keyFingerprint("token-a"), func(string) bool {
		found = true
		return false
	})
	cancel()
	if !found {
		t.Skip("mDNS browse returned no entries (multicast loopback unavailable)")
	}

	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	found = false
	_ = browseRound(ctx, keyFingerprint("token-b"), func(string) bool {
		found = true
		return false
	})
	if found {
		t.Error("browseRound yielded a candidate for a foreign fingerprint")
	}
}

// collect ranges seq until it ends, returning everything it yielded.
func collect(seq iter.Seq[string]) []string {
	var out []string
	for v := range seq {
		out = append(out, v)
	}
	return out
}

func TestResolveServerExplicit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	got := collect(resolveServer(ctx, "explicit.example:1234", ""))
	if len(got) != 1 || got[0] != "explicit.example:1234" {
		t.Fatalf("got %v, want [explicit.example:1234]", got)
	}
}

func TestResolveServerEnv(t *testing.T) {
	t.Setenv("SOFTKVM_SERVER", "env.example:5678")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	got := collect(resolveServer(ctx, "", ""))
	if len(got) != 1 || got[0] != "env.example:5678" {
		t.Fatalf("got %v, want [env.example:5678]", got)
	}
}

func TestResolveServerCacheFallsThrough(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stateDir env override is only verified on Linux")
	}
	t.Setenv("SOFTKVM_SERVER", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const cached = "cached.example:9999"
	saveCachedServer(cached)

	ctx, cancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
	defer cancel()

	start := time.Now()
	// A fingerprint nothing on the LAN advertises, so the mDNS phase yields
	// nothing: the cached candidate comes first, then the sequence only ends
	// with ctx — proving the cache fell through to mDNS instead of returning.
	got := collect(resolveServer(ctx, "", keyFingerprint("no-such-token")))
	elapsed := time.Since(start)
	if len(got) != 1 || got[0] != cached {
		t.Fatalf("got %v, want [%s]", got, cached)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("sequence ended too quickly — no fall-through to mDNS: %v", elapsed)
	}
}

func TestResolveServerBrowseUntilCancelled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stateDir env override is only verified on Linux")
	}
	// Ensure none of the higher-priority sources are set.
	t.Setenv("SOFTKVM_SERVER", "")
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	_ = os.Remove(filepath.Join(stateDir(), "server"))

	ctx, cancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
	defer cancel()

	start := time.Now()
	got := collect(resolveServer(ctx, "", keyFingerprint("no-such-token")))
	elapsed := time.Since(start)
	if len(got) != 0 {
		t.Fatalf("expected no candidates, got %v", got)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("cancelled too quickly: %v", elapsed)
	}
}
