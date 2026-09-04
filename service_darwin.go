// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// service_darwin.go: the service subcommand is Linux-only (SPEC §6.2).

package main

import (
	"context"
	"errors"
)

func serviceCmd(_ context.Context) error {
	return errors.New("service management is only supported on Linux (systemd); for macOS launchd, see SPEC §6.2")
}
