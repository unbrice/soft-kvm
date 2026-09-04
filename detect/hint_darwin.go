// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// hint_darwin.go: the permission remediation for macOS. Enumeration needs no
// grant, but opening a keyboard or mouse HID node for the HID++ write is
// TCC-gated on Input Monitoring (SPEC §6.2).

package detect

// PermissionRemediation is the fix to print when opening a HID node is
// refused. detect prints it on a failed HID++ scan, mac-debug when its open
// is refused.
const PermissionRemediation = "grant this terminal Input Monitoring access (System Settings → Privacy & Security → Input Monitoring)"
