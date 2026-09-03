// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// shadow_darwin.go: kernelShadow for macOS. There are no kernel receiver
// shadows on macOS: Bluetooth HID++ peripherals are real HID nodes.

package detect

func kernelShadow(string) bool { return false }
