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
	"sync"

	"github.com/unbrice/soft-kvm/hidpp"
	"golang.org/x/sync/errgroup"
)

// trigger is one --trigger entry, resolved: a HID device, optionally narrowed
// to a single pairing slot of a receiver.
type trigger struct {
	vidpid
	slot uint8 // 0 = the device itself; 1-6 = a receiver pairing slot
}

// parseTriggers resolves a comma-separated list of names against the devices
// --declare-hid declared: an address is spelled once, in its declaration.
func parseTriggers(list string, named map[string]hidpp.Target) ([]trigger, error) {
	var out []trigger
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("empty --trigger entry")
		}
		t, ok := named[s]
		if !ok {
			return nil, fmt.Errorf("--trigger %q: no --declare-hid has that name", s)
		}
		out = append(out, trigger{vidpid: vidpid{t.VID, t.PID}, slot: t.Slot})
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

// NewDetector builds the detector for a --trigger list of names. named holds
// the devices --declare-hid declared.
func NewDetector(list string, named map[string]hidpp.Target) (*Detector, error) {
	triggers, err := parseTriggers(list, named)
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

// sourceReport is one update from a trigger source (the HID watcher or a
// receiver watcher) to Detector's fan-in.
type sourceReport struct {
	source  int
	initial bool
	edge    bool
	present bool
}

// fanIn merges startup snapshots and live presence/edge updates from n trigger
// sources into the agent's attach channel:
//   - Once every source has reported its initial snapshot, it emits one bool:
//     true if any source had a target present at startup, false otherwise.
//   - Thereafter, it emits true on any target attach edge, and false when the
//     last present source becomes absent.
type fanIn struct {
	seen    []bool
	present []bool
	ready   bool
	hadEdge bool
}

func newFanIn(n int) *fanIn {
	return &fanIn{
		seen:    make([]bool, n),
		present: make([]bool, n),
	}
}

func (f *fanIn) update(r sourceReport) (emit bool, attached bool) {
	f.seen[r.source] = true
	f.present[r.source] = r.present
	if !f.ready {
		if r.edge {
			f.hadEdge = true
		}
		for _, s := range f.seen {
			if !s {
				return false, false
			}
		}
		f.ready = true
		return true, f.anyPresent() || f.hadEdge
	}
	if r.edge {
		return true, true
	}
	if !f.anyPresent() {
		return true, false
	}
	return false, false
}

func (f *fanIn) anyPresent() bool {
	for _, p := range f.present {
		if p {
			return true
		}
	}
	return false
}

// Run starts every configured source and blocks until ctx is cancelled or a
// source fails. A failure returns: the agent's detectorLoop retries the whole
// detector on backoff, which is also how an unplugged receiver is recovered.
func (d *Detector) Run(ctx context.Context, attach chan<- bool) error {
	n := len(d.receivers)
	if len(d.hidTargets) > 0 {
		n++
	}
	if n == 0 {
		select {
		case attach <- false:
		case <-ctx.Done():
			return nil
		}
		<-ctx.Done()
		return nil
	}

	reports := make(chan sourceReport, n*2)
	var sourcesWG sync.WaitGroup
	g, gctx := errgroup.WithContext(ctx)

	nextID := 0
	if len(d.hidTargets) > 0 {
		id := nextID
		nextID++
		hd := &HIDDetector{targets: d.hidTargets}
		sourcesWG.Add(1)
		g.Go(func() error {
			defer sourcesWG.Done()
			return hd.run(gctx, id, reports)
		})
	}
	for _, r := range d.receivers {
		id := nextID
		nextID++
		sourcesWG.Add(1)
		g.Go(func() error {
			defer sourcesWG.Done()
			return watchReceiver(gctx, id, r, reports)
		})
	}
	go func() {
		sourcesWG.Wait()
		close(reports)
	}()

	fi := newFanIn(n)
	for r := range reports {
		if emit, attached := fi.update(r); emit {
			select {
			case attach <- attached:
			case <-gctx.Done():
			}
		}
	}

	if err := g.Wait(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// watchReceiver reports the startup link state of the watched slots on this
// receiver, each subsequent link-established edge, and transitions where all
// watched slots on the receiver have lost the link.
func watchReceiver(ctx context.Context, id int, r receiverTarget, reports chan<- sourceReport) error {
	reported := false
	defer func() {
		if !reported {
			reports <- sourceReport{source: id, initial: true}
		}
	}()

	// A slot already linked at startup counts as one attach edge, matching
	// the HID detector's snapshot rule. All watched slots are probed so linked
	// tracks the full set before WatchLinks begins.
	linked := make(map[uint8]bool, len(r.slots))
	for _, slot := range r.slots {
		online, err := hidpp.SlotOnline(ctx, r.vid, r.pid, slot)
		if err != nil {
			return fmt.Errorf("receiver %04x:%04x slot %d: %w", r.vid, r.pid, slot, err)
		}
		if online {
			slog.Info("receiver slot linked at startup",
				"receiver", fmt.Sprintf("%04x:%04x", r.vid, r.pid), "slot", slot)
			linked[slot] = true
		}
	}
	reported = true
	reports <- sourceReport{source: id, initial: true, edge: len(linked) > 0, present: len(linked) > 0}

	events := make(chan hidpp.LinkEvent)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return hidpp.WatchLinks(gctx, r.vid, r.pid, events) })
	g.Go(func() error {
		for {
			select {
			case <-gctx.Done():
				return nil
			case ev := <-events:
				if !slices.Contains(r.slots, ev.Slot) {
					continue
				}
				if ev.Established {
					linked[ev.Slot] = true
					slog.Info("receiver slot took the link",
						"receiver", fmt.Sprintf("%04x:%04x", r.vid, r.pid), "slot", ev.Slot)
					reports <- sourceReport{source: id, edge: true, present: true}
					continue
				}
				wasPresent := len(linked) > 0
				delete(linked, ev.Slot)
				if wasPresent && len(linked) == 0 {
					reports <- sourceReport{source: id, present: false}
				}
			}
		}
	})
	if err := g.Wait(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
