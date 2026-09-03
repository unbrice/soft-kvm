// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// hint_darwin.go: the permission remediation for macOS. Enumeration needs no
// grant, but opening a keyboard or mouse HID node for the HID++ write is
// TCC-gated on Input Monitoring (SPEC §6.2).

package detect

const permissionRemediation = "grant this terminal Input Monitoring access (System Settings → Privacy & Security → Input Monitoring)"
