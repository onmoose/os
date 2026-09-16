package relmanifest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// --- test signer ------------------------------------------------------------
//
// A minisign signer, written here because the box only ever verifies. It
// produces the real file layout (both algorithm modes and both signatures), so
// the tests exercise the parser against bytes shaped like minisign's own output
// rather than against a shape invented to match the verifier.

type signer struct {
	id   [8]byte
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatalf("key id: %v", err)
	}
	return signer{id: id, priv: priv, pub: pub}
}

// pubKeyFile renders the two-line minisign public-key file.
func (s signer) pubKeyFile() string {
	raw := append([]byte{'E', 'd'}, s.id[:]...)
	raw = append(raw, s.pub...)
	return "untrusted comment: minisign public key\n" + base64.StdEncoding.EncodeToString(raw) + "\n"
}

func (s signer) publicKey(t *testing.T) PublicKey {
	t.Helper()
	pk, err := ParsePublicKey(s.pubKeyFile())
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	return pk
}

// sign renders a .minisig file. prehashed picks the "ED" mode (sign BLAKE2b-512
// of the message) over the legacy "Ed" mode (sign the message itself).
func (s signer) sign(message []byte, trustedComment string, prehashed bool) string {
	alg := [2]byte{'E', 'd'}
	signed := message
	if prehashed {
		alg = [2]byte{'E', 'D'}
		h := blake2b.Sum512(message)
		signed = h[:]
	}
	sig := ed25519.Sign(s.priv, signed)
	line1 := append(append([]byte{alg[0], alg[1]}, s.id[:]...), sig...)
	global := ed25519.Sign(s.priv, append(append([]byte(nil), sig...), []byte(trustedComment)...))
	return fmt.Sprintf(
		"untrusted comment: signature from minisign secret key\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(line1),
		trustedComment,
		base64.StdEncoding.EncodeToString(global),
	)
}

const goodManifest = `{
  "manifest_version": 1,
  "channel": "stable",
  "brain": "1.4.2",
  "ui": "1.4.2",
  "minimum_host_agent": "1.0.0",
  "released_at": "2026-05-08T12:00:00Z",
  "rollback_to": null
}`

// --- verify -----------------------------------------------------------------

func TestVerifyAcceptsAGenuineSignatureInBothModes(t *testing.T) {
	s := newSigner(t)
	v := NewVerifier(s.publicKey(t))

	for _, prehashed := range []bool{false, true} {
		sig := s.sign([]byte(goodManifest), "timestamp:1746705600\tfile:stable.json", prehashed)
		tc, err := v.Verify([]byte(goodManifest), sig)
		if err != nil {
			t.Fatalf("prehashed=%v: Verify: %v", prehashed, err)
		}
		if !strings.Contains(tc, "file:stable.json") {
			t.Fatalf("prehashed=%v: trusted comment = %q", prehashed, tc)
		}
	}
}

// A box built before a signing key exists trusts nothing. This is the shipping
// state today, so it is the one that must not quietly pass.
func TestVerifyWithNoKeysRefusesEverything(t *testing.T) {
	s := newSigner(t)
	v := NewVerifier()
	if v.Keys() != 0 {
		t.Fatalf("Keys() = %d, want 0", v.Keys())
	}
	_, err := v.Verify([]byte(goodManifest), s.sign([]byte(goodManifest), "t", true))
	if !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys", err)
	}
}

func TestVerifyRefusesAKeyItDoesNotAccept(t *testing.T) {
	mine, theirs := newSigner(t), newSigner(t)
	v := NewVerifier(mine.publicKey(t))
	_, err := v.Verify([]byte(goodManifest), theirs.sign([]byte(goodManifest), "t", true))
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

// Rotation is why the verifier holds a list. A box shipped with {old, new} must
// accept a manifest signed by either.
func TestVerifyAcceptsAnyKeyInTheList(t *testing.T) {
	oldKey, newKey := newSigner(t), newSigner(t)
	v := NewVerifier(oldKey.publicKey(t), newKey.publicKey(t))
	for name, s := range map[string]signer{"old": oldKey, "new": newKey} {
		if _, err := v.Verify([]byte(goodManifest), s.sign([]byte(goodManifest), "t", true)); err != nil {
			t.Fatalf("%s key: Verify: %v", name, err)
		}
	}
}

func TestVerifyRefusesATamperedManifest(t *testing.T) {
	s := newSigner(t)
	v := NewVerifier(s.publicKey(t))
	sig := s.sign([]byte(goodManifest), "t", true)
	tampered := strings.Replace(goodManifest, `"brain": "1.4.2"`, `"brain": "9.9.9"`, 1)
	if tampered == goodManifest {
		t.Fatal("test bug: nothing was tampered with")
	}
	if _, err := v.Verify([]byte(tampered), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

// The global signature is the only thing standing between a caller and an
// attacker-chosen "trusted" comment on an otherwise genuine file. Deleting the
// global-signature check in Verify makes this test fail and nothing else.
func TestVerifyRefusesARewrittenTrustedComment(t *testing.T) {
	s := newSigner(t)
	v := NewVerifier(s.publicKey(t))
	sig := s.sign([]byte(goodManifest), "timestamp:1746705600\tfile:stable.json", true)
	rewritten := strings.Replace(sig, "file:stable.json", "file:anything-else.json", 1)
	if rewritten == sig {
		t.Fatal("test bug: the comment was not rewritten")
	}
	if _, err := v.Verify([]byte(goodManifest), rewritten); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyRefusesMalformedSignatureFiles(t *testing.T) {
	s := newSigner(t)
	v := NewVerifier(s.publicKey(t))
	full := s.sign([]byte(goodManifest), "t", true)
	lines := strings.Split(strings.TrimRight(full, "\n"), "\n")

	cases := map[string]string{
		"empty":                 "",
		"comment only":          lines[0] + "\n",
		"no trusted comment":    lines[0] + "\n" + lines[1] + "\n" + lines[3] + "\n",
		"no global signature":   lines[0] + "\n" + lines[1] + "\n" + lines[2] + "\n",
		"signature not base64":  lines[0] + "\nnot base64!!\n" + lines[2] + "\n" + lines[3] + "\n",
		"signature wrong size":  lines[0] + "\n" + base64.StdEncoding.EncodeToString([]byte("short")) + "\n" + lines[2] + "\n" + lines[3] + "\n",
		"unknown algorithm":     strings.Replace(full, lines[1], base64.StdEncoding.EncodeToString(append([]byte("XX"), make([]byte, 72)...)), 1),
		"global sig wrong size": lines[0] + "\n" + lines[1] + "\n" + lines[2] + "\n" + base64.StdEncoding.EncodeToString([]byte("short")) + "\n",
	}
	for name, sig := range cases {
		if _, err := v.Verify([]byte(goodManifest), sig); err == nil {
			t.Errorf("%s: Verify accepted a malformed signature file", name)
		}
	}
}

func TestParsePublicKeyRejectsJunk(t *testing.T) {
	for name, in := range map[string]string{
		"empty":      "",
		"not base64": "untrusted comment: x\nnot base64!!\n",
		"too short":  base64.StdEncoding.EncodeToString([]byte("Ed12345678")),
		"bad alg":    base64.StdEncoding.EncodeToString(append([]byte("XX"), make([]byte, 40)...)),
	} {
		if _, err := ParsePublicKey(in); err == nil {
			t.Errorf("%s: ParsePublicKey accepted junk", name)
		}
	}
}

// --- parse ------------------------------------------------------------------

func TestParseReadsTheV1Schema(t *testing.T) {
	m, err := Parse([]byte(goodManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Brain != "1.4.2" || m.UI != "1.4.2" || m.MinimumHostAgent != "1.0.0" {
		t.Fatalf("parsed %+v", m)
	}
	if m.RollbackTo != nil {
		t.Fatalf("rollback_to = %+v, want nil in steady state", m.RollbackTo)
	}
	if m.ReleasedAt.IsZero() {
		t.Fatal("released_at did not parse")
	}
}

// Additive evolution must not need a fleet update: an unknown field is ignored,
// not rejected.
func TestParseIgnoresUnknownFields(t *testing.T) {
	withExtra := strings.Replace(goodManifest, `"channel": "stable",`, `"channel": "stable", "cohort": "canary", "notes": {"a": 1},`, 1)
	if _, err := Parse([]byte(withExtra)); err != nil {
		t.Fatalf("Parse rejected an additive field: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct {
		body string
		is   error
	}{
		"not json":         {`{`, nil},
		"wrong schema":     {strings.Replace(goodManifest, `"manifest_version": 1`, `"manifest_version": 2`, 1), ErrWrongSchema},
		"no schema":        {strings.Replace(goodManifest, `"manifest_version": 1,`, ``, 1), ErrWrongSchema},
		"wrong channel":    {strings.Replace(goodManifest, `"stable"`, `"beta"`, 1), ErrWrongChannel},
		"brain not semver": {strings.Replace(goodManifest, `"brain": "1.4.2"`, `"brain": "latest"`, 1), nil},
		"ui empty":         {strings.Replace(goodManifest, `"ui": "1.4.2"`, `"ui": ""`, 1), nil},
		"min agent junk":   {strings.Replace(goodManifest, `"minimum_host_agent": "1.0.0"`, `"minimum_host_agent": "one"`, 1), nil},
		"rollback junk":    {strings.Replace(goodManifest, `"rollback_to": null`, `"rollback_to": {"brain": "x", "ui": "1.0.0"}`, 1), nil},
	}
	for name, c := range cases {
		_, err := Parse([]byte(c.body))
		if err == nil {
			t.Errorf("%s: Parse accepted it", name)
			continue
		}
		if c.is != nil && !errors.Is(err, c.is) {
			t.Errorf("%s: err = %v, want %v", name, err, c.is)
		}
	}
}

func TestParseReadsTheKillSwitch(t *testing.T) {
	body := strings.Replace(goodManifest, `"rollback_to": null`, `"rollback_to": {"brain": "1.4.1", "ui": "1.4.1"}`, 1)
	m, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.RollbackTo == nil || *m.RollbackTo != (Pair{Brain: "1.4.1", UI: "1.4.1"}) {
		t.Fatalf("rollback_to = %+v", m.RollbackTo)
	}
}

// --- decide -----------------------------------------------------------------

func TestDecide(t *testing.T) {
	current := Pair{Brain: "1.4.2", UI: "1.4.2"}
	older := Pair{Brain: "1.4.1", UI: "1.4.1"}
	base := Manifest{ManifestVersion: 1, Channel: "stable", Brain: "1.4.2", UI: "1.4.2", MinimumHostAgent: "1.0.0"}
	killed := base
	killed.RollbackTo = &older

	cases := []struct {
		name   string
		m      Manifest
		box    BoxState
		want   Action
		target Pair
	}{
		{"already current", base, BoxState{Running: current, HostAgentVersion: "1.0.0"}, ActionNone, Pair{}},
		{"newer available", base, BoxState{Running: older, HostAgentVersion: "1.0.0"}, ActionUpdate, current},
		{"host-agent newer than required", base, BoxState{Running: older, HostAgentVersion: "1.2.0"}, ActionUpdate, current},
		{"host-agent too old", base, BoxState{Running: older, HostAgentVersion: "0.9.0"}, ActionHoldHostAgentTooOld, Pair{}},
		{"host-agent unreadable fails closed", base, BoxState{Running: older, HostAgentVersion: "dev"}, ActionHoldHostAgentTooOld, Pair{}},
		{"retracted and applied", killed, BoxState{Running: current, HostAgentVersion: "1.0.0"}, ActionRollback, older},
		{"retracted but never applied", killed, BoxState{Running: older, HostAgentVersion: "1.0.0"}, ActionNone, Pair{}},
		// The way back is offered even to a box that is behind on host-agent:
		// the retracted version is what is hurting it now.
		{"retracted, host-agent old", killed, BoxState{Running: current, HostAgentVersion: "0.1.0"}, ActionRollback, older},
	}
	for _, c := range cases {
		got := Decide(c.m, c.box)
		if got.Action != c.want {
			t.Errorf("%s: action = %v, want %v", c.name, got.Action, c.want)
		}
		if got.Target != c.target {
			t.Errorf("%s: target = %+v, want %+v", c.name, got.Target, c.target)
		}
	}
}

// The versions in the file have no leading "v" and semver.Compare needs one.
// Getting this wrong makes every comparison return "not new enough", which
// reads as a quiet box rather than as a bug.
func TestDecideComparesVersionsWithoutTheVPrefix(t *testing.T) {
	m := Manifest{ManifestVersion: 1, Channel: "stable", Brain: "1.10.0", UI: "1.10.0", MinimumHostAgent: "1.9.0"}
	box := BoxState{Running: Pair{Brain: "1.9.0", UI: "1.9.0"}, HostAgentVersion: "1.9.0"}
	if got := Decide(m, box); got.Action != ActionUpdate {
		t.Fatalf("action = %v, want update (1.9.0 satisfies minimum 1.9.0)", got.Action)
	}
	// 1.10 is newer than 1.9 numerically; a string compare would say otherwise.
	box.HostAgentVersion = "1.10.0"
	m.MinimumHostAgent = "1.9.0"
	if got := Decide(m, box); got.Action != ActionUpdate {
		t.Fatalf("action = %v, want update", got.Action)
	}
}

// The running pair and the manifest are produced by different code paths, so
// "already current" is compared as versions, not as strings. A box recording
// "v1.4.2" for the same release the manifest writes as "1.4.2" must not be
// offered an update to what it is already running.
func TestDecideTreatsTheSameVersionWrittenTwoWaysAsCurrent(t *testing.T) {
	m := Manifest{ManifestVersion: 1, Channel: "stable", Brain: "1.4.2", UI: "1.4.2", MinimumHostAgent: "1.0.0"}
	box := BoxState{Running: Pair{Brain: "v1.4.2", UI: "v1.4.2"}, HostAgentVersion: "1.0.0"}
	if got := Decide(m, box); got.Action != ActionNone {
		t.Fatalf("action = %v, want none — the box already runs this release", got.Action)
	}
}

// --- cache ------------------------------------------------------------------

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newSigner(t)
	v := NewVerifier(s.publicKey(t))
	sig := s.sign([]byte(goodManifest), "t", true)

	if err := Save(dir, []byte(goodManifest), sig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	c, ok, err := Load(dir, v)
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if c.Manifest.Brain != "1.4.2" {
		t.Fatalf("brain = %q", c.Manifest.Brain)
	}
	if string(c.Raw) != goodManifest {
		t.Fatal("cached bytes changed on the round trip; the stored signature covers the publisher's exact bytes")
	}
	if c.Signature != sig {
		t.Fatal("cached signature changed on the round trip")
	}
}

func TestLoadWithNoCacheIsNotAnError(t *testing.T) {
	_, ok, err := Load(t.TempDir(), NewVerifier())
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v, want false/nil for a box that never fetched", ok, err)
	}
}

// Re-verifying on read is what stops the local file system being a way around
// the signature: anything that can write /var/lib/moose could otherwise choose
// the version the box runs.
func TestLoadRefusesACacheEditedOnDisk(t *testing.T) {
	dir := t.TempDir()
	s := newSigner(t)
	v := NewVerifier(s.publicKey(t))
	if err := Save(dir, []byte(goodManifest), s.sign([]byte(goodManifest), "t", true)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(ManifestPath(dir))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	edited := strings.Replace(string(b), `1.4.2`, `6.6.6`, 1)
	if edited == string(b) {
		t.Fatal("test bug: nothing was edited")
	}
	if err := os.WriteFile(ManifestPath(dir), []byte(edited), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, ok, err := Load(dir, v); ok || err == nil {
		t.Fatal("Load accepted a manifest edited on disk")
	}
}

func TestLoadRefusesACacheWithNoSignature(t *testing.T) {
	dir := t.TempDir()
	// A file holding only the manifest, with no signature beside it: exactly
	// what an operator dropping a manifest into place by hand would produce.
	body := `{"manifest": ` + strconv.Quote(goodManifest) + `}`
	if err := os.WriteFile(ManifestPath(dir), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok, err := Load(dir, NewVerifier()); ok || err == nil {
		t.Fatal("Load accepted a cache with no signature in it")
	}
}

func TestLoadRefusesAJunkCacheFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ManifestPath(dir), []byte("not json at all"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok, err := Load(dir, NewVerifier()); ok || err == nil {
		t.Fatal("Load accepted a cache file that is not a cache")
	}
}

// The manifest and its signature share ONE file so that saving them is ONE
// rename. Two files would mean two renames, and a power cut between them leaves
// the new manifest beside the old signature — a pair that cannot verify, with
// the previous good manifest already overwritten. RELEASE_MANIFEST.md
// # Failure modes promises the previous valid manifest stays in effect, and a
// box with no usable cache at all does not keep that promise.
func TestSaveWritesOneFileSoThePairCannotBeSplit(t *testing.T) {
	dir := t.TempDir()
	s := newSigner(t)
	if err := Save(dir, []byte(goodManifest), s.sign([]byte(goodManifest), "t", true)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != filepath.Base(ManifestPath(dir)) {
		t.Fatalf("files = %v, want exactly [%s] — a second file is a second rename, and a leftover temp file is a failed write leaving rubbish behind",
			names, filepath.Base(ManifestPath(dir)))
	}
}

// A second Save replaces the whole pair. The point is that the file is never a
// mix of two releases: after saving generation 2, nothing of generation 1 is
// readable, and generation 2 verifies on its own.
func TestSaveReplacesTheWholePair(t *testing.T) {
	dir := t.TempDir()
	s := newSigner(t)
	v := NewVerifier(s.publicKey(t))
	if err := Save(dir, []byte(goodManifest), s.sign([]byte(goodManifest), "t", true)); err != nil {
		t.Fatalf("Save gen1: %v", err)
	}
	gen2 := strings.Replace(goodManifest, `"brain": "1.4.2"`, `"brain": "1.5.0"`, 1)
	gen2 = strings.Replace(gen2, `"ui": "1.4.2"`, `"ui": "1.5.0"`, 1)
	if err := Save(dir, []byte(gen2), s.sign([]byte(gen2), "t", true)); err != nil {
		t.Fatalf("Save gen2: %v", err)
	}
	c, ok, err := Load(dir, v)
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if c.Manifest.Brain != "1.5.0" || c.Manifest.UI != "1.5.0" {
		t.Fatalf("loaded %+v, want the second generation", c.Manifest.Pair())
	}
}
