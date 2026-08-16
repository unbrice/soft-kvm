// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"
)

func TestRequireToken(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Setenv("SOFTKVM_TOKEN", "")
		if _, err := requireToken(); !errors.Is(err, errToken) {
			t.Fatalf("got %v, want errToken", err)
		}
	})

	t.Run("too short", func(t *testing.T) {
		t.Setenv("SOFTKVM_TOKEN", strings.Repeat("a", tokenMinLen-1))
		if _, err := requireToken(); !errors.Is(err, errToken) {
			t.Fatalf("got %v, want errToken", err)
		}
	})

	t.Run("minimum accepted", func(t *testing.T) {
		want := strings.Repeat("a", tokenMinLen)
		t.Setenv("SOFTKVM_TOKEN", want)
		got, err := requireToken()
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q, nil", got, err, want)
		}
	})

	t.Run("long accepted", func(t *testing.T) {
		want := strings.Repeat("a", tokenGoodLen)
		t.Setenv("SOFTKVM_TOKEN", want)
		got, err := requireToken()
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q, nil", got, err, want)
		}
	})
}
