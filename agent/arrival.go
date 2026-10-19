// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// arrival.go: the arrival watcher — polls the devices this host took and
// reports the ones that reached it (SPEC §4.3, §5.4).

package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/unbrice/soft-kvm/state"
)

// tracked is a device assignment the watcher is polling for, and since when.
type tracked struct {
	assignment state.Assignment
	since      time.Time
}

// arrivals is the arrival watcher's state: the devices it polls for, and the
// arrivals it posted that no state shows yet. Only the watcher touches it.
type arrivals struct {
	a        *agent
	watching map[state.AssignmentKey]tracked
	reported map[state.AssignmentKey]state.Seq
}

// arrivalWatch polls every device whose assignment names this host as assignee,
// is not yet arrived, and has a local declaration — every ArrivalPoll, for
// ArrivalFor from first seeing that (key, seq) — and posts /arrived once the
// device is linked here. It decides nothing and never blocks the loop: states
// arrive on a latest-wins channel, and the poll runs on this goroutine (SPEC
// §4.3).
func (a *agent) arrivalWatch(ctx context.Context, states <-chan *state.ServerState) error {
	if a.cfg.HIDPresent == nil || len(a.hids) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	ticker := time.NewTicker(a.cfg.arrivalPollOrDefault())
	defer ticker.Stop()

	w := &arrivals{a: a, watching: map[state.AssignmentKey]tracked{}, reported: map[state.AssignmentKey]state.Seq{}}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case s := <-states:
			w.track(time.Now(), s)
		case <-ticker.C:
		}
		w.poll(ctx)
	}
}

// track updates what is being watched from a state: new device assignments of
// this host start, and assignments that arrived, moved on or went away stop.
func (w *arrivals) track(now time.Time, s *state.ServerState) {
	a := w.a
	current := make(map[state.AssignmentKey]bool, len(s.Assignments))
	for _, h := range s.Assignments {
		key := h.Key()
		if h.Kind != state.KindHID || h.Assignee != a.cfg.ID {
			continue
		}
		if _, ok := a.hids[h.Name]; !ok {
			continue
		}
		if h.Arrived {
			delete(w.reported, key)
			continue
		}
		if w.reported[key] == h.Seq {
			continue
		}
		current[key] = true
		if t, ok := w.watching[key]; !ok || t.assignment.Seq != h.Seq {
			w.watching[key] = tracked{assignment: h, since: now}
			slog.Debug("watching for arrival", "device", h.Name, "seq", h.Seq)
		}
	}
	for key := range w.watching {
		if !current[key] {
			delete(w.watching, key)
		}
	}
}

// poll checks each watched device once and reports those that are
// linked here. A device not here by the end of ArrivalFor is dropped: a later
// arrival leaves the assignment un-arrived, which is harmless (SPEC §8).
func (w *arrivals) poll(ctx context.Context) {
	a := w.a
	now := time.Now()
	var arrivals []state.Arrival
	for key, t := range w.watching {
		if now.Sub(t.since) > a.cfg.arrivalForOrDefault() {
			slog.Info("device did not arrive, no longer watching", "device", t.assignment.Name, "seq", t.assignment.Seq)
			delete(w.watching, key)
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, a.cfg.switchTimeoutOrDefault())
		here, err := a.cfg.HIDPresent(pctx, a.hids[t.assignment.Name].Target)
		cancel()
		if err != nil {
			slog.Debug("arrival poll failed", "device", t.assignment.Name, "error", err)
			continue
		}
		if here {
			arrivals = append(arrivals, state.Arrival{Kind: key.Kind, Name: key.Name, Seq: t.assignment.Seq})
		}
	}
	if len(arrivals) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	err := a.eachServer(rctx, func(ctx context.Context, base string) error {
		return a.cfg.Client.Arrived(ctx, base, a.cfg.ID, arrivals...)
	})
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("could not report arrival; will retry on the next poll", "error", err)
		}
		return
	}
	for _, ar := range arrivals {
		key := state.AssignmentKey{Kind: ar.Kind, Name: ar.Name}
		w.reported[key] = ar.Seq
		delete(w.watching, key)
		slog.Info("device arrived", "device", ar.Name, "seq", ar.Seq)
	}
}
