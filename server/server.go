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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/unbrice/soft-kvm/hidpp"
	"github.com/unbrice/soft-kvm/identity"
	"github.com/unbrice/soft-kvm/state"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// DefaultWaitTimeout is how long /wait blocks before returning 204. It must be
// smaller than the client's internal /wait timeout.
const DefaultWaitTimeout = 50 * time.Second

// LiveGraceTime is how long a host stays live after its last /wait closed.
// Every wake and every 204 closes the long-poll, and the agent re-opens it one
// /state round trip later: without the grace the loser reads /state inside the
// winner's gap and refuses to switch (SPEC §4.3).
const LiveGraceTime = 30 * time.Second

// MaxTakes bounds the takes of one POST /take (SPEC §7).
const MaxTakes = 16

// maxBody bounds a request body: sixteen takes are a few KB at most.
const maxBody = 64 << 10

// maxNameRunes bounds an assignment's name (SPEC §7).
const maxNameRunes = 64

// validInput is a screen take's VCP:SRC=VAL: three hex bytes (SPEC §5.6).
var validInput = regexp.MustCompile(`^[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}=[0-9A-Fa-f]{2}$`)

// Server carries the display bit and the assignments, and serves the /claim
// /take /arrived /state /wait API.
type Server struct {
	mu         sync.Mutex
	token      string
	statePath  string
	owner      string
	epoch      int64
	since      time.Time
	activation int64
	// assignments is the last take per (kind, name), with the activation it was
	// taken under.
	assignments map[state.AssignmentKey]state.StoredAssignment
	serverID    string
	waiters     map[string]int
	lastWait    map[string]time.Time // when each host's last /wait closed
	broadcast   chan struct{}
	waitTimeout time.Duration
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
		token:       token,
		statePath:   statePath,
		owner:       persisted.Owner,
		epoch:       persisted.Epoch,
		since:       persisted.Since,
		activation:  persisted.Activation,
		assignments: make(map[state.AssignmentKey]state.StoredAssignment, len(persisted.Assignments)),
		serverID:    newUUID(),
		waiters:     make(map[string]int),
		lastWait:    make(map[string]time.Time),
		broadcast:   make(chan struct{}),
		waitTimeout: DefaultWaitTimeout,
	}
	for _, h := range persisted.Assignments {
		s.assignments[h.Key()] = h
	}
	return s
}

// isLiveLocked reports whether id has an open /wait, or closed its last one
// within the grace. Caller must hold s.mu.
func (s *Server) isLiveLocked(id string) bool {
	if s.waiters[id] > 0 {
		return true
	}
	last, ok := s.lastWait[id]
	return ok && time.Since(last) < LiveGraceTime
}

// liveLocked builds the /state live map: the owner is always present, every
// other host only while live. It drops expired grace entries. Caller must hold
// s.mu.
func (s *Server) liveLocked() map[string]bool {
	live := make(map[string]bool, len(s.waiters)+1)
	live[s.owner] = false
	for id := range s.waiters {
		live[id] = true
	}
	for id := range s.lastWait {
		if s.isLiveLocked(id) {
			live[id] = true
		} else {
			delete(s.lastWait, id)
		}
	}
	return live
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
	mux.HandleFunc("POST /take/{id}", s.handleTake)
	mux.HandleFunc("POST /arrived/{id}", s.handleArrived)
	mux.HandleFunc("GET /state", s.handleState)
	mux.HandleFunc("GET /wait", s.handleWait)
	return mux
}

// handleClaim implements POST /claim/{id} (SPEC §7). A change of owner bumps
// the activation and the epoch; so does ?retake=true on an unchanged owner,
// which is how `activate` makes the owner take again. Either needs a live
// agent unless ?force=true.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	retake := queryBool(r, "retake")
	if id == s.owner && !retake {
		maybeWriteJSON(w, map[string]any{
			"owner":   s.owner,
			"epoch":   s.epoch,
			"changed": false,
		})
		return
	}

	if !s.isLiveLocked(id) && !queryBool(r, "force") {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("no live agent for %s", id))
		return
	}

	old := s.owner
	if old != id {
		s.owner = id
		s.since = time.Now().UTC()
	}
	s.activation++
	s.epoch++
	s.persist()
	slog.Info("claimed", "from", old, "to", s.owner,
		"epoch", s.epoch, "activation", s.activation, "retake", retake)
	s.wake()

	maybeWriteJSON(w, map[string]any{
		"owner":   s.owner,
		"epoch":   s.epoch,
		"changed": true,
	})
}

// takeRequest is the body of POST /take/{id}.
type takeRequest struct {
	Activation int64        `json:"activation"`
	Takes      []state.Take `json:"takes"`
}

// handleTake implements POST /take/{id} (SPEC §7): the owner, under the
// activation its ownership dates from, asks that named screens and devices
// come to it.
func (s *Server) handleTake(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req takeRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateTakes(req.Takes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if id != s.owner || req.Activation != s.activation {
		slog.Info("take refused", "id", id, "owner", s.owner,
			"activation", req.Activation, "current", s.activation)
		writeError(w, http.StatusConflict, "not the owner under this activation")
		return
	}

	next := s.epoch + 1
	changed := false
	for _, t := range req.Takes {
		old, held := s.assignments[t.Key()]
		input := strings.ToUpper(t.Input)
		if held && old.Assignee == id && old.Input == input && old.Channel == t.Channel &&
			old.Activation == req.Activation {
			continue
		}
		h := state.Assignment{
			Kind: t.Kind, Name: t.Name, Assignee: id, Seq: state.Seq(next),
			Input: input, Channel: t.Channel, Arrived: t.Arrived && t.Kind == state.KindHID,
		}
		if held {
			h.Prev = old.Prev
			if old.Assignee != id {
				h.Prev = old.Assignee
			}
		}
		s.assignments[t.Key()] = state.StoredAssignment{Assignment: h, Activation: req.Activation}
		changed = true
		slog.Info("take recorded", "kind", h.Kind, "name", h.Name, "assignee", id,
			"prev", h.Prev, "seq", h.Seq, "input", h.Input, "channel", h.Channel, "arrived", h.Arrived)
	}
	s.bumpLocked(changed)
	maybeWriteJSON(w, map[string]any{"epoch": s.epoch, "changed": changed})
}

// arrivedRequest is the body of POST /arrived/{id}.
type arrivedRequest struct {
	Arrivals []state.Arrival `json:"arrivals"`
}

// handleArrived implements POST /arrived/{id} (SPEC §7): the assignee reports
// that a device it took is linked to it. An arrival that does not match a
// current assignment of this assignee is ignored: it answers a take that has
// since been replaced.
func (s *Server) handleArrived(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req arrivedRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	for _, a := range req.Arrivals {
		key := state.AssignmentKey{Kind: a.Kind, Name: a.Name}
		h, ok := s.assignments[key]
		if !ok || h.Assignee != id || h.Seq != a.Seq || h.Arrived {
			continue
		}
		h.Arrived = true
		s.assignments[key] = h
		changed = true
		slog.Info("arrived", "kind", h.Kind, "name", h.Name, "assignee", id, "seq", h.Seq)
	}
	s.bumpLocked(changed)
	maybeWriteJSON(w, map[string]any{"epoch": s.epoch, "changed": changed})
}

// bumpLocked ends a mutation: when changed, one epoch step, persisted, and
// every waiter woken. Caller must hold s.mu.
func (s *Server) bumpLocked(changed bool) {
	if !changed {
		return
	}
	s.epoch++
	s.persist()
	s.wake()
}

// validateTakes checks the shape of a take request, not its meaning: which
// VCP:SRC pairs are safe to write is the writer's check (SPEC §5.6, §7).
func validateTakes(takes []state.Take) error {
	if len(takes) > MaxTakes {
		return fmt.Errorf("%d takes, at most %d", len(takes), MaxTakes)
	}
	seen := make(map[state.AssignmentKey]bool, len(takes))
	for _, t := range takes {
		if t.Kind != state.KindScreen && t.Kind != state.KindHID {
			return fmt.Errorf("invalid kind %q", t.Kind)
		}
		if n := utf8.RuneCountInString(t.Name); n < 1 || n > maxNameRunes ||
			strings.IndexFunc(t.Name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
			return fmt.Errorf("invalid name %q: want 1-%d printable characters", t.Name, maxNameRunes)
		}
		if seen[t.Key()] {
			return fmt.Errorf("%s/%s taken twice", t.Kind, t.Name)
		}
		seen[t.Key()] = true
		switch t.Kind {
		case state.KindScreen:
			if !validInput.MatchString(t.Input) || t.Channel != 0 {
				return fmt.Errorf("invalid screen take %q: want input VCP:SRC=VAL as three hex bytes", t.Name)
			}
		case state.KindHID:
			if t.Input != "" || t.Channel < 1 || t.Channel > hidpp.MaxChannel {
				return fmt.Errorf("invalid hid take %q: want channel 1-%d and no input", t.Name, hidpp.MaxChannel)
			}
		}
	}
	return nil
}

// decodeBody reads a bounded JSON request body into v.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	return nil
}

// queryBool reads a flag such as ?force=true; "1" is accepted too.
func queryBool(r *http.Request, name string) bool {
	v := r.URL.Query().Get(name)
	return v == "true" || v == "1"
}

// writeError answers with a JSON error body.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	maybeWriteJSON(w, map[string]string{"error": msg})
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
	return s.serverStateLocked()
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
				s.lastWait[id] = time.Now()
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

// serverStateLocked returns the ServerState snapshot while s.mu is held. The
// assignments are sorted by kind, then name, and never nil.
func (s *Server) serverStateLocked() state.ServerState {
	return state.ServerState{
		Owner:       s.owner,
		Epoch:       s.epoch,
		Since:       s.since,
		Activation:  s.activation,
		Live:        s.liveLocked(),
		ServerID:    s.serverID,
		Assignments: s.assignmentsLocked(),
	}
}

// assignmentsLocked lists the assignments sorted by kind, then name. Caller
// must hold s.mu.
func (s *Server) assignmentsLocked() []state.Assignment {
	assignments := make([]state.Assignment, 0, len(s.assignments))
	for _, h := range s.assignments {
		assignments = append(assignments, h.Assignment)
	}
	slices.SortFunc(assignments, func(a, b state.Assignment) int {
		return cmpKeys(a.Key(), b.Key())
	})
	return assignments
}

func cmpKeys(a, b state.AssignmentKey) int {
	if c := strings.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
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

// persist writes the owner bit and the assignments. A failure is logged but
// does not block the in-memory state (SPEC §6.4).
func (s *Server) persist() {
	stored := make([]state.StoredAssignment, 0, len(s.assignments))
	for _, h := range s.assignments {
		stored = append(stored, h)
	}
	slices.SortFunc(stored, func(a, b state.StoredAssignment) int { return cmpKeys(a.Key(), b.Key()) })
	err := state.Save(s.statePath, state.OwnerState{
		Owner:       s.owner,
		Epoch:       s.epoch,
		Since:       s.since,
		Activation:  s.activation,
		Assignments: stored,
	})
	if err != nil {
		slog.Error("failed to persist state", "path", s.statePath, "error", err)
	}
}
