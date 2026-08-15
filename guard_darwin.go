// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// macOS guard: AC power and LG display presence (SPEC §6.2).

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

func newGuard(displayMatch string) Guard {
	return &displayGuard{match: strings.ToLower(displayMatch)}
}

type displayGuard struct {
	match    string
	mu       sync.Mutex
	lastSeen time.Time
}

func (g *displayGuard) OK(ctx context.Context) (ok bool, reason string) {
	if err := g.acPower(ctx); err != nil {
		return false, "no AC power"
	}

	present, err := g.displayPresent(ctx)
	if err != nil {
		return false, "no LG: " + err.Error()
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
		return false, "no LG: display not seen"
	}
	return true, ""
}

func (g *displayGuard) acPower(ctx context.Context) error {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := commandContext(ctx2, []string{"pmset", "-g", "ps"}).Output()
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(string(out)), "ac power") {
		return fmt.Errorf("on battery")
	}
	return nil
}

func (g *displayGuard) displayPresent(ctx context.Context) (bool, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := commandContext(ctx2, []string{"betterdisplaycli", "get", "-identifiers"}).Output()
	if err != nil {
		return false, err
	}
	return strings.Contains(strings.ToLower(string(out)), g.match), nil
}
