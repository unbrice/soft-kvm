// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// watcher.go: the /state + /wait watcher — resolve/backoff outer layer and
// one-session long-poll inner layer (SPEC §5.4.3, §5.4.5).

package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/state"
)

const (
	// stateTimeout bounds GET /state when resolving or re-validating a server.
	stateTimeout = 5 * time.Second
	// waitTimeout bounds one GET /wait long-poll. It must be larger than the
	// client's internal timeout so the server fires first.
	waitTimeout = 65 * time.Second
)

// errSleepDetected signals that the wall clock jumped far enough during a
// long-poll that the machine probably slept. The caller re-resolves
// immediately without advancing the connection backoff.
var errSleepDetected = errors.New("sleep detected")

// watch resolves the server and pumps fresh states into stateCh until ctx is
// cancelled. Transient failures wait on the backoff before re-resolving; a
// sleep detection skips the backoff.
func (a *agent) watch(ctx context.Context, stateCh chan<- *state.ServerState) error {
	b := a.newBackoff()
	for {
		base, st, ok := a.connectServer(ctx)
		if !ok {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("watcher: no server candidate answered")
			if err := b.wait(ctx); err != nil {
				return err
			}
			continue
		}
		// A successful /state proves the coordinator is reachable; only now
		// does the backoff reset (SPEC §5.4.3).
		b.reset()
		a.cfg.Resolver.Save(base)
		if !a.sendState(ctx, stateCh, st) {
			return ctx.Err()
		}
		if err := a.poll(ctx, stateCh, base, st.Epoch, b); err != nil {
			if errors.Is(err, errSleepDetected) {
				continue
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ctx.Err()
			}
			// Any other error waits the backoff before re-resolving.
			if werr := b.wait(ctx); werr != nil {
				return werr
			}
		}
	}
}

// poll runs one long-poll session against base, sending every wake to stateCh.
// It returns errSleepDetected when the wall jump implies a sleep, or any error
// that should tear down this session. Cancellation returns ctx.Err().
func (a *agent) poll(ctx context.Context, stateCh chan<- *state.ServerState, base string, epoch int64, b *backoff) error {
	for {
		// Wall-only start so sleep detection compares wall-clock progress.
		start := time.Now().Round(0)
		woke, err := a.clientWait(ctx, base, epoch)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if errors.Is(err, client.ErrUnauthorized) {
				slog.Error("watcher: token rejected")
			} else {
				slog.Error("watcher: wait failed", "error", err)
			}
			return err
		}
		if time.Since(start) > 2*client.WaitClientTimeout {
			slog.Info("watcher: sleep detected, re-resolving")
			return errSleepDetected
		}
		if !woke {
			continue
		}
		st, err := a.clientState(ctx, base)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return ctx.Err()
			}
			if errors.Is(err, client.ErrUnauthorized) {
				slog.Error("watcher: token rejected")
			} else {
				slog.Error("watcher: state failed", "error", err)
			}
			return err
		}
		b.reset()
		a.cfg.Resolver.Save(base)
		if !a.sendState(ctx, stateCh, st) {
			return ctx.Err()
		}
		epoch = st.Epoch
	}
}

// connectServer ranges over Resolver.Resolve's candidates and returns the base
// and state of the first whose /state answers. ok is false when the sequence
// ended without a working candidate — for explicit/env sources after one
// candidate, for mDNS only when ctx is done (SPEC §5.1, §8).
func (a *agent) connectServer(ctx context.Context) (base string, st *state.ServerState, ok bool) {
	for base := range a.cfg.Resolver.Resolve(ctx, a.cfg.ExplicitServer, a.cfg.KeyFP) {
		st, err := a.clientState(ctx, base)
		if err == nil {
			return base, st, true
		}
		if errors.Is(err, context.Canceled) {
			return "", nil, false
		}
		if errors.Is(err, client.ErrUnauthorized) {
			slog.Error("watcher: token rejected")
		} else {
			slog.Debug("watcher: candidate failed", "base", base, "error", err)
		}
	}
	return "", nil, false
}

func (a *agent) clientState(ctx context.Context, base string) (*state.ServerState, error) {
	cctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	return a.cfg.Client.State(cctx, base)
}

func (a *agent) clientWait(ctx context.Context, base string, epoch int64) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	return a.cfg.Client.Wait(cctx, base, epoch, a.cfg.ID)
}

func (a *agent) sendState(ctx context.Context, stateCh chan<- *state.ServerState, st *state.ServerState) bool {
	select {
	case stateCh <- st:
		return true
	case <-ctx.Done():
		return false
	}
}
