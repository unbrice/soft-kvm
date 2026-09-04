// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// detect_hidpp.go: the receiver-link detector and the --trigger parser that
// routes each entry to its detector (SPEC §6.1).
//
// A device paired through a Logitech receiver never produces a HID attach on
// Linux: hid-logitech-dj creates one child HID node per *paired slot*, and
// that node lives as long as the pairing does, not as long as the device is
// linked. Easy-Switch moves the radio link, not the pairing, so the node
// never appears or disappears. The receiver announces the link change
// instead — that is what this detector listens to.

package detect

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/unbrice/soft-kvm/hidpp"
	"golang.org/x/sync/errgroup"
)

// trigger is one --trigger entry: a HID device, optionally narrowed to a
// single pairing slot of a receiver.
type trigger struct {
	vidpid
	slot uint8 // 0 = the device itself; 1-6 = a receiver pairing slot
}

// parseTriggers parses a comma-separated list of "VID:PID" and
// "VID:PID:SLOT" entries. The address grammar is hidpp.ParseTarget, shared
// with hid-switch so a device is spelled the same way everywhere.
func parseTriggers(list string) ([]trigger, error) {
	var out []trigger
	for _, s := range strings.Split(list, ",") {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("empty --trigger entry")
		}
		vid, pid, slot, err := hidpp.ParseTarget(s)
		if err != nil {
			return nil, err
		}
		out = append(out, trigger{vidpid: vidpid{vid, pid}, slot: slot})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty --trigger list")
	}
	return out, nil
}

// receiverTarget is one receiver and the pairing slots watched on it.
type receiverTarget struct {
	vidpid
	slots []uint8
}

// Detector fans every configured trigger source into one attach channel: HID
// attach edges for plain VID:PID entries, receiver link notifications for
// VID:PID:SLOT ones. It satisfies agent.Detector.
type Detector struct {
	hidTargets []vidpid
	receivers  []receiverTarget
}

// NewDetector builds the detector for a --trigger list.
func NewDetector(list string) (*Detector, error) {
	triggers, err := parseTriggers(list)
	if err != nil {
		return nil, err
	}
	d := &Detector{}
	for _, t := range triggers {
		if t.slot == 0 {
			if !slices.Contains(d.hidTargets, t.vidpid) {
				d.hidTargets = append(d.hidTargets, t.vidpid)
			}
			continue
		}
		i := slices.IndexFunc(d.receivers, func(r receiverTarget) bool { return r.vidpid == t.vidpid })
		if i < 0 {
			d.receivers = append(d.receivers, receiverTarget{vidpid: t.vidpid})
			i = len(d.receivers) - 1
		}
		if !slices.Contains(d.receivers[i].slots, t.slot) {
			d.receivers[i].slots = append(d.receivers[i].slots, t.slot)
		}
	}
	return d, nil
}

// emitAttach queues an attach edge. Non-blocking and coalescing: an edge
// already queued makes this one redundant — attach edges are idempotent
// triggers (agent invariant 3).
func emitAttach(attach chan<- struct{}) {
	select {
	case attach <- struct{}{}:
	default:
	}
}

// Run starts every configured source and blocks until ctx is cancelled or a
// source fails. A failure returns: the agent's detectorLoop retries the whole
// detector on backoff, which is also how an unplugged receiver is recovered.
func (d *Detector) Run(ctx context.Context, attach chan<- struct{}) error {
	g, gctx := errgroup.WithContext(ctx)
	if len(d.hidTargets) > 0 {
		hd := &HIDDetector{targets: d.hidTargets}
		g.Go(func() error { return hd.Run(gctx, attach) })
	}
	for _, r := range d.receivers {
		g.Go(func() error { return watchReceiver(gctx, r, attach) })
	}
	if err := g.Wait(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// watchReceiver emits an attach edge whenever one of the watched slots takes
// the radio link on this receiver.
func watchReceiver(ctx context.Context, r receiverTarget, attach chan<- struct{}) error {
	// A slot already linked at startup counts as one attach edge, matching
	// the HID detector's snapshot rule. The probe closes the interface before
	// the watch opens it, so a link change in that gap is missed; the agent
	// reconciles against the server on connect regardless (SPEC §5.4.3).
	for _, slot := range r.slots {
		online, err := hidpp.SlotOnline(ctx, r.vid, r.pid, slot)
		if err != nil {
			return fmt.Errorf("receiver %04x:%04x slot %d: %w", r.vid, r.pid, slot, err)
		}
		if online {
			slog.Info("receiver slot linked at startup",
				"receiver", fmt.Sprintf("%04x:%04x", r.vid, r.pid), "slot", slot)
			emitAttach(attach)
			break
		}
	}

	events := make(chan hidpp.LinkEvent)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return hidpp.WatchLinks(gctx, r.vid, r.pid, events) })
	g.Go(func() error {
		for {
			select {
			case <-gctx.Done():
				return nil
			case ev := <-events:
				// Only the link coming up is a trigger. A drop is ambiguous
				// — the device may have gone to the other host, or to sleep
				// — and is never acted on (SPEC §6.3, finding 2).
				if !ev.Established || !slices.Contains(r.slots, ev.Slot) {
					continue
				}
				slog.Info("receiver slot took the link",
					"receiver", fmt.Sprintf("%04x:%04x", r.vid, r.pid), "slot", ev.Slot)
				emitAttach(attach)
			}
		}
	})
	if err := g.Wait(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// OwnChannelFunc returns a reader for the Easy-Switch channel this host
// occupies on the trigger peripheral, or nil when no trigger can answer
// HID++. The agent publishes the answer with its claim so the losing host
// knows where to send the peripherals it still holds (SPEC §5.5).
//
// A receiver slot is asked before a plain HID target, whatever the order in
// --trigger: a receiver always speaks HID++, while a bare VID:PID may be any
// keyboard. Either way the answer is the same — the channel is a property of
// this host, not of the device that reports it.
func (d *Detector) OwnChannelFunc() func(context.Context) (uint8, error) {
	switch {
	case len(d.receivers) > 0:
		r := d.receivers[0]
		slot := r.slots[0]
		return func(ctx context.Context) (uint8, error) {
			return hidpp.CurrentChannel(ctx, r.vid, r.pid, slot)
		}
	case len(d.hidTargets) > 0:
		t := d.hidTargets[0]
		return func(ctx context.Context) (uint8, error) {
			return hidpp.CurrentChannel(ctx, t.vid, t.pid, 0)
		}
	}
	return nil
}
