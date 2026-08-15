// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseUevent(t *testing.T) {
	cases := []struct {
		name    string
		wantOK  bool
		wantKey string
	}{
		{"bolt_add", true, "PRODUCT"},
		{"unifying_add", true, "PRODUCT"},
		{"usb_interface_add", true, "DEVTYPE"},
		{"truncated", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", c.name+".uevent"))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			props, ok := parseUevent(data)
			if ok != c.wantOK {
				t.Fatalf("parseUevent ok=%v, want %v", ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if c.wantKey != "" {
				if _, have := props[c.wantKey]; !have {
					t.Fatalf("missing key %q in %v", c.wantKey, props)
				}
			}
		})
	}

	t.Run("garbage", func(t *testing.T) {
		if _, ok := parseUevent([]byte("not a uevent")); ok {
			t.Fatal("expected garbage to be rejected")
		}
	})
}

func TestUSBUeventMatch(t *testing.T) {
	bolt := loadProps(t, "bolt_add")
	unifying := loadProps(t, "unifying_add")
	iface := loadProps(t, "usb_interface_add")

	cases := []struct {
		name  string
		props map[string]string
		vid   int
		pid   int
		want  bool
	}{
		{"bolt", bolt, 0x046d, 0xc548, true},
		{"unifying pid", unifying, 0x046d, 0xc548, false},
		{"interface devtype", iface, 0x046d, 0xc548, false},
		{"wrong vendor", bolt, 0x1234, 0xc548, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := usbUeventMatch(c.props, c.vid, c.pid)
			if got != c.want {
				t.Fatalf("usbUeventMatch=%v, want %v", got, c.want)
			}
		})
	}
}

func loadProps(t *testing.T, name string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".uevent"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	props, ok := parseUevent(data)
	if !ok {
		t.Fatalf("failed to parse %s", name)
	}
	return props
}
