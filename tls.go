// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// tls.go: TLS identity derived from the shared secret (SPEC §7, §9).

package main

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"time"
)

// tlsServerName is the SAN every derived certificate carries. Clients pin it
// as ServerName, so verification succeeds regardless of the dial address.
const tlsServerName = "soft-kvm"

// tlsIdentity derives the Ed25519 key and self-signed CA certificate that
// every instance sharing secret generates. The template is fixed and Ed25519
// signing is deterministic, so all of them produce byte-identical
// certificates: knowing the secret is both what lets a server present the
// certificate and what lets a client trust it. No fingerprint comparison, no
// configuration beyond SOFTKVM_TOKEN (SPEC §9).
func tlsIdentity(secret string) (tls.Certificate, error) {
	seed, err := hkdf.Key(sha256.New, []byte(secret), nil, "soft-kvm tls v1", ed25519.SeedSize)
	if err != nil {
		return tls.Certificate{}, err
	}
	priv := ed25519.NewKeyFromSeed(seed)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: tlsServerName},
		DNSNames:     []string{tlsServerName},
		NotBefore:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2126, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:         true,
		// BasicConstraintsValid so the CA bit is encoded; the certificate is
		// its own root on the client side.
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// Ed25519 ignores the random source, so the DER is deterministic.
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, nil
}

// serverTLSConfig serves the secret-derived identity.
func serverTLSConfig(secret string) (*tls.Config, error) {
	cert, err := tlsIdentity(secret)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// clientTLSConfig trusts exactly the secret-derived identity: the derived
// certificate is its own root and ServerName is pinned to its SAN.
func clientTLSConfig(secret string) (*tls.Config, error) {
	cert, err := tlsIdentity(secret)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	return &tls.Config{
		RootCAs:    pool,
		ServerName: tlsServerName,
		MinVersion: tls.VersionTLS13,
	}, nil
}
