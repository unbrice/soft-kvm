// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// shadow_linux.go: kernelShadow for Linux. The logitech-djreceiver driver
// creates one virtual hidraw node per receiver-paired peripheral, nested under
// the receiver's hid device in sysfs. Those shadows accept HID++ writes but
// never relay the replies to userspace; the paired device is reached through
// the receiver's pairing slots instead (SPEC §5.5).

package detect

import (
	"path/filepath"
	"regexp"
)

// hidDeviceComponent matches one hid bus device in a sysfs path, e.g.
// "/0003:046D:C52B.0004".
var hidDeviceComponent = regexp.MustCompile(`/[0-9A-Fa-f]{4}:[0-9A-Fa-f]{4}:[0-9A-Fa-f]{4}\.`)

// kernelShadow reports whether the hidraw node at path is a kernel-created
// virtual node for a receiver-paired peripheral: its hid device is nested
// under another hid device (the receiver). Wired and Bluetooth devices have
// exactly one hid device component in their sysfs path.
func kernelShadow(path string) bool {
	real, err := filepath.EvalSymlinks(
		filepath.Join("/sys/class/hidraw", filepath.Base(path), "device"))
	if err != nil {
		return false
	}
	return len(hidDeviceComponent.FindAllString(real, -1)) >= 2
}
