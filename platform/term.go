// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// term.go: is this writer a person's terminal? (SPEC §5)

package platform

import (
	"io"
	"os"
)

// IsTerminal reports whether w is a character device — someone's terminal —
// rather than a pipe, a file or a buffer. Asking the writer rather than
// os.Stdout is what keeps callers honest: a redirect, a systemd journal and
// a test's bytes.Buffer all answer false without the caller saying so.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// WantsColor reports whether w is a terminal that also wants escape codes:
// NO_COLOR unset and TERM not "dumb". Format and colour are separate
// questions — NO_COLOR asks for plain text, not for machine-readable text —
// so callers that only change layout use IsTerminal.
func WantsColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTerminal(w)
}
