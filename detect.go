// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// detect.go: HID device enumeration and --trigger suggestions.

package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/telesma-app/hid"
)

type deviceKey struct {
	vid, pid uint16
	serial   string
}

type ifaceUsage struct {
	usagePage uint16
	usage     uint16
}

type hidDevice struct {
	key        deviceKey
	mfr        string
	product    string
	interfaces []ifaceUsage
}

// detectCmd enumerates HID devices and prints how to set --trigger for
// connect.
func detectCmd(ctx context.Context) error {
	fs := flag.NewFlagSet("detect", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: soft-kvm detect")
	}
	if err := fs.Parse(os.Args[2:]); err != nil {
		return errUsage
	}
	if fs.NArg() > 0 {
		return errUsage
	}

	devices, err := enumerateHIDDevices(ctx)
	if err != nil {
		return fmt.Errorf("enumerate HID devices: %w", err)
	}
	if err := renderDevices(os.Stdout, devices); err != nil {
		return err
	}
	if err := renderSuggestions(os.Stdout, devices); err != nil {
		return err
	}
	return nil
}

func enumerateHIDDevices(ctx context.Context) ([]*hidDevice, error) {
	_ = ctx // hid.Enumerate does not accept a context today.
	groups := make(map[deviceKey]*hidDevice)
	for info, err := range hid.Enumerate() {
		if err != nil {
			return nil, err
		}
		key := deviceKey{vid: info.VendorID, pid: info.ProductID, serial: info.SerialNbr}
		dev, ok := groups[key]
		if !ok {
			dev = &hidDevice{
				key:     key,
				mfr:     info.MfrStr,
				product: info.ProductStr,
			}
			groups[key] = dev
		}
		dev.interfaces = append(dev.interfaces, ifaceUsage{usagePage: info.UsagePage, usage: info.Usage})
	}
	devices := make([]*hidDevice, 0, len(groups))
	for _, dev := range groups {
		devices = append(devices, dev)
	}
	slices.SortFunc(devices, func(a, b *hidDevice) int {
		if c := cmp.Compare(a.key.vid, b.key.vid); c != 0 {
			return c
		}
		if c := cmp.Compare(a.key.pid, b.key.pid); c != 0 {
			return c
		}
		return strings.Compare(a.key.serial, b.key.serial)
	})
	return devices, nil
}

func renderDevices(w io.Writer, devices []*hidDevice) error {
	if len(devices) == 0 {
		_, err := fmt.Fprintln(w, "No HID devices detected.")
		return err
	}
	type row struct {
		vidpid   string
		name     string
		ifaces   string
		keyboard bool
	}
	rows := make([]row, len(devices))
	col1Width, col2Width, col3Width := 0, 0, 0
	for i, dev := range devices {
		rows[i] = row{
			vidpid:   vidpidString(dev.key.vid, dev.key.pid),
			name:     deviceName(dev),
			ifaces:   ifaceList(dev.interfaces),
			keyboard: hasKeyboard(dev),
		}
		col1Width = max(col1Width, len(rows[i].vidpid))
		col2Width = max(col2Width, len(rows[i].name))
		col3Width = max(col3Width, len(rows[i].ifaces))
	}
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %-*s", col1Width, r.vidpid, col2Width, r.name, col3Width, r.ifaces)
		if !r.keyboard {
			line += " (no keyboard)"
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func renderSuggestions(w io.Writer, devices []*hidDevice) error {
	var candidates []*hidDevice
	for _, dev := range devices {
		if hasKeyboard(dev) {
			candidates = append(candidates, dev)
		}
	}

	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Trigger candidates are keyboard-capable devices (usage page 0x01, usage 0x06)."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if len(candidates) == 0 {
		if _, err := fmt.Fprintln(w, "No keyboard-capable HID devices detected."); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, "Plug the receiver in and re-run detect.")
		return err
	}

	if _, err := fmt.Fprintln(w, "Suggested invocation:"); err != nil {
		return err
	}
	parts := make([]string, len(candidates))
	for i, dev := range candidates {
		parts[i] = vidpidString(dev.key.vid, dev.key.pid)
	}
	_, err := fmt.Fprintf(w, "  soft-kvm connect --trigger %s\n", strings.Join(parts, ","))
	return err
}

func deviceName(dev *hidDevice) string {
	switch {
	case dev.mfr != "" && dev.product != "":
		return dev.mfr + " — " + dev.product
	case dev.mfr != "":
		return dev.mfr
	case dev.product != "":
		return dev.product
	default:
		return "(unknown)"
	}
}

func ifaceList(ifaces []ifaceUsage) string {
	n := len(ifaces)
	if n == 0 {
		return "0 interfaces"
	}
	names := make([]string, n)
	for i, u := range ifaces {
		names[i] = usageName(u.usagePage, u.usage)
	}
	noun := "interfaces"
	if n == 1 {
		noun = "interface"
	}
	return fmt.Sprintf("%d %s: %s", n, noun, strings.Join(names, ", "))
}

func hasKeyboard(dev *hidDevice) bool {
	for _, u := range dev.interfaces {
		if u.usagePage == 0x01 && u.usage == 0x06 {
			return true
		}
	}
	return false
}

func usageName(usagePage, usage uint16) string {
	if usagePage == 0x01 && usage == 0x06 {
		return "keyboard"
	}
	if usagePage == 0x01 && usage == 0x02 {
		return "mouse"
	}
	if usagePage == 0x0C && usage == 0x01 {
		return "consumer"
	}
	return "raw"
}

func vidpidString(vid, pid uint16) string {
	return fmt.Sprintf("%04x:%04x", vid, pid)
}
