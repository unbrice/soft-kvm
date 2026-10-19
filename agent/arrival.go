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

// tracked is a device hold the watcher is polling for, and since when.
type tracked struct {
	hold  state.Hold
	since time.Time
}

// arrivalWatch polls every device whose hold names this host as holder, is not
// yet arrived, and has a local declaration — every ArrivalPoll, for ArrivalFor
// from first seeing that (key, seq) — and posts /arrived once the device is
// linked here. It decides nothing and never blocks the loop: states arrive on
// a latest-wins channel, and the poll runs on this goroutine (SPEC §4.3).
func (a *agent) arrivalWatch(ctx context.Context, states <-chan *state.ServerState) error {
	if a.cfg.HIDPresent == nil || len(a.hids) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	ticker := time.NewTicker(a.cfg.arrivalPollOrDefault())
	defer ticker.Stop()

	watching := map[state.HoldKey]tracked{}
	reported := map[state.HoldKey]state.Seq{} // arrivals posted, until the state shows them
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case s := <-states:
			a.track(watching, reported, s, time.Now())
		case <-ticker.C:
		}
		a.pollArrivals(ctx, watching, reported)
	}
}

// track updates what is being watched from a state: new device holds of this
// host start, and holds that arrived, moved on or went away stop.
func (a *agent) track(watching map[state.HoldKey]tracked, reported map[state.HoldKey]state.Seq, s *state.ServerState, now time.Time) {
	current := make(map[state.HoldKey]bool, len(s.Holds))
	for _, h := range s.Holds {
		key := h.Key()
		if h.Kind != state.KindHID || h.Holder != a.cfg.ID {
			continue
		}
		if _, ok := a.hids[h.Name]; !ok {
			continue
		}
		if h.Arrived {
			delete(reported, key)
			continue
		}
		if reported[key] == h.Seq {
			continue
		}
		current[key] = true
		if t, ok := watching[key]; !ok || t.hold.Seq != h.Seq {
			watching[key] = tracked{hold: h, since: now}
			slog.Debug("watching for arrival", "device", h.Name, "seq", h.Seq)
		}
	}
	for key := range watching {
		if !current[key] {
			delete(watching, key)
		}
	}
}

// pollArrivals checks each watched device once and reports those that are
// linked here. A device not here by the end of ArrivalFor is dropped: a later
// arrival leaves the hold un-arrived, which is harmless (SPEC §8).
func (a *agent) pollArrivals(ctx context.Context, watching map[state.HoldKey]tracked, reported map[state.HoldKey]state.Seq) {
	now := time.Now()
	var arrivals []state.Arrival
	for key, t := range watching {
		if now.Sub(t.since) > a.cfg.arrivalForOrDefault() {
			slog.Info("device did not arrive, no longer watching", "device", t.hold.Name, "seq", t.hold.Seq)
			delete(watching, key)
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, a.cfg.switchTimeoutOrDefault())
		here, err := a.cfg.HIDPresent(pctx, a.hids[t.hold.Name].Target)
		cancel()
		if err != nil {
			slog.Debug("arrival poll failed", "device", t.hold.Name, "error", err)
			continue
		}
		if here {
			arrivals = append(arrivals, state.Arrival{Kind: key.Kind, Name: key.Name, Seq: t.hold.Seq})
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
		key := state.HoldKey{Kind: ar.Kind, Name: ar.Name}
		reported[key] = ar.Seq
		delete(watching, key)
		slog.Info("device arrived", "device", ar.Name, "seq", ar.Seq)
	}
}
