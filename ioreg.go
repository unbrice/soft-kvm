// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// ioreg output parser. Constraint-free so the macOS detector can be unit-tested
// from Linux (SPEC §11.4).

package main

import (
	"fmt"
	"strings"
)

// ioregHasDevice reports whether out (the output of
// `ioreg -r -c IOUSBHostDevice -l`) contains a device with the given decimal
// vendor and product IDs (SPEC §6.2).
func ioregHasDevice(out []byte, vid, pid int) bool {
	var chunk strings.Builder
	match := false

	flush := func() {
		if !match && chunk.Len() > 0 && chunkMatch(chunk.String(), vid, pid) {
			match = true
		}
		chunk.Reset()
	}

	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "+-o") {
			flush()
		}
		chunk.WriteString(line)
		chunk.WriteByte('\n')
	}
	flush()
	return match
}

func chunkMatch(chunk string, vid, pid int) bool {
	return lineHasValue(chunk, "idVendor", vid) && lineHasValue(chunk, "idProduct", pid)
}

// lineHasValue reports whether chunk contains a line that assigns key the exact
// integer want, avoiding substring traps such as 1133 matching 11330.
func lineHasValue(chunk, key string, want int) bool {
	needle := fmt.Sprintf(`"%s" = `, key)
	for {
		idx := strings.Index(chunk, needle)
		if idx < 0 {
			return false
		}
		rest := chunk[idx+len(needle):]
		n, i := 0, 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			n = n*10 + int(rest[i]-'0')
			i++
		}
		if i > 0 && n == want {
			return true
		}
		chunk = rest
	}
}
