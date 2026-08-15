// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// USB attach detector for Linux: NETLINK_KOBJECT_UEVENT multicast group 2
// (SPEC §6.1).

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func newUSBDetector(vidpid string) (Detector, error) {
	vid, pid, err := parseVIDPID(vidpid)
	if err != nil {
		return nil, err
	}
	return &usbDetector{vid: vid, pid: pid}, nil
}

type usbDetector struct {
	vid int
	pid int
}

// Run reads netlink uevents until ctx is cancelled. Returns nil on ctx
// cancellation.
func (d *usbDetector) Run(ctx context.Context, attach chan<- struct{}) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}

	addr := &unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
		Groups: 2, // udev group; group 1 is raw kernel and root-only
	}
	if err := unix.Bind(fd, addr); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("netlink bind: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 256*1024); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("netlink SO_RCVBUF: %w", err)
	}
	// Drop events instead of erroring when the buffer overflows: a slow
	// reader must lose uevents silently, not die (SPEC §6).
	if err := unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_NO_ENOBUFS, 1); err != nil {
		slog.Warn("netlink NETLINK_NO_ENOBUFS failed, tolerating ENOBUFS", "error", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("netlink nonblock: %w", err)
	}

	f := os.NewFile(uintptr(fd), "netlink")
	defer func() { _ = f.Close() }()

	buf := make([]byte, 64*1024)
	for {
		if err := f.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return fmt.Errorf("netlink deadline: %w", err)
		}
		n, err := f.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				select {
				case <-ctx.Done():
					return nil
				default:
					continue
				}
			}
			// Buffer overflow despite NETLINK_NO_ENOBUFS (old kernel):
			// events were lost, keep going.
			if errors.Is(err, unix.ENOBUFS) {
				continue
			}
			return fmt.Errorf("netlink recv: %w", err)
		}

		props, ok := parseUevent(buf[:n])
		if !ok {
			continue
		}
		if !usbUeventMatch(props, d.vid, d.pid) {
			continue
		}

		select {
		case attach <- struct{}{}:
			slog.Info("usb receiver attached", "vid", fmt.Sprintf("%04x", d.vid), "pid", fmt.Sprintf("%04x", d.pid))
		case <-ctx.Done():
			return nil
		}
	}
}
