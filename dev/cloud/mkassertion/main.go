// Command mkassertion plays the portal for the hosted cloud e2e access-mode lane
// (dev/cloud, issue #308). The box-only boot-proof lane is air-gapped and holds no
// portal private key, so it can only exercise the SSO verifier's *negative* paths
// (a bad token is 401); the positive path — a valid owner assertion → owner
// auto-create → box session → the Domain-scoped forward-auth cookie — is what the
// per-app access-mode proof needs. This tool supplies it without a real portal and
// WITHOUT any brain change: the harness generates a throwaway Ed25519 keypair, seeds
// the box with the PUBLIC key (the exact seed field a real portal fills), and mints
// a valid ownership assertion with the matching PRIVATE key. The private key never
// enters the VM — the harness is the portal — so the box verifies a genuine
// portal-signed assertion exactly as it would in production (internal/assertion.Verify,
// internal/api/sso.go # ssoLanding), mirroring the real portal-to-box trust model.
//
// It prints two lines to stdout by default, nothing else (diagnostics go to stderr):
//
//	<line 1>  the Ed25519 PUBLIC key, standard-base64 — the value the harness puts in
//	          the seed's "assertion_verification_key" (the same encoding the lane's
//	          random-key seeds already use, which the brain ingests today)
//	<line 2>  the signed assertion token — base64url(claims) "." base64url(ed25519-sig),
//	          the shape internal/assertion expects — delivered to the VM over SMBIOS and
//	          replayed as GET /_moose/sso?token=<token>
//
// With -tokens N it prints N token lines after the key instead of one, each with its
// own jti. A scenario needs more than one because the box spends a jti on first use:
// the access boot signs the owner in with the first, drives the hosted confirm step
// (os#469) with the second, and probes the return path's open-redirect guard with the
// third — three real portal round-trips.
//
// The claims are minted to satisfy every box-side policy check in ssoLanding: iss ==
// profile.NetworkApex, box == the box-id, and non-empty sub/email/jti. Exp is set far
// enough out (‑ttl) to outlast a slow TCG boot — a real portal mints a ~60s TTL, but
// this token has to survive minutes of air-gapped boot before the in-VM script uses it.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onmoose/os/internal/assertion"
	"github.com/onmoose/os/internal/profile"
)

func main() {
	var (
		box    = flag.String("box", "", "box-id the assertion authorizes (must equal the box's provisioned id)")
		sub    = flag.String("sub", "portal-owner", "owner portal account id (claims.sub)")
		email  = flag.String("email", "owner@example.com", "owner email; the box derives the PAM username from it")
		ttl    = flag.Duration("ttl", 2*time.Hour, "assertion lifetime; must outlast a slow air-gapped boot")
		tokens = flag.Int("tokens", 1, "how many independent assertions to mint (each with its own jti); 0 prints only the public key")
		keyF   = flag.String("key", "", "file holding the portal private key: read if it exists, made if not, so later runs mint tokens the same seed accepts (make dev HOSTED=1)")
	)
	flag.Parse()
	if *box == "" {
		fatal("mkassertion: -box is required")
	}
	if *tokens < 0 {
		fatal("mkassertion: -tokens must not be negative")
	}

	pub, priv, err := loadOrMakeKey(*keyF)
	if err != nil {
		fatal("portal key: %v", err)
	}

	// Line 1: the public key the seed carries. Standard base64 matches the lane's
	// existing random-key seeds (dev/cloud/run-cloud-tests.sh # seed_cred), which the
	// brain already decodes at ingestion.
	fmt.Println(base64.StdEncoding.EncodeToString(pub))

	// Then one line per requested token. Each carries its own jti: the box records a
	// spent jti and refuses the second use, so two steps of one scenario cannot share a token.
	now := time.Now()
	for i := 0; i < *tokens; i++ {
		jti := make([]byte, 16)
		if _, err := rand.Read(jti); err != nil {
			fatal("generate jti: %v", err)
		}
		claims := assertion.Claims{
			Iss:   profile.NetworkApex,
			Sub:   *sub,
			Email: *email,
			Box:   *box,
			Iat:   now.Unix(),
			Exp:   now.Add(*ttl).Unix(),
			JTI:   hex.EncodeToString(jti),
		}
		token, err := mint(priv, claims)
		if err != nil {
			fatal("mint token: %v", err)
		}
		fmt.Println(token)
	}
}

// loadOrMakeKey returns the portal key pair. With no path it makes a throwaway
// pair, as the cloud lane wants. With a path it reads the private key's seed
// (standard base64) from the file, or makes a pair and writes the seed there
// with mode 0600 when the file does not exist yet.
func loadOrMakeKey(path string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	if path == "" {
		return ed25519.GenerateKey(rand.Reader)
	}
	b, err := os.ReadFile(path)
	if err == nil {
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, nil, fmt.Errorf("%s does not hold a base64 ed25519 seed", path)
		}
		priv := ed25519.NewKeyFromSeed(seed)
		return priv.Public().(ed25519.PublicKey), priv, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	enc := base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"
	if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
		return nil, nil, err
	}
	return pub, priv, nil
}

// mint builds the box's assertion wire form: base64url(claims-json) "." base64url(sig),
// where the signature covers the exact base64url(claims) ASCII bytes (mirrors
// internal/assertion.Verify and the cloud reference signer — a change to the shape is a
// two-repo change). Kept here rather than exported from internal/assertion so the
// production package stays verify-only (no signing surface on the box).
func mint(priv ed25519.PrivateKey, c assertion.Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
