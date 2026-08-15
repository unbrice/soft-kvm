// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"os"
	"testing"
)

func TestParseVIDPID(t *testing.T) {
	vid, pid, err := parseVIDPID("046d:c548")
	if err != nil {
		t.Fatalf("parseVIDPID: %v", err)
	}
	if vid != 0x046d || pid != 0xc548 {
		t.Fatalf("got %04x:%04x, want 046d:c548", vid, pid)
	}

	if _, _, err := parseVIDPID("046d"); err == nil {
		t.Fatal("expected error for missing PID")
	}
	if _, _, err := parseVIDPID("046d:zzzz"); err == nil {
		t.Fatal("expected error for non-hex PID")
	}
}

func TestIORegHasDevice(t *testing.T) {
	present, err := os.ReadFile("testdata/ioreg_bolt_present.txt")
	if err != nil {
		t.Fatalf("read present fixture: %v", err)
	}
	absent, err := os.ReadFile("testdata/ioreg_bolt_absent.txt")
	if err != nil {
		t.Fatalf("read absent fixture: %v", err)
	}

	if !ioregHasDevice(present, 1133, 50504) {
		t.Fatal("expected Bolt present")
	}
	if ioregHasDevice(absent, 1133, 50504) {
		t.Fatal("expected Bolt absent")
	}
}

func TestIORegExactMatch(t *testing.T) {
	// The absent fixture contains a hub (1133/999) and a decoy with
	// idVendor=11330 / idProduct=50504. 1133 must not match 11330, and 999
	// must not match 50504.
	absent, err := os.ReadFile("testdata/ioreg_bolt_absent.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if ioregHasDevice(absent, 1133, 50504) {
		t.Fatal("expected no false positive from 11330/999 decoys")
	}
	if !ioregHasDevice(absent, 11330, 50504) {
		t.Fatal("expected decoy device to match its own VID/PID")
	}
}
