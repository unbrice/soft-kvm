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
	"time"
)

// ServerState is the wire format of GET /state and of a /wait wake, and the
// reconciler's input (SPEC §11.3). Clients treat Epoch as opaque: adopt it,
// never compare numerically (SPEC §7).
type ServerState struct {
	Owner    string          `json:"owner"`
	Epoch    int64           `json:"epoch"`
	Since    time.Time       `json:"since"`
	Live     map[string]bool `json:"live"`
	ServerID string          `json:"server_id"`
}

// OwnerState is the persisted subset of ServerState (SPEC §5.2 --state).
type OwnerState struct {
	Owner string    `json:"owner"`
	Epoch int64     `json:"epoch"`
	Since time.Time `json:"since"`
}

// AgentState is the agent's persisted record (SPEC §4.3 agent.json).
type AgentState struct {
	LastOwner string `json:"last_owner"`
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
