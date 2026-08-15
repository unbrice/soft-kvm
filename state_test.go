// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	owner := ownerState{
		Owner: "mac",
		Epoch: 7,
		Since: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	}
	ownerPath := filepath.Join(dir, "owner.json")
	if err := saveJSON(ownerPath, owner); err != nil {
		t.Fatalf("saveJSON owner: %v", err)
	}

	var loadedOwner ownerState
	if err := loadJSON(ownerPath, &loadedOwner); err != nil {
		t.Fatalf("loadJSON owner: %v", err)
	}
	if loadedOwner != owner {
		t.Errorf("owner round-trip mismatch: got %+v, want %+v", loadedOwner, owner)
	}

	agent := agentState{LastOwner: "linux"}
	agentPath := filepath.Join(dir, "agent.json")
	if err := saveJSON(agentPath, agent); err != nil {
		t.Fatalf("saveJSON agent: %v", err)
	}

	var loadedAgent agentState
	if err := loadJSON(agentPath, &loadedAgent); err != nil {
		t.Fatalf("loadJSON agent: %v", err)
	}
	if loadedAgent != agent {
		t.Errorf("agent round-trip mismatch: got %+v, want %+v", loadedAgent, agent)
	}
}

func TestStateMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	var v ownerState
	if err := loadJSON(path, &v); err != nil {
		t.Fatalf("missing file returned error: %v", err)
	}
	if v != (ownerState{}) {
		t.Errorf("missing file should leave value zero; got %+v", v)
	}
}

func TestStateGarbageFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var v ownerState
	if err := loadJSON(path, &v); err == nil {
		t.Fatal("garbage file should return an error")
	}
}

func TestStateFileComplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	v := ownerState{Owner: "linux", Epoch: 3, Since: time.Now().UTC().Round(time.Second)}
	if err := saveJSON(path, v); err != nil {
		t.Fatalf("saveJSON: %v", err)
	}

	// The file must exist and be readable, with valid JSON, immediately after
	// saveJSON returns (no partial writes).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after save: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("saved file is empty")
	}

	var loaded ownerState
	if err := loadJSON(path, &loaded); err != nil {
		t.Fatalf("loadJSON after save: %v", err)
	}
	if loaded != v {
		t.Errorf("loaded mismatch: got %+v, want %+v", loaded, v)
	}
}
