// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const clientTestToken = "client-test-token"

func newClientTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := NewServer(statePath, clientTestToken)
	s.waitTimeout = 300 * time.Millisecond
	return s, startTLSTestServer(t, s)
}

func baseFromURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u.Host
}

func TestClientClaimForce(t *testing.T) {
	_, srv := newClientTestServer(t)
	defer srv.Close()

	c := newTestClient(t, clientTestToken)
	changed, err := c.Claim(context.Background(), baseFromURL(t, srv.URL), "mac", true)
	if err != nil {
		t.Fatalf("claim force: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}

	state, err := c.State(context.Background(), baseFromURL(t, srv.URL))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.Owner != "mac" || state.Epoch != 1 {
		t.Fatalf("state after claim: %+v", state)
	}
}

func TestClientClaimNoLiveAgent(t *testing.T) {
	_, srv := newClientTestServer(t)
	defer srv.Close()

	c := newTestClient(t, clientTestToken)
	_, err := c.Claim(context.Background(), baseFromURL(t, srv.URL), "mac", false)
	if !errors.Is(err, ErrNoLiveAgent) {
		t.Fatalf("expected ErrNoLiveAgent, got %v", err)
	}
}

func TestClientWaitWakesOnClaim(t *testing.T) {
	s, srv := newClientTestServer(t)
	defer srv.Close()

	base := baseFromURL(t, srv.URL)
	c := newTestClient(t, clientTestToken)

	wokeCh := make(chan bool, 1)
	errCh := make(chan error, 1)
	go func() {
		woke, err := c.Wait(context.Background(), base, 0, "mac")
		if err != nil {
			errCh <- err
			return
		}
		wokeCh <- woke
	}()

	// Wait for the server's wait handler to register mac as live.
	deadline := time.Now().Add(time.Second)
	for {
		state := s.currentState()
		if state.Live["mac"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mac never registered as live")
		}
		time.Sleep(5 * time.Millisecond)
	}

	changed, err := c.Claim(context.Background(), base, "mac", false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}

	select {
	case err := <-errCh:
		t.Fatalf("wait returned error: %v", err)
	case woke := <-wokeCh:
		if !woke {
			t.Fatal("expected woke=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return")
	}
}

func TestClientWaitTimeout(t *testing.T) {
	_, srv := newClientTestServer(t)
	defer srv.Close()

	c := newTestClient(t, clientTestToken)
	woke, err := c.Wait(context.Background(), baseFromURL(t, srv.URL), 0, "mac")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if woke {
		t.Fatal("expected woke=false on server timeout")
	}
}

func TestClientState(t *testing.T) {
	_, srv := newClientTestServer(t)
	defer srv.Close()

	c := newTestClient(t, clientTestToken)
	state, err := c.State(context.Background(), baseFromURL(t, srv.URL))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.ServerID == "" {
		t.Fatal("expected non-empty server_id")
	}
	if state.Epoch != 0 {
		t.Fatalf("initial epoch should be 0, got %d", state.Epoch)
	}
}

func TestClientWrongSecret(t *testing.T) {
	_, srv := newClientTestServer(t)
	defer srv.Close()

	base := baseFromURL(t, srv.URL)
	// A client holding a different secret derives a different trust root, so
	// the TLS handshake fails before any request reaches the token check
	// (SPEC §9).
	c := newTestClient(t, "wrong-token")

	if _, err := c.Claim(context.Background(), base, "mac", true); err == nil {
		t.Fatal("claim: expected TLS failure")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("claim: expected certificate error, got %v", err)
	}
	if _, err := c.State(context.Background(), base); err == nil {
		t.Fatal("state: expected TLS failure")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("state: expected certificate error, got %v", err)
	}
	if _, err := c.Wait(context.Background(), base, 0, "mac"); err == nil {
		t.Fatal("wait: expected TLS failure")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("wait: expected certificate error, got %v", err)
	}
}

func TestClientConnectionRefused(t *testing.T) {
	c := newTestClient(t, clientTestToken)
	if _, err := c.State(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestClientMalformedResponse(t *testing.T) {
	malformed := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	var err error
	malformed.TLS, err = serverTLSConfig(clientTestToken)
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	malformed.StartTLS()
	defer malformed.Close()

	c := newTestClient(t, clientTestToken)
	_, err = c.State(context.Background(), baseFromURL(t, malformed.URL))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "decode state response") {
		t.Fatalf("error should mention decode failure: %v", err)
	}
}

func TestClientNonJSONErrorBody(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, "plain text error")
	}))
	var err error
	server.TLS, err = serverTLSConfig(clientTestToken)
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	server.StartTLS()
	defer server.Close()

	c := newTestClient(t, clientTestToken)
	_, err = c.Claim(context.Background(), baseFromURL(t, server.URL), "mac", false)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "plain text error") {
		t.Fatalf("error should contain body: %v", err)
	}
}
