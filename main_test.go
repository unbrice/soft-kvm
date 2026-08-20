// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"slices"
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

func TestSplitSwitchCommands(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    [][]string
		wantErr bool
	}{
		{name: "empty", args: nil, want: nil},
		{name: "one", args: []string{"ddcutil", "setvcp", "0xF4"}, want: [][]string{{"ddcutil", "setvcp", "0xF4"}}},
		{
			name: "two",
			args: []string{"ddcutil", "setvcp", "0xF4", "--", "usb-switch", "2"},
			want: [][]string{{"ddcutil", "setvcp", "0xF4"}, {"usb-switch", "2"}},
		},
		{name: "lone separator", args: []string{"--", "cmd"}, wantErr: true},
		{name: "trailing separator", args: []string{"cmd", "--"}, wantErr: true},
		{name: "empty middle", args: []string{"a", "--", "--", "b"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitSwitchCommands(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %v, nil; want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("got error %v", err)
			}
			if !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
