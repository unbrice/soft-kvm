// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// macOS guard: AC power and display presence (SPEC §6.2).

package platform

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Guard is the macOS AC-power and display-presence guard.
type Guard struct {
	match    string
	disabled bool
	displays func() ([]string, error)
	mu       sync.Mutex
	lastSeen time.Time
}

// NewGuard returns a macOS guard. When disabled is true all checks are
// bypassed. displays lists the external monitors by name — main passes
// screen.DisplayNames, since platform imports nothing internal — and match is
// the name substring that counts as the shared monitor; "" is any of them.
func NewGuard(match string, disabled bool, displays func() ([]string, error)) *Guard {
	return &Guard{
		match:    strings.ToLower(match),
		disabled: disabled,
		displays: displays,
	}
}

// OK reports whether this host may participate in switching.
func (g *Guard) OK(ctx context.Context) (ok bool, reason string) {
	if g.disabled {
		return true, "guards disabled"
	}

	if err := g.acPower(ctx); err != nil {
		return false, "no AC power"
	}

	present, err := g.displayPresent()
	if err != nil {
		return false, "no display: " + err.Error()
	}
	if present {
		g.mu.Lock()
		g.lastSeen = time.Now()
		g.mu.Unlock()
	}

	g.mu.Lock()
	seen := g.lastSeen
	g.mu.Unlock()

	if !present && (seen.IsZero() || time.Since(seen) > 10*time.Minute) {
		return false, "display not seen"
	}
	return true, ""
}

func (g *Guard) acPower(ctx context.Context) error {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := CommandContext(ctx2, []string{"pmset", "-g", "ps"}).Output()
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(string(out)), "ac power") {
		return fmt.Errorf("on battery")
	}
	return nil
}

// displayPresent reports whether an external monitor whose name contains
// match is attached. displays walks the IORegistry, which takes milliseconds
// where system_profiler takes seconds (SPEC §6.2).
func (g *Guard) displayPresent() (bool, error) {
	names, err := g.displays()
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), g.match) {
			return true, nil
		}
	}
	return false, nil
}
