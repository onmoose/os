// Package assertion is the box side of the portal-to-box SSO handshake: it
// verifies the short-lived ownership assertion the portal hands a hosted box so
// its owner reaches the box dashboard without a box password or a /setup secret
// (cloud specs/AUTH_AND_ACCESS.md # Portal-to-box single sign-on). The portal
// signs "account X owns box-id Y" with its Ed25519 private key; this package
// verifies it with the public key the box received in its first-boot seed (cloud
// specs/ARCHITECTURE.md # Contract 1). The assertion is a convenience bootstrap,
// not a second identity store on the box — PAM stays the box's source of truth.
//
// Token format — a minimal Ed25519-signed token, NOT a JWT. The portal signs and
// this brain verifies; nothing third-party reads it, so there is no algorithm to
// negotiate: the token is base64url(claims-json) "." base64url(ed25519-sig), and
// the signature covers the exact base64url(claims) ASCII bytes transmitted (no
// separate canonicalization). Dropping the JWT header removes the entire
// alg-confusion / "alg":none footgun class — verification is a single
// ed25519.Verify plus an expiry check, never routed through a JWT library
// (cloud DECISIONS.md 2026-06-27). This Verify mirrors the cloud reference in
// cloud internal/assertion.Verify byte-for-byte; the wire shape is co-owned by
// the two repos (the OS C-series #275 / cloud #51), so a change here is a
// two-side change.
package assertion

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Claims is the assertion payload: the ownership fact plus the bounds the box
// enforces. The JSON tags are the wire contract with the cloud signer — they
// must match cloud internal/assertion.Claims exactly.
type Claims struct {
	// Iss is the issuer, which is the box domain every hosted box lives under
	// (profile.NetworkApex, "onmoose.io") and not the portal's own host. The box
	// rejects an assertion whose Iss is not that domain.
	Iss string `json:"iss"`
	// Sub is the portal account id that owns the box — the stable owner identity
	// the box ties its first admin to.
	Sub string `json:"sub"`
	// Email is the owner's portal email, from which the box derives the PAM
	// username for the auto-created first admin (the derivation is box-side).
	Email string `json:"email"`
	// Box is the box-id this assertion authorizes the bearer for. The box rejects
	// an assertion whose Box is not its own box-id.
	Box string `json:"box"`
	// Iat and Exp bound the token's validity in Unix seconds. Exp is intentionally
	// short (minted with a ~60s TTL): the token only has to survive one redirect.
	Iat int64 `json:"iat"`
	Exp int64 `json:"exp"`
	// JTI is a random nonce so the box can enforce single-use. Replay protection
	// is the box's job — the cloud only guarantees uniqueness per mint.
	JTI string `json:"jti"`
	// KID names the signing key, reserved for a future key rotation. v1 trusts the
	// single seed-carried verification key regardless of KID.
	KID string `json:"kid"`
}

// ErrMalformed is returned when the token is not the expected
// payload "." signature shape or a segment is not valid base64url. ErrSignature
// is returned when the Ed25519 signature does not verify, and ErrExpired when
// the token's Exp has passed. These are distinct so the SSO handler can log the
// reason without leaking it to the caller (every failure is one opaque 401).
var (
	ErrMalformed = errors.New("assertion: malformed token")
	ErrSignature = errors.New("assertion: signature mismatch")
	ErrExpired   = errors.New("assertion: expired")
)

// Verify checks token against pubKey and returns its claims, or an error if the
// token is malformed, the signature is bad, or it has expired. It mirrors the
// cloud reference verifier exactly: split on ".", decode and check the signature
// over the first segment's bytes *before* decoding the payload, then unmarshal
// and reject on expiry. It does NOT check the issuer, box-id, or replay (jti) —
// those are the SSO handler's policy to apply against the box's own identity once
// the signature and expiry hold.
func Verify(pubKey ed25519.PublicKey, token string) (Claims, error) {
	encPayload, encSig, ok := strings.Cut(token, ".")
	if !ok {
		return Claims{}, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(encSig)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: decode signature: %v", ErrMalformed, err)
	}
	// Verify the signature over the exact transmitted base64url(claims) bytes
	// before decoding the payload — never trust unverified bytes.
	if !ed25519.Verify(pubKey, []byte(encPayload), sig) {
		return Claims{}, ErrSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: decode payload: %v", ErrMalformed, err)
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: unmarshal claims: %v", ErrMalformed, err)
	}
	if time.Now().Unix() >= claims.Exp {
		return Claims{}, ErrExpired
	}
	return claims, nil
}
