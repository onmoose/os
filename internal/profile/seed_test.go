package profile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSeed writes content to a seed file in a fresh temp dir and returns its path.
func writeSeed(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return p
}

func TestReadSeed(t *testing.T) {
	tests := []struct {
		name string
		// content is the seed file's bytes; nil means no file is written
		// (the absent-seed case).
		content    *string
		wantErr    error  // sentinel to match with errors.Is; nil means "no error"
		wantErrSub string // substring the error must contain (when wantErr is nil but an error is expected)
		wantBox    string
		wantKey    string
	}{
		{
			name:    "bare box-id (the owner's chosen name)",
			content: ptr(`{"box_id":"andrei","assertion_verification_key":"a2V5"}`),
			wantBox: "andrei",
			wantKey: "a2V5",
		},
		{
			name:    "valid seed with a legacy dashed box-id",
			content: ptr(`{"box_id":"cindy-fox","assertion_verification_key":"a2V5"}`),
			wantBox: "cindy-fox",
			wantKey: "a2V5",
		},
		{
			name:    "surrounding whitespace trimmed",
			content: ptr(`{"box_id":"  cindy-fox \n","assertion_verification_key":"\t a2V5  "}`),
			wantBox: "cindy-fox",
			wantKey: "a2V5",
		},
		{
			name:    "absent file returns ErrSeedAbsent",
			content: nil,
			wantErr: ErrSeedAbsent,
		},
		{
			name:       "malformed JSON is a hard error",
			content:    ptr(`{not json`),
			wantErrSub: "parse seed",
		},
		{
			name:       "missing box_id is a hard error",
			content:    ptr(`{"assertion_verification_key":"a2V5"}`),
			wantErrSub: "missing box_id",
		},
		{
			name:       "blank box_id is a hard error",
			content:    ptr(`{"box_id":"   ","assertion_verification_key":"a2V5"}`),
			wantErrSub: "missing box_id",
		},
		{
			name:       "missing assertion_verification_key is a hard error",
			content:    ptr(`{"box_id":"cindy-fox"}`),
			wantErrSub: "missing assertion_verification_key",
		},
		{
			name:       "blank assertion_verification_key is a hard error",
			content:    ptr(`{"box_id":"cindy-fox","assertion_verification_key":"  "}`),
			wantErrSub: "missing assertion_verification_key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "does-not-exist")
			if tt.content != nil {
				path = writeSeed(t, *tt.content)
			}
			got, err := ReadSeed(path)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ReadSeed err = %v, want errors.Is %v", err, tt.wantErr)
				}
			case tt.wantErrSub != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("ReadSeed err = %v, want substring %q", err, tt.wantErrSub)
				}
			default:
				if err != nil {
					t.Fatalf("ReadSeed unexpected err: %v", err)
				}
				if got.BoxID != tt.wantBox {
					t.Errorf("BoxID = %q, want %q", got.BoxID, tt.wantBox)
				}
				if got.AssertionVerificationKey != tt.wantKey {
					t.Errorf("AssertionVerificationKey = %q, want %q", got.AssertionVerificationKey, tt.wantKey)
				}
			}
		})
	}
}

// TestReadSeed_UnreadableIsHardError covers the present-but-unreadable branch (an
// error other than not-exist). A directory is a deterministic stand-in under any
// uid — unlike a 0o000 file, which root (CI) can still read. It must NOT be
// mistaken for the absent-seed case.
func TestReadSeed_UnreadableIsHardError(t *testing.T) {
	_, err := ReadSeed(t.TempDir())
	if err == nil {
		t.Fatal("ReadSeed(dir) err = nil, want error")
	}
	if errors.Is(err, ErrSeedAbsent) {
		t.Errorf("ReadSeed(dir) returned ErrSeedAbsent; an unreadable seed must be a hard error, not the absent case")
	}
}

// TestReadSeed_EnrollmentParsed confirms the acme-dns enrollment block is parsed
// into typed fields off the cloud producer's wire shape (C3b consumes it for the
// wildcard cert). The field names must match the cloud's
// internal/seed.EnrollmentCredentials byte-for-byte.
func TestReadSeed_EnrollmentParsed(t *testing.T) {
	raw := `{"box_id":"cindy-fox","assertion_verification_key":"a2V5","enrollment":{"subdomain":"abc-123","username":"user","password":"pass"}}`
	got, err := ReadSeed(writeSeed(t, raw))
	if err != nil {
		t.Fatalf("ReadSeed: %v", err)
	}
	if !got.Enrollment.Complete() {
		t.Fatalf("enrollment = %+v, want Complete()", got.Enrollment)
	}
	if got.Enrollment.Subdomain != "abc-123" || got.Enrollment.Username != "user" || got.Enrollment.Password != "pass" {
		t.Errorf("enrollment = %+v, want subdomain=abc-123 username=user password=pass", got.Enrollment)
	}
}

// TestReadSeed_EnrollmentOptional confirms a seed with no enrollment block still
// parses (box-id + assertion key work) and reports an incomplete enrollment so
// the cert pass skips rather than handing Caddy a half-configured issuer.
func TestReadSeed_EnrollmentOptional(t *testing.T) {
	got, err := ReadSeed(writeSeed(t, `{"box_id":"cindy-fox","assertion_verification_key":"a2V5"}`))
	if err != nil {
		t.Fatalf("ReadSeed: %v", err)
	}
	if got.Enrollment.Complete() {
		t.Errorf("enrollment = %+v, want !Complete() (absent block)", got.Enrollment)
	}
}

// TestSeed_BareBoxIDRoundTrip writes a seed with a bare box-id the way the
// provisioning service would, reads it back, and checks the hosted names the
// box derives from it. The box must not split, extend or reshape the id.
func TestSeed_BareBoxIDRoundTrip(t *testing.T) {
	in := Seed{
		BoxID:                    "andrei",
		AssertionVerificationKey: "a2V5",
		Enrollment:               EnrollmentCredentials{Subdomain: "abc-123", Username: "user", Password: "pass"},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"box_id":"andrei"`) {
		t.Fatalf("wire seed = %s, want it to carry \"box_id\":\"andrei\"", raw)
	}
	got, err := ReadSeed(writeSeed(t, string(raw)))
	if err != nil {
		t.Fatalf("ReadSeed: %v", err)
	}
	if got.BoxID != "andrei" {
		t.Fatalf("BoxID = %q, want %q", got.BoxID, "andrei")
	}
	if !got.Enrollment.Complete() {
		t.Errorf("enrollment = %+v, want Complete()", got.Enrollment)
	}
	if h := HostedDashboardHost(got.BoxID); h != "andrei.onmoose.io" {
		t.Errorf("dashboard host = %q, want andrei.onmoose.io", h)
	}
	if h := HostedAppHost(got.BoxID, "photos"); h != "photos.andrei.onmoose.io" {
		t.Errorf("app host = %q, want photos.andrei.onmoose.io", h)
	}
}
