// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package client_test

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

	"github.com/unbrice/soft-kvm/client"
	"github.com/unbrice/soft-kvm/identity"
	"github.com/unbrice/soft-kvm/server"
	"github.com/unbrice/soft-kvm/state"
)

const clientTestToken = "client-test-token"

func startTLSTestServer(t *testing.T, s *server.Server, token string) *httptest.Server {
	t.Helper()
	tlsCfg, err := identity.ServerTLSConfig(token)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	return srv
}

func newClientTestServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := server.NewServer(statePath, clientTestToken)
	s.SetWaitTimeout(300 * time.Millisecond)
	return s, startTLSTestServer(t, s, clientTestToken)
}

func newTestClient(t *testing.T, token string) *client.Client {
	t.Helper()
	c, err := client.NewClient(token)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
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
	changed, err := c.Claim(context.Background(), baseFromURL(t, srv.URL), "mac", true, false)
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
	_, err := c.Claim(context.Background(), baseFromURL(t, srv.URL), "mac", false, false)
	if !errors.Is(err, client.ErrNoLiveAgent) {
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
		st := s.CurrentState()
		if st.Live["mac"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mac never registered as live")
		}
		time.Sleep(5 * time.Millisecond)
	}

	changed, err := c.Claim(context.Background(), base, "mac", false, false)
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

	if _, err := c.Claim(context.Background(), base, "mac", true, false); err == nil {
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
	malformed.TLS, err = identity.ServerTLSConfig(clientTestToken)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
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
	server.TLS, err = identity.ServerTLSConfig(clientTestToken)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	server.StartTLS()
	defer server.Close()

	c := newTestClient(t, clientTestToken)
	_, err = c.Claim(context.Background(), baseFromURL(t, server.URL), "mac", false, false)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "plain text error") {
		t.Fatalf("error should contain body: %v", err)
	}
}

func TestClientTakeAndArrived(t *testing.T) {
	_, srv := newClientTestServer(t)
	defer srv.Close()
	base := baseFromURL(t, srv.URL)
	c := newTestClient(t, clientTestToken)
	ctx := context.Background()
	takes := []state.Take{{Kind: state.KindHID, Name: "mouse", Channel: 2}}

	// Nobody owns: a 409 becomes the sentinel.
	if err := c.Take(ctx, base, "mac", 0, takes); !errors.Is(err, client.ErrNotOwner) {
		t.Fatalf("take by a non-owner: %v, want ErrNotOwner", err)
	}
	if _, err := c.Claim(ctx, base, "mac", true, false); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := c.Take(ctx, base, "mac", 1, takes); err != nil {
		t.Fatalf("take: %v", err)
	}
	// A malformed take is a plain error, not the sentinel.
	err := c.Take(ctx, base, "mac", 1, []state.Take{{Kind: "led", Name: "x"}})
	if err == nil || errors.Is(err, client.ErrNotOwner) {
		t.Fatalf("malformed take: %v, want a plain error", err)
	}

	st, err := c.State(ctx, base)
	if err != nil || len(st.Holds) != 1 {
		t.Fatalf("state: %+v, %v", st, err)
	}
	h := st.Holds[0]
	if err := c.Arrived(ctx, base, "mac", state.Arrival{Kind: h.Kind, Name: h.Name, Seq: h.Seq}); err != nil {
		t.Fatalf("arrived: %v", err)
	}
	if st, _ = c.State(ctx, base); !st.Holds[0].Arrived {
		t.Fatalf("hold not arrived: %+v", st.Holds[0])
	}
}
