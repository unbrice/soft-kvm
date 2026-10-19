// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	owner := OwnerState{
		Owner:      "mac",
		Epoch:      7,
		Since:      time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
		Activation: 4,
		Holds: []StoredHold{{
			Hold:       Hold{Kind: KindScreen, Name: "LG", Holder: "mac", Prev: "linux", Seq: 7, Input: "F4:50=D1"},
			Activation: 4,
		}},
	}
	ownerPath := filepath.Join(dir, "owner.json")
	if err := Save(ownerPath, owner); err != nil {
		t.Fatalf("Save owner: %v", err)
	}

	var loadedOwner OwnerState
	if err := Load(ownerPath, &loadedOwner); err != nil {
		t.Fatalf("Load owner: %v", err)
	}
	if !reflect.DeepEqual(loadedOwner, owner) {
		t.Errorf("owner round-trip mismatch: got %+v, want %+v", loadedOwner, owner)
	}

	agent := AgentState{LastOwner: "linux", LastActivation: 3, Seen: map[string]Seq{"hid/mouse": 5}}
	agentPath := filepath.Join(dir, "agent.json")
	if err := Save(agentPath, agent); err != nil {
		t.Fatalf("Save agent: %v", err)
	}

	var loadedAgent AgentState
	if err := Load(agentPath, &loadedAgent); err != nil {
		t.Fatalf("Load agent: %v", err)
	}
	if !reflect.DeepEqual(loadedAgent, agent) {
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
	if !reflect.DeepEqual(v, OwnerState{}) {
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
	if !reflect.DeepEqual(loaded, v) {
		t.Errorf("loaded mismatch: got %+v, want %+v", loaded, v)
	}
}

// Hold encodes every field, so a client decoding into a reused struct never
// keeps a stale one (SPEC §7).
func TestHoldEncodesEveryField(t *testing.T) {
	data, err := json.Marshal(Hold{Kind: KindHID, Name: "mouse"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"kind", "name", "holder", "prev", "seq", "input", "channel", "arrived"} {
		if !strings.Contains(string(data), `"`+f+`":`) {
			t.Errorf("hold JSON %s lacks %q", data, f)
		}
	}
}

func TestHoldKey(t *testing.T) {
	h := Hold{Kind: KindScreen, Name: "LG"}
	if h.Key() != (HoldKey{KindScreen, "LG"}) || h.Key().String() != "screen/LG" {
		t.Errorf("Key() = %+v / %q", h.Key(), h.Key().String())
	}
}

func TestParseHoldKey(t *testing.T) {
	for _, k := range []HoldKey{{KindScreen, "LG HDR WQHD"}, {KindHID, "mouse"}, {KindHID, "a/b"}} {
		if got, ok := ParseHoldKey(k.String()); !ok || got != k {
			t.Errorf("ParseHoldKey(%q) = %+v, %v; want %+v", k.String(), got, ok, k)
		}
	}
	for _, s := range []string{"", "mouse", "led/x", "hid/", "/x"} {
		if got, ok := ParseHoldKey(s); ok {
			t.Errorf("ParseHoldKey(%q) = %+v, want no key", s, got)
		}
	}
}
