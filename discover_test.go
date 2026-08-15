// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
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
	stop, err := advertise(instance, port)
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
		wantTXT := []string{"proto=1", "id=" + instance}
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
			if strings.Contains(txt, "token") {
				t.Errorf("TXT record leaks token: %q", txt)
			}
		}
	case <-ctx.Done():
		t.Skip("mDNS browse returned no entries within timeout (multicast loopback unavailable)")
	}
}

func TestResolveServerExplicit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	got, err := resolveServer(ctx, "explicit.example:1234")
	if err != nil {
		t.Fatalf("resolveServer: %v", err)
	}
	if got != "explicit.example:1234" {
		t.Fatalf("got %q, want explicit.example:1234", got)
	}
}

func TestResolveServerEnv(t *testing.T) {
	t.Setenv("SOFTKVM_SERVER", "env.example:5678")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	got, err := resolveServer(ctx, "")
	if err != nil {
		t.Fatalf("resolveServer: %v", err)
	}
	if got != "env.example:5678" {
		t.Fatalf("got %q, want env.example:5678", got)
	}
}

func TestResolveServerCache(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stateDir env override is only verified on Linux")
	}
	t.Setenv("SOFTKVM_SERVER", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const cached = "cached.example:9999"
	saveCachedServer(cached)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	got, err := resolveServer(ctx, "")
	if err != nil {
		t.Fatalf("resolveServer: %v", err)
	}
	if got != cached {
		t.Fatalf("got %q, want %q", got, cached)
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
	_, err := resolveServer(ctx, "")
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("cancelled too quickly: %v", elapsed)
	}
}
