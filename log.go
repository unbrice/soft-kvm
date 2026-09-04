// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// log.go: the terminal log format (SPEC §5).

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
)

// ANSI attributes for the level tag and the trailing key=value pairs.
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
)

// consoleHandler renders records for a person watching a terminal: a clock,
// a level tag, the message, then the attributes dimmed. The install
// walkthrough asks people to run serve and connect by hand, and logfmt with
// nanosecond timestamps is not readable there. Anything that is not a
// terminal — a pipe, or journald under systemd — keeps the TextHandler, so
// machine capture is unchanged.
type consoleHandler struct {
	w      io.Writer
	mu     *sync.Mutex
	level  slog.Leveler
	color  bool
	attrs  string // preformatted by WithAttrs
	groups []string
}

func newConsoleHandler(w io.Writer, level slog.Leveler, color bool) *consoleHandler {
	return &consoleHandler{w: w, mu: &sync.Mutex{}, level: level, color: color}
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	c := *h
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range attrs {
		c.appendAttr(&b, a)
	}
	c.attrs = b.String()
	return &c
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.groups = append(slices.Clip(c.groups), name)
	return &c
}

// appendAttr writes one " key=value" pair, dotted with any open groups. A
// group-valued attribute opens one more level, as the TextHandler does.
func (h *consoleHandler) appendAttr(b *strings.Builder, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		inner := *h
		inner.groups = append(slices.Clip(h.groups), a.Key)
		for _, g := range a.Value.Group() {
			inner.appendAttr(b, g)
		}
		return
	}
	key := strings.Join(append(slices.Clip(h.groups), a.Key), ".")
	fmt.Fprintf(b, " %s=%s", key, quoteValue(a.Value.String()))
}

// quoteValue quotes a value that would otherwise run into the next pair.
func quoteValue(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\"=") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// levelTag is a fixed-width level, coloured by severity so a warning or an
// error is findable in a scrolling terminal.
func (h *consoleHandler) levelTag(l slog.Level) string {
	tag := fmt.Sprintf("%-5s", l.String())
	if !h.color {
		return tag
	}
	switch {
	case l >= slog.LevelError:
		return ansiRed + tag + ansiReset
	case l >= slog.LevelWarn:
		return ansiYellow + tag + ansiReset
	case l < slog.LevelInfo:
		return ansiDim + tag + ansiReset
	default:
		return tag
	}
}

func (h *consoleHandler) dim(s string) string {
	if !h.color || s == "" {
		return s
	}
	return ansiDim + s + ansiReset
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	if !r.Time.IsZero() {
		b.WriteString(h.dim(r.Time.Format("15:04:05")))
		b.WriteByte(' ')
	}
	b.WriteString(h.levelTag(r.Level))
	b.WriteByte(' ')
	b.WriteString(r.Message)

	var attrs strings.Builder
	attrs.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		h.appendAttr(&attrs, a)
		return true
	})
	b.WriteString(h.dim(attrs.String()))
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}
