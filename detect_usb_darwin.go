// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// USB attach detector for macOS: poll ioreg every 2 s (SPEC §6.2).

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func newUSBDetector(vidpid string) (Detector, error) {
	vid, pid, err := parseVIDPID(vidpid)
	if err != nil {
		return nil, err
	}
	return &darwinUSBDetector{vid: vid, pid: pid}, nil
}

type darwinUSBDetector struct {
	vid int
	pid int
}

// Run polls ioreg until ctx is cancelled. Returns nil on ctx cancellation.
func (d *darwinUSBDetector) Run(ctx context.Context, attach chan<- struct{}) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	present := false

	for {
		now, err := d.present(ctx)
		if err != nil {
			return err
		}
		if now && !present {
			select {
			case attach <- struct{}{}:
				slog.Info("usb receiver attached", "vid", fmt.Sprintf("%04x", d.vid), "pid", fmt.Sprintf("%04x", d.pid))
			case <-ctx.Done():
				return nil
			}
		}
		present = now

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (d *darwinUSBDetector) present(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := commandContext(ctx, []string{"ioreg", "-w", "0", "-r", "-c", "IOUSBHostDevice", "-l"}).Output()
	if err != nil {
		return false, fmt.Errorf("ioreg: %w", err)
	}
	return ioregHasDevice(out, d.vid, d.pid), nil
}
