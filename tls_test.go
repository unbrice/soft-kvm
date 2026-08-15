// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

// startTLSTestServer serves s.Handler() over TLS with the identity derived
// from s.token — the same path Server.Run takes.
func startTLSTestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	tlsCfg, err := serverTLSConfig(s.token)
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	return srv
}

// newTestClient returns a Client for token or fails the test.
func newTestClient(t *testing.T, token string) *Client {
	t.Helper()
	c, err := NewClient(token)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestTLSIdentityDeterministic(t *testing.T) {
	c1, err := tlsIdentity("secret-a")
	if err != nil {
		t.Fatalf("tlsIdentity: %v", err)
	}
	c2, err := tlsIdentity("secret-a")
	if err != nil {
		t.Fatalf("tlsIdentity: %v", err)
	}
	if !bytes.Equal(c1.Certificate[0], c2.Certificate[0]) {
		t.Fatal("same secret produced different certificates")
	}

	c3, err := tlsIdentity("secret-b")
	if err != nil {
		t.Fatalf("tlsIdentity: %v", err)
	}
	if bytes.Equal(c1.Certificate[0], c3.Certificate[0]) {
		t.Fatal("different secrets produced the same certificate")
	}
}
