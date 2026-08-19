// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	owner := OwnerState{
		Owner: "mac",
		Epoch: 7,
		Since: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	}
	ownerPath := filepath.Join(dir, "owner.json")
	if err := Save(ownerPath, owner); err != nil {
		t.Fatalf("Save owner: %v", err)
	}

	var loadedOwner OwnerState
	if err := Load(ownerPath, &loadedOwner); err != nil {
		t.Fatalf("Load owner: %v", err)
	}
	if loadedOwner != owner {
		t.Errorf("owner round-trip mismatch: got %+v, want %+v", loadedOwner, owner)
	}

	agent := AgentState{LastOwner: "linux"}
	agentPath := filepath.Join(dir, "agent.json")
	if err := Save(agentPath, agent); err != nil {
		t.Fatalf("Save agent: %v", err)
	}

	var loadedAgent AgentState
	if err := Load(agentPath, &loadedAgent); err != nil {
		t.Fatalf("Load agent: %v", err)
	}
	if loadedAgent != agent {
		t.Errorf("agent round-trip mismatch: got %+v, want %+v", loadedAgent, agent)
	}
}

func TestStateMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	var v OwnerState
	if err := Load(path, &v); err != nil {
		t.Fatalf("missing file returned error: %v", err)
	}
	if v != (OwnerState{}) {
		t.Errorf("missing file should leave value zero; got %+v", v)
	}
}

func TestStateGarbageFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var v OwnerState
	if err := Load(path, &v); err == nil {
		t.Fatal("garbage file should return an error")
	}
}

func TestStateFileComplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	v := OwnerState{Owner: "linux", Epoch: 3, Since: time.Now().UTC().Round(time.Second)}
	if err := Save(path, v); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The file must exist and be readable, with valid JSON, immediately after
	// Save returns (no partial writes).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after save: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("saved file is empty")
	}

	var loaded OwnerState
	if err := Load(path, &loaded); err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if loaded != v {
		t.Errorf("loaded mismatch: got %+v, want %+v", loaded, v)
	}
}
