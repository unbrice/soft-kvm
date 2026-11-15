// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// worker.go: the worker that carries out decisions — commands, assignments and
// the notification (SPEC §4.3, §5.5, §5.6, §11.1).

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/model"
	"github.com/unbrice/soft-kvm/screen"
	"github.com/unbrice/soft-kvm/state"
	"golang.org/x/sync/errgroup"
)

// errSwitchTimeout names the class of failure where --switch-timeout kills a
// hung command or write. The decider counts it like any failure, but the log
// line must distinguish a hung child from a spontaneous ddcutil death.
var errSwitchTimeout = errors.New("switch timed out")

// hasWork reports whether d is for the worker: commands, assignments or a
// notification.
func hasWork(d model.Decision) bool {
	return len(d.Run) > 0 || len(d.GiveAway) > 0 || d.Notify
}

// maxQueuedWrites bounds one hardware queue's channel: the server caps takes
// at 16 and the decider has at most one issue per assignment key in flight,
// so routing an assignment to its queue never blocks workerLoop.
const maxQueuedWrites = 16

// workerLoop routes decisions to channel-fed workers — one for command batches,
// one for notifications, and one per hardware write queue — so independent
// writes run and report in parallel while writes sharing a receiver run in
// order. Every child goroutine belongs to workerLoop's own errgroup.
func (a *agent) workerLoop(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	cmdsCh := make(chan []model.Command, 1)
	notifyCh := make(chan struct{}, 1)
	queues := make(map[string]chan state.Assignment)

	g.Go(func() error {
		for cmds := range cmdsCh {
			if ctx.Err() != nil {
				return nil
			}
			a.runCommands(ctx, cmds)
		}
		return nil
	})
	g.Go(func() error {
		for range notifyCh {
			if ctx.Err() != nil {
				return nil
			}
			if err := a.cfg.Runner(ctx, a.cfg.NotifyArgv); err != nil {
				slog.Error("notify command failed", "error", err)
			}
		}
		return nil
	})

	defer func() {
		close(cmdsCh)
		close(notifyCh)
		for _, ch := range queues {
			close(ch)
		}
		_ = g.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d := <-a.decisions:
			if len(d.Run) > 0 {
				select {
				case cmdsCh <- d.Run:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			for _, h := range d.GiveAway {
				a.enqueueGiveAway(ctx, g, queues, h)
			}
			if d.Notify {
				select {
				case notifyCh <- struct{}{}:
				default:
				}
			}
		}
	}
}

// post reports a result to the loop as an ordinary observation.
func (a *agent) post(ctx context.Context, o model.Observation) {
	select {
	case a.results <- o:
	case <-ctx.Done():
	}
}

// bounded runs fn under --switch-timeout and distinguishes a hung child from a
// spontaneous failure.
func (a *agent) bounded(ctx context.Context, fn func(context.Context) error) error {
	sctx, cancel := context.WithTimeout(ctx, a.cfg.switchTimeoutOrDefault())
	err := fn(sctx)
	cancel()
	if errors.Is(sctx.Err(), context.DeadlineExceeded) {
		if err == nil {
			return errSwitchTimeout
		}
		return fmt.Errorf("%w: %w", errSwitchTimeout, err)
	}
	return err
}

// hidTakeProbeTimeout bounds the presence check take-hid runs before posting
// takes: if a device is slow to answer, take-hid posts arrived=false rather
// than holding back take-screen, and the arrival watcher reports it once it
// links.
const hidTakeProbeTimeout = 1500 * time.Millisecond

// runCommands runs commands in parallel, each with its own timeout, and
// reports one result per command, in the order given, once all are over.
// Built-in takes (take-screen and take-hid) issued in the same batch are
// coalesced into a single POST /take/<id> so screen and device assignments
// land in one server epoch. One failure does not stop the rest: the commands
// target independent things, and a user who needs an order gives one command
// that runs them in it.
func (a *agent) runCommands(ctx context.Context, cmds []model.Command) {
	results := make([]error, len(cmds))
	var takeIndices []int
	var activation int64
	var wantScreen, wantHID bool
	var wg sync.WaitGroup
	for i, c := range cmds {
		switch {
		case c.TakeScreen != 0:
			takeIndices = append(takeIndices, i)
			activation = c.TakeScreen
			wantScreen = true
		case c.TakeHID != 0:
			takeIndices = append(takeIndices, i)
			activation = c.TakeHID
			wantHID = true
		default:
			wg.Go(func() {
				results[i] = a.bounded(ctx, func(ctx context.Context) error { return a.cfg.Runner(ctx, c.Argv) })
				if results[i] != nil {
					slog.Warn("command failed", "command", c.String(), "error", results[i])
				}
			})
		}
	}
	if len(takeIndices) > 0 {
		wg.Go(func() {
			err := a.bounded(ctx, func(ctx context.Context) error {
				var takes []state.Take
				if wantScreen {
					takes = append(takes, a.screenTakes()...)
				}
				if wantHID {
					takes = append(takes, a.hidTakes(ctx)...)
				}
				return a.take(ctx, activation, takes)
			})
			for _, i := range takeIndices {
				results[i] = err
				if err != nil {
					slog.Warn("command failed", "command", cmds[i].String(), "error", err)
				}
			}
		})
	}
	wg.Wait()
	a.post(ctx, model.Observation{Ran: &model.CommandsRun{Results: results}})
}

// screenTakes is what take-screen posts: for each declaration, the write that
// selects this host's input (SPEC §5.6).
func (a *agent) screenTakes() []state.Take {
	takes := make([]state.Take, len(a.cfg.Screens))
	for i, d := range a.cfg.Screens {
		takes[i] = state.Take{Kind: state.KindScreen, Name: d.Name, Input: d.Write.String()}
	}
	return takes
}

// hidTakes is what take-hid posts: for each declaration with a channel, the
// channel this host occupies and whether the device is linked here already. A
// presence check that fails or times out reads as absent — the arrival watcher
// reports it later (SPEC §4.3).
func (a *agent) hidTakes(ctx context.Context) []state.Take {
	var takes []state.Take
	for _, d := range a.cfg.HIDs {
		if d.Channel == 0 {
			continue
		}
		arrived := false
		if a.cfg.HIDPresent != nil {
			pctx, cancel := context.WithTimeout(ctx, hidTakeProbeTimeout)
			var err error
			arrived, err = a.cfg.HIDPresent(pctx, d.Target)
			cancel()
			if err != nil {
				slog.Warn("could not tell whether the device is here", "device", d.Name, "error", err)
				arrived = false
			}
		}
		takes = append(takes, state.Take{Kind: state.KindHID, Name: d.Name, Channel: d.Channel, Arrived: arrived})
	}
	return takes
}

// take posts takes to the server under activation. A 409 is a failure like
// any other: this host stopped being the owner while its commands ran.
func (a *agent) take(ctx context.Context, activation int64, takes []state.Take) error {
	return a.eachServer(ctx, func(ctx context.Context, base string) error {
		return a.cfg.Client.Take(ctx, base, a.cfg.ID, activation, takes)
	})
}

// eachServer calls fn against the candidates the resolver yields, the way a
// claim does, until one answers. An answer that is a refusal ends it: another
// candidate would be the same server.
func (a *agent) eachServer(ctx context.Context, fn func(ctx context.Context, base string) error) error {
	var last error
	for base := range a.cfg.Resolver.Resolve(ctx, a.cfg.ExplicitServer, a.cfg.KeyFP) {
		err := fn(ctx, base)
		if err == nil {
			a.cfg.Resolver.Save(base)
			return nil
		}
		if errors.Is(err, client.ErrNotOwner) {
			return err
		}
		slog.Debug("candidate failed", "base", base, "error", err)
		last = err
	}
	if last == nil {
		last = errors.New("no server candidate answered")
	}
	return last
}

func (a *agent) enqueueGiveAway(ctx context.Context, g *errgroup.Group, queues map[string]chan state.Assignment, h state.Assignment) {
	q := a.writeQueue(h)
	ch, ok := queues[q]
	if !ok {
		ch = make(chan state.Assignment, maxQueuedWrites)
		queues[q] = ch
		g.Go(func() error {
			for h := range ch {
				if ctx.Err() != nil {
					return nil
				}
				err := a.giveAwayAssignment(ctx, h)
				a.post(ctx, model.Observation{
					GaveAway: &model.AssignmentGivenAway{Key: h.Key(), Err: err},
				})
			}
			return nil
		})
	}
	select {
	case ch <- h:
	case <-ctx.Done():
	}
}

// writeQueue names the hardware an assignment's write goes through. Devices
// behind one receiver share its node, and the HID++ replies on it are matched
// to their request, so their sends must not overlap; each monitor has its own
// DDC/CI link.
func (a *agent) writeQueue(h state.Assignment) string {
	if d, ok := a.hids[h.Name]; ok && h.Kind == state.KindHID {
		return fmt.Sprintf("receiver %04x:%04x", d.Target.VID, d.Target.PID)
	}
	return h.Key().String()
}

// giveAwayAssignment moves one thing on this host to its assignee: a screen is
// written to the monitor of that name, a device is sent to the assignee's
// channel. A host with nothing of that name, or the device not linked here, has
// nothing to move and returns model.ErrNotHere (SPEC §4.3).
func (a *agent) giveAwayAssignment(ctx context.Context, h state.Assignment) error {
	start := time.Now()
	var err error
	switch h.Kind {
	case state.KindScreen:
		err = a.giveScreen(ctx, h)
	case state.KindHID:
		err = a.giveDevice(ctx, h)
	default:
		err = fmt.Errorf("unknown kind %q", h.Kind)
	}
	switch {
	case errors.Is(err, model.ErrNotHere):
		slog.Info("nothing to move here", "assignment", h.Key().String())
	case err != nil:
		slog.Warn("giving away failed", "assignment", h.Key().String(), "error", err)
	default:
		slog.Info("given away", "assignment", h.Key().String(), "assignee", h.Assignee, "took", time.Since(start).Round(time.Millisecond))
	}
	return err
}

func (a *agent) giveScreen(ctx context.Context, h state.Assignment) error {
	w, err := screen.ParseWrite(h.Input)
	if err != nil {
		return err
	}
	if a.cfg.WriteScreen == nil {
		return errors.New("no screen backend configured")
	}
	err = a.bounded(ctx, func(ctx context.Context) error { return a.cfg.WriteScreen(ctx, w, h.Name) })
	if errors.Is(err, screen.ErrNoDisplay) {
		return model.ErrNotHere
	}
	return err
}

func (a *agent) giveDevice(ctx context.Context, h state.Assignment) error {
	d, ok := a.hids[h.Name]
	if !ok {
		return model.ErrNotHere
	}
	if a.cfg.SendHID == nil {
		return errors.New("no device backend configured")
	}
	var sent bool
	err := a.bounded(ctx, func(ctx context.Context) error {
		var err error
		sent, err = a.cfg.SendHID(ctx, d.Target, h.Channel)
		return err
	})
	if err == nil && !sent {
		return model.ErrNotHere
	}
	return err
}
