// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestBTConnectEvent(t *testing.T) {
	mac := "AA:BB:CC:DD:EE:FF"
	path := "dev_AA_BB_CC_DD_EE_FF"

	pos := "/org/bluez/hci0/" + path + ": PropertiesChanged {'Connected': <true>}"
	if !btConnectEvent(pos, mac) {
		t.Fatal("expected positive connect event")
	}

	cases := []struct {
		name string
		line string
	}{
		{"false", "/org/bluez/hci0/" + path + ": PropertiesChanged {'Connected': <false>}"},
		{"other mac", "/org/bluez/hci0/dev_00_11_22_33_44_55: PropertiesChanged {'Connected': <true>}"},
		{"other property", "/org/bluez/hci0/" + path + ": PropertiesChanged {'Name': <true>}"},
		{"no path", "PropertiesChanged {'Connected': <true>}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if btConnectEvent(c.line, mac) {
				t.Fatalf("unexpected match for %q", c.line)
			}
		})
	}

	t.Run("case insensitive mac input", func(t *testing.T) {
		if !btConnectEvent(pos, strings.ToLower(mac)) {
			t.Fatal("expected normalized MAC to match")
		}
	})
}
