// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Linux guard: the always-on desktop has no guards (SPEC §6.2).

package platform

import "context"

// Guard is the Linux no-op guard.
type Guard struct{}

// NewGuard returns a Linux guard; match and disabled are ignored.
func NewGuard(match string, disabled bool) *Guard { return &Guard{} }

// OK always reports that switching is allowed.
func (*Guard) OK(context.Context) (bool, string) { return true, "no guards" }
