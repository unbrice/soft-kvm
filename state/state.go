// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// state.go: the /state wire type and atomic JSON persistence (SPEC §6.4, §7).

package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Assignment kinds: what a take names (SPEC §4.3, §7).
const (
	KindScreen = "screen"
	KindHID    = "hid"
)

// AssignmentKey identifies an assignment: a monitor and a device may share a
// name.
type AssignmentKey struct{ Kind, Name string }

// String renders the key as "kind/name", the form agent.json stores it in.
func (k AssignmentKey) String() string { return k.Kind + "/" + k.Name }

// ParseAssignmentKey reads the "kind/name" form back; ok is false for any other
// string. A name may contain "/", a kind never does.
func ParseAssignmentKey(s string) (AssignmentKey, bool) {
	kind, name, ok := strings.Cut(s, "/")
	if !ok || name == "" || (kind != KindScreen && kind != KindHID) {
		return AssignmentKey{}, false
	}
	return AssignmentKey{kind, name}, true
}

// Seq identifies one recording of an assignment: the server sets it to the
// epoch at which it recorded the take. Clients compare it for equality only
// (SPEC §7).
type Seq int64

// Assignment is the server's record of the last take of one name (SPEC §7). No
// field is omitted when empty: a client decoding into a reused struct must not
// keep a stale value.
type Assignment struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Assignee string `json:"assignee"`
	// Prev is the assignee before this one, empty on a first take.
	Prev string `json:"prev"`
	// Seq changes every time the assignment is recorded anew.
	Seq Seq `json:"seq"`
	// Input is a screen's VCP:SRC=VAL write, empty for a device.
	Input string `json:"input"`
	// Channel is a device's Easy-Switch channel, 0 for a screen.
	Channel uint8 `json:"channel"`
	// Arrived is set once the assignee holds the device's link.
	Arrived bool `json:"arrived"`
}

// Key returns the assignment's identity.
func (h Assignment) Key() AssignmentKey { return AssignmentKey{h.Kind, h.Name} }

// Take is one item of a POST /take request: what the owner asks to come to it.
type Take struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Input   string `json:"input"`
	Channel uint8  `json:"channel"`
	Arrived bool   `json:"arrived"`
}

// Key returns the identity of the assignment the take addresses.
func (t Take) Key() AssignmentKey { return AssignmentKey{t.Kind, t.Name} }

// Arrival is one item of a POST /arrived request: the assignee reports the link
// of the device held at Seq.
type Arrival struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Seq  Seq    `json:"seq"`
}

// ServerState is the wire format of GET /state and of a /wait wake, and the
// reconciler's input (SPEC §11.3). Clients treat Epoch, Activation and every
// Seq as opaque: adopt them, compare for equality only (SPEC §7).
type ServerState struct {
	Owner      string          `json:"owner"`
	Epoch      int64           `json:"epoch"`
	Since      time.Time       `json:"since"`
	Activation int64           `json:"activation"`
	Live       map[string]bool `json:"live"`
	ServerID   string          `json:"server_id"`
	// Assignments is sorted by kind, then name, and never nil in JSON.
	Assignments []Assignment `json:"assignments"`
}

// OwnerState is the server's persisted state (SPEC §5.2 --state, §7). Each
// assignment carries the activation it was taken under: the server needs it for
// the idempotence rule and clients never see it.
type OwnerState struct {
	Owner       string             `json:"owner"`
	Epoch       int64              `json:"epoch"`
	Since       time.Time          `json:"since"`
	Activation  int64              `json:"activation"`
	Assignments []StoredAssignment `json:"assignments"`
}

// StoredAssignment is a Assignment as persisted, with the activation it was
// taken under.
type StoredAssignment struct {
	Assignment
	Activation int64 `json:"activation"`
}

// AgentState is the agent's persisted record of what it handled (SPEC §4.3
// agent.json): the ownership whose commands last ran, and the seq of each
// assignment last handled.
type AgentState struct {
	HandledOwner      string `json:"handled_owner"`
	HandledActivation int64  `json:"handled_activation"`
	// HandledSeqs maps "kind/name" to the seq of the last assignment handled.
	HandledSeqs map[string]Seq `json:"handled_seqs"`
}

// Load reads path into v. A missing file is not an error: v is left
// untouched (fresh install). On any other error the caller must discard v
// (it may be partially decoded) and start from the zero value (SPEC §6.4:
// a truncated state file after a power cut must not crash-loop the service).
func Load(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Save writes v to path atomically: temp file in the same directory,
// fsync, rename (SPEC §6.4).
func Save(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".soft-kvm-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// Fsync the directory so the rename itself is crash-durable.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
