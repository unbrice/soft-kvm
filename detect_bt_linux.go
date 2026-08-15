// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Bluetooth fallback detector for Linux: gdbus monitor of BlueZ (SPEC §6.3).

package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"
)

func newBTDetector(mac string) Detector {
	return &btDetector{mac: normalizeBTMAC(mac)}
}

type btDetector struct {
	mac string
}

func normalizeBTMAC(mac string) string {
	return strings.ToUpper(strings.TrimSpace(mac))
}

// Run restarts the gdbus monitor with backoff. Returns nil on ctx
// cancellation.
func (d *btDetector) Run(ctx context.Context, attach chan<- struct{}) error {
	b := newBackoff()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		start := time.Now()
		err := d.runOnce(ctx, attach)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("gdbus monitor exited", "err", err)
		}
		// A monitor that stayed up this long was healthy; the failure was
		// transient, so start the backoff over.
		if time.Since(start) > 30*time.Second {
			b.reset()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(b.next()):
		}
	}
}

// runOnce monitors until gdbus exits or ctx is cancelled. Returns nil on ctx
// cancellation.
func (d *btDetector) runOnce(ctx context.Context, attach chan<- struct{}) error {
	cmd := commandContext(ctx, []string{"gdbus", "monitor", "--system", "--dest", "org.bluez"})
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
scan:
	for scanner.Scan() {
		if btConnectEvent(scanner.Text(), d.mac) {
			select {
			case attach <- struct{}{}:
				slog.Info("bluetooth receiver connected", "mac", d.mac)
			case <-ctx.Done():
				break scan
			}
		}
	}
	// Wait exactly once; commandContext kills gdbus when ctx is cancelled.
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil // killed by cancellation, not a real failure
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return waitErr
}

// btConnectEvent reports whether line is a BlueZ connect event for mac.
// The device path segment is dev_<MAC with ':' replaced by '_' and uppercased>.
func btConnectEvent(line, mac string) bool {
	mac = normalizeBTMAC(mac)
	path := "dev_" + strings.ReplaceAll(mac, ":", "_")
	return strings.Contains(line, path) && strings.Contains(line, "'Connected': <true>")
}
