// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// server.go: the coordinator HTTP service (SPEC §6.4, §7, §11.1).

package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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
)

var validID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Server carries the display bit and serves the /claim /state /wait API.
type Server struct {
	mu          sync.Mutex
	token       string
	statePath   string
	owner       string
	epoch       int64
	since       time.Time
	serverID    string
	waiters     map[string]int
	broadcast   chan struct{}
	waitTimeout time.Duration
}

// NewServer loads the persisted ownerState from statePath. A missing file is
// a fresh server (owner="", epoch=0); a corrupt one logs a warning and starts
// fresh the same way (SPEC §6.4).
func NewServer(statePath, token string) *Server {
	var persisted ownerState
	if err := loadJSON(statePath, &persisted); err != nil {
		slog.Warn("corrupt state file, starting fresh", "path", statePath, "error", err)
		persisted = ownerState{}
	}

	s := &Server{
		token:       token,
		statePath:   statePath,
		owner:       persisted.Owner,
		epoch:       persisted.Epoch,
		since:       persisted.Since,
		serverID:    newUUID(),
		waiters:     make(map[string]int),
		broadcast:   make(chan struct{}),
		waitTimeout: 50 * time.Second,
	}
	return s
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

// Handler returns the http.Handler for the service endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /claim/{id}", s.handleClaim)
	mux.HandleFunc("GET /state", s.handleState)
	mux.HandleFunc("GET /wait", s.handleWait)
	return s.withAuth(mux)
}

// withAuth enforces X-Display-Token on every handler except /health.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-Display-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			slog.Info("rejected request", "path", r.URL.Path, "remote", r.RemoteAddr, "reason", "bad token")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleClaim implements POST /claim/{id} (SPEC §7).
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid id"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if id == s.owner {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"owner":   s.owner,
			"epoch":   s.epoch,
			"changed": false,
		})
		return
	}

	force := r.URL.Query().Get("force")
	if s.waiters[id] == 0 && force != "true" && force != "1" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("no live agent for %s", id)})
		return
	}

	old := s.owner
	s.owner = id
	s.epoch++
	s.since = time.Now().UTC()
	if err := saveJSON(s.statePath, ownerState{Owner: s.owner, Epoch: s.epoch, Since: s.since}); err != nil {
		// Persist failure is logged but does not block the in-memory bit.
		slog.Error("failed to persist state", "path", s.statePath, "error", err)
	}
	slog.Info("owner changed", "from", old, "to", s.owner, "epoch", s.epoch)
	s.wake()

	_ = json.NewEncoder(w).Encode(map[string]any{
		"owner":   s.owner,
		"epoch":   s.epoch,
		"changed": true,
	})
}

// handleState implements GET /state (SPEC §7).
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	state := s.currentState()
	_ = json.NewEncoder(w).Encode(state)
}

// currentState returns a snapshot of ServerState; caller must not mutate it.
func (s *Server) currentState() ServerState {
	s.mu.Lock()
	defer s.mu.Unlock()

	live := make(map[string]bool, len(s.waiters)+1)
	live[s.owner] = false
	for id, n := range s.waiters {
		if n > 0 {
			live[id] = true
		}
	}

	return ServerState{
		Owner:    s.owner,
		Epoch:    s.epoch,
		Since:    s.since,
		Live:     live,
		ServerID: s.serverID,
	}
}

// handleWait implements GET /wait?epoch=N&id=<me> (SPEC §7, §11.1).
func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	epochStr := r.URL.Query().Get("epoch")
	id := r.URL.Query().Get("id")

	if epochStr == "" || id == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing epoch or id"})
		return
	}
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid epoch"})
		return
	}

	s.mu.Lock()
	if epoch != s.epoch {
		state := s.serverStateLocked()
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"owner": state.Owner, "epoch": state.Epoch})
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
		timeout = 50 * time.Second
	}

	select {
	case <-bc:
		state := s.currentState()
		_ = json.NewEncoder(w).Encode(map[string]any{"owner": state.Owner, "epoch": state.Epoch})
	case <-time.After(timeout):
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
}

// serverStateLocked returns the ServerState snapshot while s.mu is held.
func (s *Server) serverStateLocked() ServerState {
	live := make(map[string]bool, len(s.waiters)+1)
	live[s.owner] = false
	for id, n := range s.waiters {
		if n > 0 {
			live[id] = true
		}
	}
	return ServerState{
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

// handleHealth implements GET /health (SPEC §7).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// Run serves on addr ("[IP:]PORT") until ctx is cancelled, then shuts down
// gracefully. BaseContext derives every request context from ctx so one cancel
// releases all /wait long-polls and Shutdown returns in ms (SPEC §11.1).
// Returns nil on ctx cancellation.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
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
