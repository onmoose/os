package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultSeedPath is the well-known, root-owned path the hosted cloud image's
// first-boot unit materializes the provisioning seed to (ENVIRONMENT.md #
// Provisioning & first-boot). The brain makes it overridable via
// MOOSE_SEED_PATH for tests; in the appliance profile no seed exists and the
// path is never read.
const DefaultSeedPath = "/var/lib/moose/seed.json"

// ErrSeedAbsent is returned by ReadSeed when no seed file exists at the path.
// On a hosted box this is the "provisioned without a seed" case: the brain logs
// it and stays pre-setup rather than crashing (a hosted box must never silently
// fall back to the appliance's open-/setup trust). It is distinct from a
// malformed/incomplete seed, which is a hard error worth surfacing.
var ErrSeedAbsent = errors.New("seed absent")

// Seed is the hosted-profile first-boot provisioning data (ENVIRONMENT.md #
// Provisioning). It replaces the appliance's "whoever is physically at the box
// during first boot" trust: the control plane allocates the box-id and the
// portal's assertion-verification key at provision time and injects them
// cloud-init-style. The verification key lets the box trust the portal's
// short-lived ownership assertions, so the owner reaches the dashboard via the
// portal-to-box SSO handshake (no box password, no /setup secret) — the box side
// of cloud Contract 1 (cloud specs/AUTH_AND_ACCESS.md # Portal-to-box SSO).
//
// Delivered as JSON at DefaultSeedPath. The test lane materializes it from a
// systemd credential over SMBIOS; a real cloud's metadata / config-drive maps
// onto the same first-boot materialization.
type Seed struct {
	// BoxID is the box's permanent identity in `base-suffix` form (e.g.
	// "cindy-fox"), allocated at provision and frozen for the life of the
	// install (MOOSE_NETWORK.md). The brain persists and surfaces it.
	BoxID string `json:"box_id"`
	// AssertionVerificationKey is the portal's Ed25519 *public* key in standard
	// (padded) base64 — 44 chars for the 32-byte key, the same for every box in v1
	// (single global portal keypair). The brain verifies the portal's SSO
	// assertions against it (internal/assertion). It replaces the prior one-time
	// admin-bootstrap secret: hosted boxes bootstrap the first admin via the SSO
	// handshake, not a /setup secret.
	AssertionVerificationKey string `json:"assertion_verification_key"`
	// Enrollment carries the per-box acme-dns account the box's Caddy uses to
	// obtain and renew its `*.<box-id>.onmoose.network` wildcard cert via ACME
	// DNS-01 (C3b; ENVIRONMENT.md # Networking & discovery). The JSON shape
	// mirrors the cloud producer's wire contract byte-for-byte (cloud
	// internal/seed.EnrollmentCredentials) — the two repos meet at this format,
	// not a shared Go type. It is optional at the seed-parse layer: a hosted box
	// seeded without it simply can't get a cert (the cert pass logs and skips);
	// the box-id + assertion key still work. omitempty so an appliance/un-enrolled
	// seed round-trips without an empty block.
	Enrollment EnrollmentCredentials `json:"enrollment,omitempty"`
	// UpdateTargetURL is the update-target endpoint this box reads (UPDATES.md
	// # 8.4). Empty means the control plane's fleet endpoint. It is here because
	// "which control plane does this box belong to" is fixed for the life of the
	// box, and the seed is written once when the VM is created (ENVIRONMENT.md
	// # Provisioning & first-boot). It is **configuration, not identity**: unlike
	// BoxID it is re-read on every boot and never frozen into the brain's SQLite.
	// host-agent reads it, the brain does not. omitempty so an appliance or
	// un-steered seed round-trips unchanged.
	UpdateTargetURL string `json:"update_target_url,omitempty"`
}

// EnrollmentCredentials is a per-box joohoi/acme-dns account (cloud
// specs/ARCHITECTURE.md Contract 2). The credential can set only this box's own
// `_acme-challenge` TXT, so Caddy renews the wildcard cert directly against
// acme-dns with no per-renewal cloud call. The acme-dns server's API endpoint is
// a box-side constant (the same for every box), not part of this seeded payload.
type EnrollmentCredentials struct {
	// Subdomain is the acme-dns account subdomain the box's
	// `_acme-challenge.<box-id>` CNAME points at (the CNAME is set cloud-side in
	// Route53 at provision).
	Subdomain string `json:"subdomain"`
	// Username and Password authenticate the box to acme-dns for TXT updates.
	Username string `json:"username"`
	Password string `json:"password"`
}

// Complete reports whether all three acme-dns fields are present. An incomplete
// (or absent) block means the box cannot configure DNS-01, so the hosted cert
// pass logs and skips rather than handing Caddy a half-configured issuer.
func (e EnrollmentCredentials) Complete() bool {
	return e.Subdomain != "" && e.Username != "" && e.Password != ""
}

// ReadSeed reads and validates the provisioning seed at path. A missing file
// returns ErrSeedAbsent (the expected hosted "no seed yet" case). A present but
// unreadable, malformed, or incomplete seed (missing box-id or
// assertion-verification key) returns a descriptive error — a mis-provisioned
// hosted box should be loud, not silently degraded. Surrounding whitespace on
// the string fields is trimmed.
func ReadSeed(path string) (Seed, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Seed{}, ErrSeedAbsent
		}
		return Seed{}, fmt.Errorf("read seed: %w", err)
	}
	var s Seed
	if err := json.Unmarshal(b, &s); err != nil {
		return Seed{}, fmt.Errorf("parse seed: %w", err)
	}
	s.BoxID = strings.TrimSpace(s.BoxID)
	s.AssertionVerificationKey = strings.TrimSpace(s.AssertionVerificationKey)
	if s.BoxID == "" {
		return Seed{}, errors.New("seed missing box_id")
	}
	if s.AssertionVerificationKey == "" {
		return Seed{}, errors.New("seed missing assertion_verification_key")
	}
	return s, nil
}
