// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package identity

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"testing"
	"time"
)

func TestKeyFingerprint(t *testing.T) {
	fp1 := KeyFingerprint("secret-a")
	if fp1 != KeyFingerprint("secret-a") {
		t.Fatal("same secret produced different fingerprints")
	}
	if fp1 == KeyFingerprint("secret-b") {
		t.Fatal("different secrets produced the same fingerprint")
	}
	if len(fp1) != 16 {
		t.Fatalf("fingerprint %q is %d chars, want 16", fp1, len(fp1))
	}
	b, err := hex.DecodeString(fp1)
	if err != nil || len(b) != 8 || hex.EncodeToString(b) != fp1 {
		t.Fatalf("fingerprint %q is not 8 bytes of lowercase hex", fp1)
	}
}

func TestTLSIdentityDeterministic(t *testing.T) {
	ca1, client1, err := tlsIdentity("secret-a")
	if err != nil {
		t.Fatalf("tlsIdentity: %v", err)
	}
	ca2, client2, err := tlsIdentity("secret-a")
	if err != nil {
		t.Fatalf("tlsIdentity: %v", err)
	}
	if !bytes.Equal(ca1.Certificate[0], ca2.Certificate[0]) {
		t.Fatal("same secret produced different CA certificates")
	}
	if !bytes.Equal(client1.Certificate[0], client2.Certificate[0]) {
		t.Fatal("same secret produced different client certificates")
	}

	ca3, _, err := tlsIdentity("secret-b")
	if err != nil {
		t.Fatalf("tlsIdentity: %v", err)
	}
	if bytes.Equal(ca1.Certificate[0], ca3.Certificate[0]) {
		t.Fatal("different secrets produced the same certificate")
	}
}

// mtlsPair handshakes ServerTLSConfig(srvTok) against ClientTLSConfig(cliTok)
// over TCP and returns the server- and client-side results. It uses TCP rather
// than net.Pipe because a pipe's synchronous writes can deadlock once one side
// of a failing handshake stops reading; TCP lets the alert propagate.
func mtlsPair(t *testing.T, srvTok, cliTok string) (srvErr, cliErr error) {
	t.Helper()
	srvCfg, err := ServerTLSConfig(srvTok)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	cliCfg, err := ClientTLSConfig(cliTok)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing listener: %v", err)
		}
	})

	srvCh := make(chan error, 1)
	go func() {
		// Explicit close, not t.Cleanup: registering a cleanup from a spawned
		// goroutine panics if the test has already finished, and srvCh is
		// always received below, so this goroutine cannot outlive mtlsPair.
		conn, err := ln.Accept()
		if err != nil {
			srvCh <- err
			return
		}
		s := tls.Server(conn, srvCfg)
		if err := s.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			srvCh <- errors.Join(err, s.Close())
			return
		}
		// Close also closes conn; Join keeps a close error visible only when
		// the handshake itself succeeded.
		srvCh <- errors.Join(s.Handshake(), s.Close())
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	cli := tls.Client(conn, cliCfg) // Close also closes conn
	if err := cli.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	cliErr = cli.Handshake()
	srvErr = <-srvCh
	if err := cli.Close(); err != nil && cliErr == nil {
		t.Errorf("closing client conn: %v", err)
	}
	return srvErr, cliErr
}

func TestMTLSHandshake(t *testing.T) {
	srvErr, cliErr := mtlsPair(t, "token-a", "token-a")
	if srvErr != nil || cliErr != nil {
		t.Fatalf("same-token handshake failed: server=%v client=%v", srvErr, cliErr)
	}
}

func TestMTLSWrongToken(t *testing.T) {
	// A wrong token breaks both verifications at once: the server rejects the
	// client certificate and the client rejects the server identity.
	srvErr, cliErr := mtlsPair(t, "token-a", "token-b")
	if srvErr == nil && cliErr == nil {
		t.Fatal("expected handshake failure for mismatched tokens")
	}
}
