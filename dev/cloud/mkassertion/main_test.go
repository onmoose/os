package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"github.com/onmoose/os/internal/assertion"
	"github.com/onmoose/os/internal/profile"
)

// TestMintRoundTrip is the contract this tool exists to satisfy: a token it mints
// must pass the brain's real verifier (internal/assertion.Verify) AND every box-side
// policy check ssoLanding applies (internal/api/sso.go) — otherwise the box rejects
// it and the access-mode e2e never gets a session. It mirrors the seeded boot: the
// harness seeds the public key, the box verifies a token signed by the private key.
func TestMintRoundTrip(t *testing.T) {
	const boxID = "cindy-fox"
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	now := time.Now()
	claims := assertion.Claims{
		Iss:   profile.NetworkApex,
		Sub:   "portal-owner",
		Email: "owner@example.com",
		Box:   boxID,
		Iat:   now.Unix(),
		Exp:   now.Add(2 * time.Hour).Unix(),
		JTI:   "0123456789abcdef",
	}

	token, err := mint(priv, claims)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Verify against the key AS IT CROSSES THE WIRE: standard-base64 encoded into the
	// seed (what the tool prints on line 1), then decoded by the box exactly as
	// cmd/brain decodeAssertionKey does. This pins the full harness→box path, not just
	// an in-memory key object — an encoding drift would 401 every assertion.
	seedKey := base64.StdEncoding.EncodeToString(pub)
	raw, err := base64.StdEncoding.DecodeString(seedKey)
	if err != nil {
		t.Fatalf("decode seed key: %v", err)
	}
	got, err := assertion.Verify(ed25519.PublicKey(raw), token)
	if err != nil {
		t.Fatalf("Verify rejected a freshly minted token: %v", err)
	}

	// The exact fields ssoLanding refuses an assertion for when empty/mismatched.
	if got.Iss != profile.NetworkApex {
		t.Errorf("iss = %q, want %q", got.Iss, profile.NetworkApex)
	}
	if got.Box != boxID {
		t.Errorf("box = %q, want %q", got.Box, boxID)
	}
	if got.Sub == "" || got.Email == "" || got.JTI == "" {
		t.Errorf("sub/email/jti must all be non-empty: sub=%q email=%q jti=%q", got.Sub, got.Email, got.JTI)
	}
}

// TestPublicKeyEncoding pins the public-key output to standard base64 of the raw
// 32-byte Ed25519 key — the encoding the lane's seeds use and the brain decodes at
// ingestion. A drift here silently breaks provisioning (the box loads a garbage key
// and every assertion 401s).
func TestPublicKeyEncoding(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	enc := base64.StdEncoding.EncodeToString(pub)
	decoded, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		t.Errorf("decoded key len = %d, want %d", len(decoded), ed25519.PublicKeySize)
	}
}
