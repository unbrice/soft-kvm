// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// server.go: the coordinator HTTP service (SPEC §6.4, §7, §11.1).

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/unbrice/soft-kvm/hidpp"
	"github.com/unbrice/soft-kvm/identity"
	"github.com/unbrice/soft-kvm/state"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// DefaultWaitTimeout is how long /wait blocks before returning 204. It must be
// smaller than the client's internal /wait timeout.
const DefaultWaitTimeout = 50 * time.Second

// Server carries the display bit and serves the /claim /state /wait API.
type Server struct {
	mu           sync.Mutex
	token        string
	statePath    string
	owner        string
	epoch        int64
	since        time.Time
	ownerChannel uint8
	serverID     string
	waiters      map[string]int
	broadcast    chan struct{}
	waitTimeout  time.Duration
}

// NewServer loads the persisted ownerState from statePath. A missing file is
// a fresh server (owner="", epoch=0); a corrupt one logs a warning and starts
// fresh the same way (SPEC §6.4).
func NewServer(statePath, token string) *Server {
	var persisted state.OwnerState
	if err := state.Load(statePath, &persisted); err != nil {
		slog.Warn("corrupt state file, starting fresh", "path", statePath, "error", err)
		persisted = state.OwnerState{}
	}

	s := &Server{
		token:        token,
		statePath:    statePath,
		owner:        persisted.Owner,
		epoch:        persisted.Epoch,
		since:        persisted.Since,
		ownerChannel: persisted.OwnerChannel,
		serverID:     newUUID(),
		waiters:      make(map[string]int),
		broadcast:    make(chan struct{}),
		waitTimeout:  DefaultWaitTimeout,
	}
	return s
}

// SetWaitTimeout configures the long-poll timeout. Exported for tests.
func (s *Server) SetWaitTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waitTimeout = d
}

// SetWaiterCount is a test hook that makes id appear live with the given
// waiter count.
func (s *Server) SetWaiterCount(id string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 {
		delete(s.waiters, id)
		return
	}
	s.waiters[id] = n
}

// WaiterCount returns the number of registered waiters for id. Exported for
// tests.
func (s *Server) WaiterCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiters[id]
}

// newUUID returns a random UUIDv4 as a hyphenated hex string.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read only fails on serious system errors; fall back to a
		// random-looking value derived from time so the server can still start.
		slog.Error("crypto/rand.Read failed", "error", err)
		now := time.Now().UnixNano()
		for i := 0; i < 16; i++ {
			b[i] = byte(now >> (i * 4))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// Handler returns the http.Handler for the service endpoints. Authentication is
// mutual TLS: the server only accepts a client certificate signed by the
// secret-derived CA, and clients pin the secret-derived server identity (SPEC
// §7, §9).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /claim/{id}", s.handleClaim)
	mux.HandleFunc("GET /state", s.handleState)
	mux.HandleFunc("GET /wait", s.handleWait)
	return mux
}

// handleClaim implements POST /claim/{id} (SPEC §7).
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		w.WriteHeader(http.StatusBadRequest)
		maybeWriteJSON(w, map[string]string{"error": "invalid id"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// A re-claim by the current owner is idempotent on owner/epoch, but it
	// still refreshes the published channel: the owner may have learned it
	// only after the claim that made it owner.
	channel, cerr := claimChannel(r)
	if cerr != nil {
		w.WriteHeader(http.StatusBadRequest)
		maybeWriteJSON(w, map[string]string{"error": cerr.Error()})
		return
	}
	if id == s.owner {
		if channel != 0 && channel != s.ownerChannel {
			s.ownerChannel = channel
			s.persist()
		}
		maybeWriteJSON(w, map[string]any{
			"owner":   s.owner,
			"epoch":   s.epoch,
			"changed": false,
		})
		return
	}

	force := r.URL.Query().Get("force")
	if s.waiters[id] == 0 && force != "true" && force != "1" {
		w.WriteHeader(http.StatusBadRequest)
		maybeWriteJSON(w, map[string]string{"error": fmt.Sprintf("no live agent for %s", id)})
		return
	}

	old := s.owner
	s.owner = id
	s.epoch++
	s.since = time.Now().UTC()
	s.ownerChannel = channel
	s.persist()
	slog.Info("owner changed", "from", old, "to", s.owner,
		"epoch", s.epoch, "channel", s.ownerChannel)
	s.wake()

	maybeWriteJSON(w, map[string]any{
		"owner":   s.owner,
		"epoch":   s.epoch,
		"changed": true,
	})
}

// handleState implements GET /state (SPEC §7).
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	st := s.CurrentState()
	maybeWriteJSON(w, st)
}

// CurrentState returns a snapshot of ServerState; caller must not mutate it.
func (s *Server) CurrentState() state.ServerState {
	s.mu.Lock()
	defer s.mu.Unlock()

	live := make(map[string]bool, len(s.waiters)+1)
	live[s.owner] = false
	for id, n := range s.waiters {
		if n > 0 {
			live[id] = true
		}
	}

	return state.ServerState{
		Owner:        s.owner,
		Epoch:        s.epoch,
		Since:        s.since,
		OwnerChannel: s.ownerChannel,
		Live:         live,
		ServerID:     s.serverID,
	}
}

// handleWait implements GET /wait?epoch=N&id=<me> (SPEC §7, §11.1).
func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	epochStr := r.URL.Query().Get("epoch")
	id := r.URL.Query().Get("id")

	if epochStr == "" || id == "" {
		w.WriteHeader(http.StatusBadRequest)
		maybeWriteJSON(w, map[string]string{"error": "missing epoch or id"})
		return
	}
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		maybeWriteJSON(w, map[string]string{"error": "invalid epoch"})
		return
	}

	s.mu.Lock()
	if epoch != s.epoch {
		st := s.serverStateLocked()
		s.mu.Unlock()
		maybeWriteJSON(w, map[string]any{"owner": st.Owner, "epoch": st.Epoch})
		return
	}

	s.waiters[id]++
	bc := s.broadcast
	slog.Debug("wait registered", "id", id, "epoch", s.epoch)
	s.mu.Unlock()

	var once sync.Once
	deregister := func() {
		once.Do(func() {
			s.mu.Lock()
			s.waiters[id]--
			if s.waiters[id] <= 0 {
				delete(s.waiters, id)
			}
			slog.Debug("wait deregistered", "id", id)
			s.mu.Unlock()
		})
	}
	defer deregister()

	timeout := s.waitTimeout
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}

	select {
	case <-bc:
		st := s.CurrentState()
		maybeWriteJSON(w, map[string]any{"owner": st.Owner, "epoch": st.Epoch})
	case <-time.After(timeout):
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
}

// serverStateLocked returns the ServerState snapshot while s.mu is held.
func (s *Server) serverStateLocked() state.ServerState {
	live := make(map[string]bool, len(s.waiters)+1)
	live[s.owner] = false
	for id, n := range s.waiters {
		if n > 0 {
			live[id] = true
		}
	}
	return state.ServerState{
		Owner:    s.owner,
		Epoch:    s.epoch,
		Since:    s.since,
		Live:     live,
		ServerID: s.serverID,
	}
}

// wake closes the current broadcast channel and replaces it.
// Caller must hold s.mu.
func (s *Server) wake() {
	close(s.broadcast)
	s.broadcast = make(chan struct{})
}

// Run serves HTTPS on addr ("[IP:]PORT") until ctx is cancelled, then shuts
// down gracefully. The TLS identity is derived from the shared secret (SPEC
// §9). BaseContext derives every request context from ctx so one cancel
// releases all /wait long-polls and Shutdown returns in ms (SPEC §11.1).
// Returns nil on ctx cancellation.
func (s *Server) Run(ctx context.Context, addr string) error {
	tlsCfg, err := identity.ServerTLSConfig(s.token)
	if err != nil {
		return fmt.Errorf("derive TLS identity: %w", err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServeTLS("", "")
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// maybeWriteJSON encodes v to w. A marshal error is a programming bug —
// every payload here is a JSON-safe value — so it panics. A write error means
// the client is already gone and is ignored.
func maybeWriteJSON(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	_, _ = w.Write(data)
}

// persist writes the owner bit. A failure is logged but does not block the
// in-memory state (SPEC §6.4).
func (s *Server) persist() {
	err := state.Save(s.statePath, state.OwnerState{
		Owner:        s.owner,
		Epoch:        s.epoch,
		Since:        s.since,
		OwnerChannel: s.ownerChannel,
	})
	if err != nil {
		slog.Error("failed to persist state", "path", s.statePath, "error", err)
	}
}

// claimChannel reads the optional ?channel=N on a claim: the Easy-Switch
// channel the claimant occupies on its peripherals. Absent means "unknown",
// which is not an error — a host with no HID++ peripheral has none to report.
func claimChannel(r *http.Request) (uint8, error) {
	raw := r.URL.Query().Get("channel")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(raw, 10, 8)
	if err != nil || n < 1 || n > hidpp.MaxChannel {
		return 0, fmt.Errorf("invalid channel %q (1-%d)", raw, hidpp.MaxChannel)
	}
	return uint8(n), nil
}
