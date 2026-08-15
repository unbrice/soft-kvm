// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Pure uevent/USB helpers. parseUevent and usbUeventMatch are tested from any
// OS so this file has no build constraint (SPEC §11.4).

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

const (
	udevPrefix    = "libudev\x00"
	udevMagic     = 0xfeedcafe
	udevHeaderLen = 40
)

// parseUevent parses one group-2 (udev-processed) netlink message.
//
// The frame layout is systemd's monitor_netlink_header, verified from
// src/libsystemd/sd-device/device-monitor-private.h and
// src/libsystemd/sd-device/device-monitor.c:
//
//	prefix[8] = "libudev\0"
//	magic     = 0xfeedcafe big-endian
//	header_size, properties_off, properties_len = native u32
//	filter_subsystem_hash, filter_devtype_hash,
//	filter_tag_bloom_hi, filter_tag_bloom_lo = big-endian u32
//
// (SPEC §6.1).
func parseUevent(msg []byte) (map[string]string, bool) {
	if len(msg) < udevHeaderLen {
		return nil, false
	}
	if !bytes.Equal(msg[:8], []byte(udevPrefix)) {
		return nil, false
	}
	if binary.BigEndian.Uint32(msg[8:12]) != udevMagic {
		return nil, false
	}
	headerSize := binary.NativeEndian.Uint32(msg[12:16])
	propsOff := binary.NativeEndian.Uint32(msg[16:20])
	propsLen := binary.NativeEndian.Uint32(msg[20:24])
	if headerSize != udevHeaderLen {
		return nil, false
	}
	if propsOff < headerSize {
		return nil, false
	}
	end := int(propsOff) + int(propsLen)
	if end > len(msg) {
		return nil, false
	}

	props := make(map[string]string)
	for _, pair := range bytes.Split(msg[propsOff:end], []byte{0}) {
		if len(pair) == 0 {
			continue
		}
		eq := bytes.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		props[string(pair[:eq])] = string(pair[eq+1:])
	}
	return props, true
}

// parseVIDPID parses a "046d:c548" string into vendor/product integers.
func parseVIDPID(vidpid string) (vid, pid int, err error) {
	parts := strings.Split(vidpid, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid VID:PID %q", vidpid)
	}
	v, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid VID %q: %w", parts[0], err)
	}
	p, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid PID %q: %w", parts[1], err)
	}
	return int(v), int(p), nil
}

// usbUeventMatch reports whether props describe an attach of the configured
// USB receiver (SPEC §6.1).
func usbUeventMatch(props map[string]string, vid, pid int) bool {
	if props["SUBSYSTEM"] != "usb" || props["DEVTYPE"] != "usb_device" || props["ACTION"] != "add" {
		return false
	}
	product := props["PRODUCT"]
	parts := strings.Split(product, "/")
	if len(parts) < 2 {
		return false
	}
	pvid, err1 := strconv.ParseUint(parts[0], 16, 32)
	ppid, err2 := strconv.ParseUint(parts[1], 16, 32)
	if err1 != nil || err2 != nil {
		return false
	}
	return int(pvid) == vid && int(ppid) == pid
}
