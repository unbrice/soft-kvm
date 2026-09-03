// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// hint_linux.go: the permission remediation for Linux. hidraw nodes are
// root-only by default; the HID++ scan and hid-switch open them O_RDWR
// (SPEC §6.1).

package detect

const permissionRemediation = "grant write access to the hidraw node: a udev rule with TAG+=\"uaccess\", or a group, as Solaar's packaging does"
