// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

package identity

import (
	"bytes"
	"encoding/hex"
	"testing"
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
