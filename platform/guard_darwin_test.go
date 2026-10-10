// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// guard_darwin_test.go: the macOS guard's display-presence check (SPEC §6.2),
// against a fake display list.

package platform

import (
	"errors"
	"testing"
)

func TestDisplayPresent(t *testing.T) {
	names := func(ns ...string) func() ([]string, error) {
		return func() ([]string, error) { return ns, nil }
	}
	cases := []struct {
		name     string
		match    string
		displays func() ([]string, error)
		want     bool
		wantErr  bool
	}{
		{name: "any external", displays: names("LG HDR 5K"), want: true},
		{name: "no external display", displays: names(), want: false},
		{name: "unnamed counts as any", displays: names(""), want: true},
		{name: "match is case-insensitive", match: "lg hdr", displays: names("Dell U2720Q", "LG HDR 5K"), want: true},
		{name: "no match", match: "dell", displays: names("LG HDR 5K"), want: false},
		{name: "lister fails", displays: func() ([]string, error) { return nil, errors.New("IOKit") }, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NewGuard(c.match, false, c.displays).displayPresent()
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("present = %v, want %v", got, c.want)
			}
		})
	}
}
