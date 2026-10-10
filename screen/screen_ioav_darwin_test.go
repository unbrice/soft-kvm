// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_ioav_darwin_test.go: the IOKit bindings resolve (SPEC §5.6). Binding
// is dlopen and dlsym only: no display is opened and nothing reaches the bus.

package screen

import "testing"

// A bad symbol name or func signature only shows when the first switch runs:
// purego.RegisterFunc panics, and sync.OnceValue re-panics on every call.
func TestLoadIOKit(t *testing.T) {
	if err := loadIOKit(); err != nil {
		t.Fatalf("loadIOKit: %v", err)
	}
}
