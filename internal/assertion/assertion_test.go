package assertion

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// mint reproduces the cloud signer's wire format so the box-side Verify is
// exercised against exactly the bytes the portal transmits.
func mint(t *testing.T, priv ed25519.PrivateKey, c Claims) string {
	t.Helper()
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func validClaims() Claims {
	now := time.Now()
	return Claims{
		Iss: "onmoose.io", Sub: "acct_1", Email: "a@b.com", Box: "cindy-fox",
		Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), JTI: "nonce", KID: "v1",
	}
}

func TestVerify_RoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	want := validClaims()
	got, err := Verify(pub, mint(t, priv, want))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != want {
		t.Fatalf("claims = %+v; want %+v", got, want)
	}
}

func TestVerify_Tampered(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	token := []byte(mint(t, priv, validClaims()))
	token[0] ^= 0x01 // corrupt the payload segment
	if _, err := Verify(pub, string(token)); !errors.Is(err, ErrSignature) && !errors.Is(err, ErrMalformed) {
		t.Fatalf("tampered err = %v; want signature/malformed", err)
	}
}

func TestVerify_WrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(otherPub, mint(t, priv, validClaims())); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong-key err = %v; want ErrSignature", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := validClaims()
	c.Exp = time.Now().Add(-time.Second).Unix()
	if _, err := Verify(pub, mint(t, priv, c)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired err = %v; want ErrExpired", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, tok := range []string{"", "noseparator", "a.b.c", "!!!.@@@"} {
		if _, err := Verify(pub, tok); err == nil {
			t.Errorf("Verify(%q) = nil err; want error", tok)
		}
	}
}
