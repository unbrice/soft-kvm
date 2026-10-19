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

// Hold kinds: what a take names (SPEC §4.3, §7).
const (
	KindScreen = "screen"
	KindHID    = "hid"
)

// HoldKey identifies a hold: a monitor and a device may share a name.
type HoldKey struct{ Kind, Name string }

// String renders the key as "kind/name", the form agent.json stores it in.
func (k HoldKey) String() string { return k.Kind + "/" + k.Name }

// ParseHoldKey reads the "kind/name" form back; ok is false for any other
// string. A name may contain "/", a kind never does.
func ParseHoldKey(s string) (HoldKey, bool) {
	kind, name, ok := strings.Cut(s, "/")
	if !ok || name == "" || (kind != KindScreen && kind != KindHID) {
		return HoldKey{}, false
	}
	return HoldKey{kind, name}, true
}

// Seq identifies one recording of a hold: the server sets it to the epoch at
// which it recorded the take. Clients compare it for equality only (SPEC §7).
type Seq int64

// Hold is the server's record of the last take of one name (SPEC §7). No field
// is omitted when empty: a client decoding into a reused struct must not keep
// a stale value.
type Hold struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Holder string `json:"holder"`
	// Prev is the holder before this one, empty on a first take.
	Prev string `json:"prev"`
	// Seq changes every time the hold is recorded anew.
	Seq Seq `json:"seq"`
	// Input is a screen's VCP:SRC=VAL write, empty for a device.
	Input string `json:"input"`
	// Channel is a device's Easy-Switch channel, 0 for a screen.
	Channel uint8 `json:"channel"`
	// Arrived is set once the holder holds the device's link.
	Arrived bool `json:"arrived"`
}

// Key returns the hold's identity.
func (h Hold) Key() HoldKey { return HoldKey{h.Kind, h.Name} }

// Take is one item of a POST /take request: what the owner asks to come to it.
type Take struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Input   string `json:"input"`
	Channel uint8  `json:"channel"`
	Arrived bool   `json:"arrived"`
}

// Key returns the identity of the hold the take addresses.
func (t Take) Key() HoldKey { return HoldKey{t.Kind, t.Name} }

// Arrival is one item of a POST /arrived request: the holder reports the link
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
	// Holds is sorted by kind, then name, and never nil in JSON.
	Holds []Hold `json:"holds"`
}

// OwnerState is the server's persisted state (SPEC §5.2 --state, §7). Each
// hold carries the activation it was taken under: the server needs it for the
// idempotence rule and clients never see it.
type OwnerState struct {
	Owner      string       `json:"owner"`
	Epoch      int64        `json:"epoch"`
	Since      time.Time    `json:"since"`
	Activation int64        `json:"activation"`
	Holds      []StoredHold `json:"holds"`
}

// StoredHold is a Hold as persisted, with the activation it was taken under.
type StoredHold struct {
	Hold
	Activation int64 `json:"activation"`
}

// AgentState is the agent's persisted record (SPEC §4.3 agent.json).
type AgentState struct {
	LastOwner      string `json:"last_owner"`
	LastActivation int64  `json:"last_activation"`
	// Seen maps "kind/name" to the seq of the last take handled.
	Seen map[string]Seq `json:"seen"`
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
