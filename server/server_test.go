// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unbrice/soft-kvm/client"
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

	// The closed wait leaves mac live for LiveGraceTime: the agent re-opens
	// its long-poll one /state round trip later (SPEC §4.3).
	if st = s.CurrentState(); !st.Live["mac"] {
		t.Fatalf("mac not live inside the grace: %+v", st.Live)
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

// baseOf returns the host:port the real client dials.
func baseOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}
	return u.Host
}

func realClient(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.NewClient(testToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// claimAs makes id the owner, forced: no agent is live in these tests.
func claimAs(t *testing.T, c *client.Client, base, id string) {
	t.Helper()
	if _, err := c.Claim(t.Context(), base, id, true, false); err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
}

func screenTake(name, input string) state.Take {
	return state.Take{Kind: state.KindScreen, Name: name, Input: input}
}

func hidTake(name string, channel uint8, arrived bool) state.Take {
	return state.Take{Kind: state.KindHID, Name: name, Channel: channel, Arrived: arrived}
}

func TestServerClaimActivation(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()
	c, base := realClient(t), baseOf(t, srv)

	claimAs(t, c, base, "linux")
	if st := s.CurrentState(); st.Owner != "linux" || st.Activation != 1 || st.Epoch != 1 {
		t.Fatalf("after first claim: %+v", st)
	}

	// An idempotent re-claim moves nothing.
	if changed, err := c.Claim(t.Context(), base, "linux", false, false); err != nil || changed {
		t.Fatalf("re-claim = %v, %v; want unchanged", changed, err)
	}
	if st := s.CurrentState(); st.Activation != 1 || st.Epoch != 1 {
		t.Fatalf("re-claim moved the state: %+v", st)
	}

	// A retake of the same owner bumps the activation and the epoch, and
	// wakes the waiters.
	woke := make(chan *http.Response, 1)
	go func() { woke <- doReq(t, http.MethodGet, srv.URL+"/wait?epoch=1&id=linux", nil) }()
	waitForLive(t, s, "linux")
	since := s.CurrentState().Since
	if changed, err := c.Claim(t.Context(), base, "linux", false, true); err != nil || !changed {
		t.Fatalf("retake = %v, %v; want changed", changed, err)
	}
	mustStatus(t, <-woke, http.StatusOK)
	st := s.CurrentState()
	if st.Owner != "linux" || st.Activation != 2 || st.Epoch != 2 || !st.Since.Equal(since) {
		t.Fatalf("after retake: %+v (since was %v)", st, since)
	}

	// A change of owner bumps both again.
	claimAs(t, c, base, "mac")
	if st := s.CurrentState(); st.Owner != "mac" || st.Activation != 3 || st.Epoch != 3 {
		t.Fatalf("after owner change: %+v", st)
	}
}

// The liveness rule applies to a retake as it does to a claim.
func TestServerRetakeNeedsLiveAgent(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()
	c, base := realClient(t), baseOf(t, srv)

	claimAs(t, c, base, "linux")
	if _, err := c.Claim(t.Context(), base, "linux", false, true); !errors.Is(err, client.ErrNoLiveAgent) {
		t.Fatalf("retake of an owner with no live agent: %v, want ErrNoLiveAgent", err)
	}
	if _, err := c.Claim(t.Context(), base, "linux", true, true); err != nil {
		t.Fatalf("forced retake: %v", err)
	}
	if st := s.CurrentState(); st.Activation != 2 {
		t.Fatalf("activation = %d, want 2", st.Activation)
	}
}

func TestServerTakeRecordsAssignments(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()
	c, base := realClient(t), baseOf(t, srv)

	if st := s.CurrentState(); st.Assignments == nil {
		t.Fatal("assignments is nil on a fresh server")
	}
	resp := doReq(t, http.MethodGet, srv.URL+"/state", nil)
	var raw map[string]json.RawMessage
	readJSON(t, resp, &raw)
	if string(raw["assignments"]) != "[]" {
		t.Fatalf("assignments on the wire = %s, want []", raw["assignments"])
	}

	claimAs(t, c, base, "linux") // epoch 1, activation 1
	err := c.Take(t.Context(), base, "linux", 1, []state.Take{
		hidTake("mouse", 2, false),
		screenTake("LG HDR WQHD", "f4:50=d1"),
		hidTake("keyboard", 1, true),
	})
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	st := s.CurrentState()
	if st.Epoch != 2 {
		t.Fatalf("epoch = %d, want one step for the whole request", st.Epoch)
	}
	want := []state.Assignment{
		{Kind: "hid", Name: "keyboard", Assignee: "linux", Seq: 2, Channel: 1, Arrived: true},
		{Kind: "hid", Name: "mouse", Assignee: "linux", Seq: 2, Channel: 2},
		{Kind: "screen", Name: "LG HDR WQHD", Assignee: "linux", Seq: 2, Input: "F4:50=D1"},
	}
	if !reflect.DeepEqual(st.Assignments, want) {
		t.Fatalf("assignments = %+v\nwant    %+v", st.Assignments, want)
	}

	// The same take again, same activation: nothing changes, nothing wakes.
	err = c.Take(t.Context(), base, "linux", 1, []state.Take{hidTake("mouse", 2, false), screenTake("LG HDR WQHD", "F4:50=D1")})
	if err != nil {
		t.Fatalf("re-take: %v", err)
	}
	if got := s.CurrentState(); got.Epoch != 2 || !reflect.DeepEqual(got.Assignments, want) {
		t.Fatalf("an identical take moved the state: %+v", got)
	}

	// Another input from the same assignee replaces the assignment; prev stays.
	if err := c.Take(t.Context(), base, "linux", 1, []state.Take{screenTake("LG HDR WQHD", "F4:50=D0")}); err != nil {
		t.Fatalf("take: %v", err)
	}
	got := s.CurrentState()
	if h := got.Assignments[2]; h.Input != "F4:50=D0" || h.Seq != 3 || h.Prev != "" || got.Epoch != 3 {
		t.Fatalf("replaced assignment = %+v, epoch %d", h, got.Epoch)
	}

	// A new assignee: prev becomes the old one, seq the new epoch, and the
	// arrival the take reported.
	claimAs(t, c, base, "mac") // epoch 4, activation 2
	if err := c.Take(t.Context(), base, "mac", 2, []state.Take{hidTake("mouse", 3, false), screenTake("LG HDR WQHD", "F4:50=D1")}); err != nil {
		t.Fatalf("take: %v", err)
	}
	got = s.CurrentState()
	byName := map[string]state.Assignment{}
	for _, h := range got.Assignments {
		byName[h.Kind+"/"+h.Name] = h
	}
	if h := byName["hid/mouse"]; h.Assignee != "mac" || h.Prev != "linux" || h.Seq != 5 || h.Channel != 3 || h.Arrived {
		t.Fatalf("mouse = %+v", h)
	}
	if h := byName["screen/LG HDR WQHD"]; h.Assignee != "mac" || h.Prev != "linux" || h.Seq != 5 || h.Input != "F4:50=D1" {
		t.Fatalf("screen = %+v", h)
	}
	if h := byName["hid/keyboard"]; h.Assignee != "linux" || h.Seq != 2 {
		t.Fatalf("an untouched assignment changed: %+v", h)
	}
}

// A restarted agent re-posts its takes for free, but a re-activation records
// them anew (SPEC §7).
func TestServerTakeNewActivationRecordsAgain(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()
	c, base := realClient(t), baseOf(t, srv)

	claimAs(t, c, base, "linux")
	takes := []state.Take{hidTake("mouse", 2, false)}
	if err := c.Take(t.Context(), base, "linux", 1, takes); err != nil {
		t.Fatal(err)
	}
	first := s.CurrentState().Assignments[0]
	if _, err := c.Claim(t.Context(), base, "linux", true, true); err != nil {
		t.Fatal(err)
	}
	if err := c.Take(t.Context(), base, "linux", 2, takes); err != nil {
		t.Fatal(err)
	}
	again := s.CurrentState().Assignments[0]
	if again.Seq == first.Seq || again.Prev != "" || again.Assignee != "linux" {
		t.Fatalf("assignment after re-activation = %+v, was %+v", again, first)
	}
}

func TestServerTakeRefusals(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()
	c, base := realClient(t), baseOf(t, srv)
	takes := []state.Take{hidTake("mouse", 2, false)}

	// Nobody owns yet.
	if err := c.Take(t.Context(), base, "linux", 0, takes); !errors.Is(err, client.ErrNotOwner) {
		t.Fatalf("take with no owner: %v, want ErrNotOwner", err)
	}
	claimAs(t, c, base, "linux")
	// Not the owner.
	if err := c.Take(t.Context(), base, "mac", 1, takes); !errors.Is(err, client.ErrNotOwner) {
		t.Fatalf("take by a non-owner: %v, want ErrNotOwner", err)
	}
	// A stale activation: the flip back landed while the commands ran.
	claimAs(t, c, base, "mac")
	claimAs(t, c, base, "linux") // activation 3
	if err := c.Take(t.Context(), base, "linux", 1, takes); !errors.Is(err, client.ErrNotOwner) {
		t.Fatalf("take under a stale activation: %v, want ErrNotOwner", err)
	}
	if st := s.CurrentState(); len(st.Assignments) != 0 || st.Epoch != 3 {
		t.Fatalf("a refused take changed the state: %+v", st)
	}
	if err := c.Take(t.Context(), base, "linux", 3, takes); err != nil {
		t.Fatalf("take under the current activation: %v", err)
	}
}

func TestServerTakeValidation(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()
	claimAs(t, realClient(t), baseOf(t, srv), "linux")

	many := make([]state.Take, 17)
	for i := range many {
		many[i] = hidTake(fmt.Sprintf("d%d", i), 1, false)
	}
	cases := []struct {
		name  string
		takes any
	}{
		{"unknown kind", []state.Take{{Kind: "led", Name: "x", Channel: 1}}},
		{"empty name", []state.Take{hidTake("", 1, false)}},
		{"long name", []state.Take{hidTake(strings.Repeat("n", 65), 1, false)}},
		{"unprintable name", []state.Take{hidTake("a\nb", 1, false)}},
		{"screen without input", []state.Take{screenTake("LG", "")}},
		{"screen input not hex", []state.Take{screenTake("LG", "F4:50=ZZ")}},
		{"screen input malformed", []state.Take{screenTake("LG", "F4-50=D1")}},
		{"screen input with a channel", []state.Take{{Kind: "screen", Name: "LG", Input: "60:51=0F", Channel: 1}}},
		{"hid channel 0", []state.Take{hidTake("mouse", 0, false)}},
		{"hid channel 4", []state.Take{hidTake("mouse", 4, false)}},
		{"hid with input", []state.Take{{Kind: "hid", Name: "mouse", Channel: 1, Input: "60:51=0F"}}},
		{"taken twice", []state.Take{hidTake("mouse", 1, false), hidTake("mouse", 2, false)}},
		{"seventeen takes", many},
		{"not an object", "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"activation": 1, "takes": tc.takes})
			resp := doReq(t, http.MethodPost, srv.URL+"/take/linux", strings.NewReader(string(body)))
			mustStatus(t, resp, http.StatusBadRequest)
			_ = resp.Body.Close()
		})
	}
	if st := s.CurrentState(); len(st.Assignments) != 0 {
		t.Fatalf("a malformed take left assignments: %+v", st.Assignments)
	}

	// Sixteen takes and a 64-character name are the limits, and fit.
	ok := many[:16]
	ok[0].Name = strings.Repeat("n", 64)
	body, _ := json.Marshal(map[string]any{"activation": 1, "takes": ok})
	resp := doReq(t, http.MethodPost, srv.URL+"/take/linux", strings.NewReader(string(body)))
	mustStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	resp = doReq(t, http.MethodPost, srv.URL+"/take/bad$id", strings.NewReader("{}"))
	mustStatus(t, resp, http.StatusBadRequest)
	_ = resp.Body.Close()
}

func TestServerArrived(t *testing.T) {
	s, srv := testServer(t)
	defer srv.Close()
	c, base := realClient(t), baseOf(t, srv)

	claimAs(t, c, base, "mac")
	if err := c.Take(t.Context(), base, "mac", 1, []state.Take{hidTake("mouse", 3, false), screenTake("LG", "60:51=0F")}); err != nil {
		t.Fatal(err)
	}
	epoch := s.CurrentState().Epoch
	seq := s.CurrentState().Assignments[0].Seq

	// None of these matches a current assignment of the sender: ignored, 200.
	for name, a := range map[string]struct {
		id string
		a  state.Arrival
	}{
		"another host": {"linux", state.Arrival{Kind: "hid", Name: "mouse", Seq: seq}},
		"stale seq":    {"mac", state.Arrival{Kind: "hid", Name: "mouse", Seq: seq - 1}},
		"unknown name": {"mac", state.Arrival{Kind: "hid", Name: "pen", Seq: seq}},
		"wrong kind":   {"mac", state.Arrival{Kind: "screen", Name: "mouse", Seq: seq}},
	} {
		if err := c.Arrived(t.Context(), base, a.id, a.a); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := s.CurrentState(); got.Epoch != epoch || got.Assignments[0].Arrived {
			t.Fatalf("%s moved the state: %+v", name, got)
		}
	}

	woke := make(chan *http.Response, 1)
	go func() {
		woke <- doReq(t, http.MethodGet, fmt.Sprintf("%s/wait?epoch=%d&id=linux", srv.URL, epoch), nil)
	}()
	waitForLive(t, s, "linux")
	if err := c.Arrived(t.Context(), base, "mac", state.Arrival{Kind: "hid", Name: "mouse", Seq: seq}); err != nil {
		t.Fatal(err)
	}
	mustStatus(t, <-woke, http.StatusOK)
	got := s.CurrentState()
	if h := got.Assignments[0]; !h.Arrived || h.Seq != seq || got.Epoch != epoch+1 {
		t.Fatalf("after arrival: %+v, epoch %d", h, got.Epoch)
	}

	// A second report of the same arrival changes nothing.
	if err := c.Arrived(t.Context(), base, "mac", state.Arrival{Kind: "hid", Name: "mouse", Seq: seq}); err != nil {
		t.Fatal(err)
	}
	if s.CurrentState().Epoch != epoch+1 {
		t.Fatal("a repeated arrival moved the epoch")
	}
}

func TestServerAssignmentsPersist(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := server.NewServer(statePath, testToken)
	srv := httptest.NewUnstartedServer(s.Handler())
	tlsCfg, err := identity.ServerTLSConfig(testToken)
	if err != nil {
		t.Fatal(err)
	}
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()
	c, base := realClient(t), baseOf(t, srv)

	claimAs(t, c, base, "mac")
	takes := []state.Take{hidTake("mouse", 3, false), screenTake("LG", "F4:50=D1")}
	if err := c.Take(t.Context(), base, "mac", 1, takes); err != nil {
		t.Fatal(err)
	}
	before := s.CurrentState()

	s2 := server.NewServer(statePath, testToken)
	after := s2.CurrentState()
	if after.Owner != "mac" || after.Activation != 1 || after.Epoch != before.Epoch || !reflect.DeepEqual(after.Assignments, before.Assignments) {
		t.Fatalf("reloaded %+v, want %+v", after, before)
	}

	// The reloaded server remembers the activation each assignment was taken
	// under: an identical re-take from a restarted agent changes nothing.
	srv2 := httptest.NewUnstartedServer(s2.Handler())
	srv2.TLS = tlsCfg
	srv2.StartTLS()
	defer srv2.Close()
	if err := c.Take(t.Context(), baseOf(t, srv2), "mac", 1, takes); err != nil {
		t.Fatal(err)
	}
	if got := s2.CurrentState(); got.Epoch != before.Epoch {
		t.Fatalf("re-take after a restart moved the epoch to %d", got.Epoch)
	}
}
