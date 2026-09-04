// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// hint_linux.go: the permission remediation for Linux. hidraw nodes are
// root-only by default; the HID++ scan and hid-switch open them O_RDWR
// (SPEC §6.1).

package detect

// PermissionRemediation is the fix to print when opening a HID node is
// refused. detect prints it on a failed HID++ scan, mac-debug when its open
// is refused.
const PermissionRemediation = "grant write access to the hidraw node: a udev rule with TAG+=\"uaccess\", or a group, as Solaar's packaging does"
