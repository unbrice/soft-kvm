// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unbrice/soft-kvm/identity"
	"github.com/unbrice/soft-kvm/server"
	"github.com/unbrice/soft-kvm/state"
)

const testToken = "test-token-12345"

func testServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := server.NewServer(statePath, testToken)
	s.SetWaitTimeout(200 * time.Millisecond)
	srv := httptest.NewUnstartedServer(s.Handler())
	tlsCfg, err := identity.ServerTLSConfig(testToken)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	srv.TLS = tlsCfg
	srv.StartTLS()
	return s, srv
}

func testHTTPClient(t *testing.T, token string) *http.Client {
	t.Helper()
	tlsCfg, err := identity.ClientTLSConfig(token)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
}

func doReq(t *testing.T, method, rawURL string, body io.Reader) *http.Response {
	t.Helper()
	return doReqWithClient(t, testHTTPClient(t, testToken), method, rawURL, body)
}

func doReqWithClient(t *testing.T, client *http.Client, method, rawURL string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do %s %s: %v", method, rawURL, err)
	}
	return resp
}

func mustStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status %d, want %d: %s", resp.StatusCode, want, body)
	}
}

func readJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

func waitForLive(t *testing.T, s *server.Server, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		st := s.CurrentState()
		if st.Live[id] {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiter %q never registered", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForWaiters(t *testing.T, s *server.Server, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		n := s.WaiterCount(id)
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiter count for %q is %d, want %d", id, n, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServerClaimPersists(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := server.NewServer(statePath, testToken)
	s.SetWaitTimeout(200 * time.Millisecond)
	srv := httptest.NewUnstartedServer(s.Handler())
	tlsCfg, err := identity.ServerTLSConfig(testToken)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	// The server only accepts a claim for a live id (or with force=true).
	waitDone := make(chan *http.Response, 1)
	go func() {
		waitDone <- doReq(t, http.MethodGet, srv.URL+"/wait?epoch=0&id=mac", nil)
	}()
	waitForLive(t, s, "mac")

	resp := doReq(t, http.MethodPost, srv.URL+"/claim/mac", nil)
	mustStatus(t, resp, http.StatusOK)
	var claim map[string]any
	readJSON(t, resp, &claim)
	if claim["changed"] != true || claim["epoch"] != float64(1) || claim["owner"] != "mac" {
		t.Fatalf("unexpected claim response: %+v", claim)
	}

	// Release the waiter; it woke because the epoch changed.
	wake := <-waitDone
	mustStatus(t, wake, http.StatusOK)
	_ = wake.Body.Close()

	// A new server on the same state path reloads the persisted owner/epoch.
	s2 := server.NewServer(statePath, testToken)
	st := s2.CurrentState()
	if st.Owner != "mac" || st.Epoch != 1 {
		t.Fatalf("reloaded state mismatch: %+v", st)
	}
}

func TestServerClaimIdempotent(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()

	// Establish mac as a live agent and claim it once.
	wait1Done := make(chan *http.Response, 1)
	go func() {
		wait1Done <- doReq(t, http.MethodGet, srv.URL+"/wait?epoch=0&id=mac", nil)
	}()
	waitForLive(t, s, "mac")

	resp := doReq(t, http.MethodPost, srv.URL+"/claim/mac", nil)
	mustStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	wake1 := <-wait1Done
	mustStatus(t, wake1, http.StatusOK)
	_ = wake1.Body.Close()

	// Start a second waiter at the current epoch; re-claims must not wake it.
	wait2Done := make(chan *http.Response, 1)
	go func() {
		wait2Done <- doReq(t, http.MethodGet, srv.URL+"/wait?epoch=1&id=mac", nil)
	}()
	waitForLive(t, s, "mac")

	for i := 0; i < 50; i++ {
		resp := doReq(t, http.MethodPost, srv.URL+"/claim/mac", nil)
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		readJSON(t, resp, &body)
		if body["changed"] != false || body["epoch"] != float64(1) {
			t.Fatalf("re-claim %d moved epoch: %+v", i, body)
		}
	}

	waitResp := <-wait2Done
	mustStatus(t, waitResp, http.StatusNoContent)
	_ = waitResp.Body.Close()
}

func TestServerWaitWokenByClaim(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()

	waitURL := fmt.Sprintf("%s/wait?epoch=0&id=mac", srv.URL)
	results := make(chan *http.Response, 2)
	for i := 0; i < 2; i++ {
		go func() {
			results <- doReq(t, http.MethodGet, waitURL, nil)
		}()
	}

	// Wait for both wait handlers to register before claiming.
	waitForWaiters(t, s, "mac", 2)

	// Claim linux with force because no linux agent is currently live.
	resp := doReq(t, http.MethodPost, srv.URL+"/claim/linux?force=true", nil)
	mustStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	for i := 0; i < 2; i++ {
		r := <-results
		mustStatus(t, r, http.StatusOK)
		var body map[string]any
		readJSON(t, r, &body)
		if body["owner"] != "linux" || body["epoch"] != float64(1) {
			t.Fatalf("waiter got wrong state: %+v", body)
		}
	}
}

func TestServerWaitStaleEpoch(t *testing.T) {
	_, srv := testServer(t)
	defer srv.Close()

	// Current epoch is 0; asking for 99 returns immediately.
	resp := doReq(t, http.MethodGet, srv.URL+"/wait?epoch=99&id=mac", nil)
	mustStatus(t, resp, http.StatusOK)
	var body map[string]any
	readJSON(t, resp, &body)
	if body["epoch"] != float64(0) {
		t.Fatalf("stale epoch response: %+v", body)
	}
}

func TestServerWaitTimeout(t *testing.T) {
	_, srv := testServer(t)
	defer srv.Close()

	start := time.Now()
	resp := doReq(t, http.MethodGet, srv.URL+"/wait?epoch=0&id=mac", nil)
	mustStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("wait took too long: %v", elapsed)
	}
}

func TestServerStateLive(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()

	// No waiters yet; live map contains only the owner key.
	st := s.CurrentState()
	if st.Live == nil {
		t.Fatal("live map is nil")
	}
	if _, ok := st.Live[st.Owner]; !ok {
		t.Fatalf("owner key missing from live map: %+v", st.Live)
	}

	// Open a wait connection and wait for it to register.
	waitURL := fmt.Sprintf("%s/wait?epoch=0&id=mac", srv.URL)
	waitDone := make(chan *http.Response, 1)
	go func() {
		waitDone <- doReq(t, http.MethodGet, waitURL, nil)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		st = s.CurrentState()
		if st.Live["mac"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Close the wait by cancelling its request context.
	// (The httptest server request context is tied to the client request;
	// closing the response body after the server starts the handler does not
	// cancel it. We let it time out instead.)
	waitResp := <-waitDone
	mustStatus(t, waitResp, http.StatusNoContent)
	_ = waitResp.Body.Close()

	// After the waiter exits, mac is no longer live.
	deadline = time.Now().Add(time.Second)
	for {
		st = s.CurrentState()
		if !st.Live["mac"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mac still live after wait closed: %+v", st.Live)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServerAuth(t *testing.T) {
	_, srv := testServer(t)
	defer srv.Close()

	// A client with the matching token succeeds.
	resp := doReq(t, http.MethodGet, srv.URL+"/state", nil)
	mustStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	// A client with a different token fails at the TLS handshake.
	badClient := testHTTPClient(t, "wrong-token")
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/state", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err = badClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected TLS handshake failure for wrong token")
	}
	if !strings.Contains(err.Error(), "tls:") {
		t.Fatalf("expected tls error, got %v", err)
	}
}

func TestServerClaimNoLiveAgent(t *testing.T) {
	_, srv := testServer(t)
	defer srv.Close()

	resp := doReq(t, http.MethodPost, srv.URL+"/claim/linux", nil)
	mustStatus(t, resp, http.StatusBadRequest)
	var body map[string]string
	readJSON(t, resp, &body)
	if !strings.Contains(body["error"], "no live agent") {
		t.Fatalf("unexpected error: %q", body["error"])
	}

	// With force=true the claim succeeds anyway.
	resp = doReq(t, http.MethodPost, srv.URL+"/claim/linux?force=true", nil)
	mustStatus(t, resp, http.StatusOK)
	var claim map[string]any
	readJSON(t, resp, &claim)
	if claim["changed"] != true || claim["owner"] != "linux" {
		t.Fatalf("forced claim failed: %+v", claim)
	}

	// force=1 is also accepted.
	resp = doReq(t, http.MethodPost, srv.URL+"/claim/mac?force=1", nil)
	mustStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
}

func TestServerBadRequests(t *testing.T) {
	_, srv := testServer(t)
	defer srv.Close()

	cases := []struct {
		name, method, path string
	}{
		{"malformed id", http.MethodPost, "/claim/bad$id"},
		{"missing epoch", http.MethodGet, "/wait?id=mac"},
		{"missing id", http.MethodGet, "/wait?epoch=0"},
		{"non-numeric epoch", http.MethodGet, "/wait?epoch=abc&id=mac"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doReq(t, tc.method, srv.URL+tc.path, nil)
			mustStatus(t, resp, http.StatusBadRequest)
			_ = resp.Body.Close()
		})
	}
}

func TestServerEpochBackwards(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	// Seed epoch 10.
	if err := state.Save(statePath, state.OwnerState{Owner: "mac", Epoch: 10, Since: time.Now().UTC()}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	s1 := server.NewServer(statePath, testToken)
	s1.SetWaitTimeout(200 * time.Millisecond)
	srv1 := httptest.NewUnstartedServer(s1.Handler())
	tlsCfg, err := identity.ServerTLSConfig(testToken)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	srv1.TLS = tlsCfg
	srv1.StartTLS()
	defer srv1.Close()

	resp := doReq(t, http.MethodPost, srv1.URL+"/claim/linux?force=true", nil)
	mustStatus(t, resp, http.StatusOK)
	var claim map[string]any
	readJSON(t, resp, &claim)
	if claim["epoch"] != float64(11) {
		t.Fatalf("expected epoch 11, got %+v", claim)
	}

	// Rewind the file to epoch 3; the server serves what it loaded.
	if err := state.Save(statePath, state.OwnerState{Owner: "mac", Epoch: 3, Since: time.Now().UTC()}); err != nil {
		t.Fatalf("rewind state: %v", err)
	}
	s2 := server.NewServer(statePath, testToken)
	st := s2.CurrentState()
	if st.Epoch != 3 {
		t.Fatalf("expected reloaded epoch 3, got %d", st.Epoch)
	}
}

func TestServerCorruptState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := server.NewServer(statePath, testToken)
	st := s.CurrentState()
	if st.Owner != "" || st.Epoch != 0 {
		t.Fatalf("corrupt state did not start fresh: %+v", st)
	}
}

func TestServerRunShutdown(t *testing.T) {
	_, srv := testServer(t)
	defer srv.Close()

	// Run on an ephemeral port and cancel the context; Shutdown should complete
	// quickly because BaseContext ties request contexts to ctx.
	ctx, cancel := context.WithCancel(context.Background())
	s := server.NewServer(filepath.Join(t.TempDir(), "state.json"), testToken)
	s.SetWaitTimeout(200 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		done <- s.Run(ctx, "127.0.0.1:0")
	}()

	// Give Run a moment to start listening.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not shut down in time")
	}
}

// The winner's Easy-Switch channel travels with the claim and comes back on
// /state: the losing host cannot work it out locally (SPEC §5.5).
func TestServerClaimPublishesChannel(t *testing.T) {
	_, srv := testServer(t)
	defer srv.Close()

	resp := doReq(t, http.MethodPost, srv.URL+"/claim/linux?force=true&channel=2", nil)
	mustStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	resp = doReq(t, http.MethodGet, srv.URL+"/state", nil)
	mustStatus(t, resp, http.StatusOK)
	var st state.ServerState
	readJSON(t, resp, &st)
	if st.OwnerChannel != 2 {
		t.Fatalf("OwnerChannel = %d, want 2", st.OwnerChannel)
	}

	// A re-claim by the same owner is idempotent on the epoch but still
	// refreshes the channel: the agent may learn it only after winning.
	resp = doReq(t, http.MethodPost, srv.URL+"/claim/linux?channel=3", nil)
	mustStatus(t, resp, http.StatusOK)
	var claim map[string]any
	readJSON(t, resp, &claim)
	if claim["changed"] != false {
		t.Fatalf("re-claim changed the owner: %+v", claim)
	}
	resp = doReq(t, http.MethodGet, srv.URL+"/state", nil)
	mustStatus(t, resp, http.StatusOK)
	readJSON(t, resp, &st)
	if st.OwnerChannel != 3 {
		t.Fatalf("OwnerChannel after re-claim = %d, want 3", st.OwnerChannel)
	}

	// A claim with no channel leaves the owner's channel unknown rather than
	// inheriting the previous owner's.
	resp = doReq(t, http.MethodPost, srv.URL+"/claim/mac?force=true", nil)
	mustStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	resp = doReq(t, http.MethodGet, srv.URL+"/state", nil)
	mustStatus(t, resp, http.StatusOK)
	st = state.ServerState{}
	readJSON(t, resp, &st)
	if st.OwnerChannel != 0 {
		t.Fatalf("OwnerChannel = %d, want 0 for an owner that published none", st.OwnerChannel)
	}
}

func TestServerClaimRejectsBadChannel(t *testing.T) {
	_, srv := testServer(t)
	defer srv.Close()

	for _, q := range []string{"channel=0", "channel=4", "channel=x"} {
		resp := doReq(t, http.MethodPost, srv.URL+"/claim/linux?force=true&"+q, nil)
		mustStatus(t, resp, http.StatusBadRequest)
		_ = resp.Body.Close()
	}
}
