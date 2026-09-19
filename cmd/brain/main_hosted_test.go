package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

// fakeBoxMeta is an in-memory boxMetaStore for the hosted-seed ingestion tests.
// sets records the key write order so the commit-marker ordering (assertion key
// before box-id) can be asserted.
type fakeBoxMeta struct {
	m      map[string]string
	getErr map[string]error
	setErr map[string]error
	sets   []string
}

func newFakeBoxMeta() *fakeBoxMeta {
	return &fakeBoxMeta{m: map[string]string{}, getErr: map[string]error{}, setErr: map[string]error{}}
}

func (f *fakeBoxMeta) GetBoxMeta(key string) (string, error) {
	if err := f.getErr[key]; err != nil {
		return "", err
	}
	v, ok := f.m[key]
	if !ok {
		return "", store.ErrNotFound
	}
	return v, nil
}

func (f *fakeBoxMeta) SetBoxMeta(key, value string) error {
	if err := f.setErr[key]; err != nil {
		return err
	}
	f.m[key] = value
	f.sets = append(f.sets, key)
	return nil
}

func writeSeedFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return p
}

// testAssertionKey is an opaque base64 stand-in for the portal's verification key
// — loadHostedEnvironment persists it verbatim and never decodes it (decoding is
// cmd/brain's decodeAssertionKey, tested separately), so any string round-trips.
const testAssertionKey = "a2V5"

const validSeedJSON = `{"box_id":"cindy-fox","assertion_verification_key":"a2V5"}`

func TestLoadHostedEnvironment_ApplianceIsNoop(t *testing.T) {
	bm := newFakeBoxMeta()
	// A seed path that would error if read proves appliance never touches it.
	boxID, key, _ := loadHostedEnvironment(profile.Appliance, bm, "/nonexistent/seed.json")
	if boxID != "" || key != "" {
		t.Fatalf("appliance = (%q,%q); want empty", boxID, key)
	}
	if len(bm.sets) != 0 {
		t.Errorf("appliance wrote box_meta: %v", bm.sets)
	}
}

func TestLoadHostedEnvironment_FirstBootIngestsSeed(t *testing.T) {
	bm := newFakeBoxMeta()
	seedPath := writeSeedFile(t, validSeedJSON)

	boxID, key, _ := loadHostedEnvironment(profile.Hosted, bm, seedPath)
	if boxID != "cindy-fox" {
		t.Errorf("box_id = %q; want cindy-fox", boxID)
	}
	if key != testAssertionKey {
		t.Errorf("key = %q; want %q", key, testAssertionKey)
	}
	if bm.m[store.BoxMetaBoxID] != "cindy-fox" {
		t.Errorf("persisted box_id = %q", bm.m[store.BoxMetaBoxID])
	}
	if bm.m[store.BoxMetaAssertionKey] != testAssertionKey {
		t.Errorf("persisted key = %q", bm.m[store.BoxMetaAssertionKey])
	}
	// Commit-marker ordering: the assertion key must land before box-id.
	if len(bm.sets) != 2 || bm.sets[0] != store.BoxMetaAssertionKey || bm.sets[1] != store.BoxMetaBoxID {
		t.Errorf("write order = %v; want [assertion_key, box_id]", bm.sets)
	}
}

// A box-id already persisted is the install's frozen identity: subsequent boots
// load it (and the stored key) and ignore the seed entirely.
func TestLoadHostedEnvironment_FrozenIdentityIgnoresSeed(t *testing.T) {
	bm := newFakeBoxMeta()
	bm.m[store.BoxMetaBoxID] = "cindy-fox"
	bm.m[store.BoxMetaAssertionKey] = "storedkey"
	// A different seed on disk must NOT override the frozen identity.
	seedPath := writeSeedFile(t, `{"box_id":"rocky-owl","assertion_verification_key":"other"}`)

	boxID, key, _ := loadHostedEnvironment(profile.Hosted, bm, seedPath)
	if boxID != "cindy-fox" || key != "storedkey" {
		t.Fatalf("frozen identity = (%q,%q); want (cindy-fox, storedkey)", boxID, key)
	}
	if len(bm.sets) != 0 {
		t.Errorf("frozen-identity boot wrote box_meta: %v", bm.sets)
	}
}

func TestLoadHostedEnvironment_AbsentSeedStaysClosed(t *testing.T) {
	bm := newFakeBoxMeta()
	boxID, key, _ := loadHostedEnvironment(profile.Hosted, bm, filepath.Join(t.TempDir(), "missing.json"))
	if boxID != "" || key != "" {
		t.Fatalf("absent seed = (%q,%q); want empty (SSO stays closed)", boxID, key)
	}
	if len(bm.sets) != 0 {
		t.Errorf("absent seed wrote box_meta: %v", bm.sets)
	}
}

func TestLoadHostedEnvironment_MalformedSeedStaysClosed(t *testing.T) {
	bm := newFakeBoxMeta()
	seedPath := writeSeedFile(t, `{not valid json`)
	boxID, key, _ := loadHostedEnvironment(profile.Hosted, bm, seedPath)
	if boxID != "" || key != "" {
		t.Fatalf("malformed seed = (%q,%q); want empty", boxID, key)
	}
	if len(bm.sets) != 0 {
		t.Errorf("malformed seed wrote box_meta: %v", bm.sets)
	}
}

// Defensive: the key-before-box-id ordering makes a persisted box-id with no key
// unreachable, but if it ever happens (the key row gone, or a read error) SSO
// stays closed (empty key ⇒ 503) rather than opening — and never loads a usable
// identity without its verification key.
func TestLoadHostedEnvironment_FrozenIdentityMissingKeyStaysClosed(t *testing.T) {
	bm := newFakeBoxMeta()
	bm.m[store.BoxMetaBoxID] = "cindy-fox" // box-id present, key row absent
	boxID, key, _ := loadHostedEnvironment(profile.Hosted, bm, "/nonexistent/seed.json")
	if boxID != "cindy-fox" {
		t.Errorf("box_id = %q; want cindy-fox (identity still frozen)", boxID)
	}
	if key != "" {
		t.Errorf("key = %q; want empty so SSO stays closed", key)
	}
}

// A persist failure on the key leaves SSO closed and never writes box-id — so the
// next boot re-ingests cleanly rather than seeing a box-id with no key.
func TestLoadHostedEnvironment_KeyPersistFailureStaysClosed(t *testing.T) {
	bm := newFakeBoxMeta()
	bm.setErr[store.BoxMetaAssertionKey] = errors.New("disk full")
	seedPath := writeSeedFile(t, validSeedJSON)

	boxID, key, _ := loadHostedEnvironment(profile.Hosted, bm, seedPath)
	if boxID != "" || key != "" {
		t.Fatalf("key-persist failure = (%q,%q); want empty", boxID, key)
	}
	if _, ok := bm.m[store.BoxMetaBoxID]; ok {
		t.Error("box-id persisted despite key-persist failure")
	}
}

const seedWithEnrollmentJSON = `{"box_id":"cindy-fox","assertion_verification_key":"a2V5","enrollment":{"subdomain":"abc-123","username":"u","password":"p"}}`

// First boot with a complete enrollment block: it is returned for the cert pass,
// persisted as JSON, and written *before* the box-id commit marker (so a crash
// mid-write re-ingests rather than stranding a box-id with no enrollment).
func TestLoadHostedEnvironment_FirstBootIngestsEnrollment(t *testing.T) {
	bm := newFakeBoxMeta()
	seedPath := writeSeedFile(t, seedWithEnrollmentJSON)

	boxID, _, enr := loadHostedEnvironment(profile.Hosted, bm, seedPath)
	if boxID != "cindy-fox" {
		t.Fatalf("box_id = %q; want cindy-fox", boxID)
	}
	if !enr.Complete() || enr.Subdomain != "abc-123" || enr.Username != "u" || enr.Password != "p" {
		t.Errorf("enrollment = %+v; want {abc-123 u p}", enr)
	}
	// Persisted, and ordered key → enrollment → box-id (box-id is the marker).
	if len(bm.sets) != 3 || bm.sets[0] != store.BoxMetaAssertionKey ||
		bm.sets[1] != store.BoxMetaEnrollment || bm.sets[2] != store.BoxMetaBoxID {
		t.Errorf("write order = %v; want [assertion_key, enrollment, box_id]", bm.sets)
	}
}

// A complete enrollment that fails to persist aborts the ingest before the
// box-id commit marker — so the seed is re-ingested next boot rather than
// freezing an identity whose enrollment was never recorded (which would leave
// the box certless on every subsequent boot). Mirrors the key-persist abort.
func TestLoadHostedEnvironment_EnrollmentPersistFailureStaysClosed(t *testing.T) {
	bm := newFakeBoxMeta()
	bm.setErr[store.BoxMetaEnrollment] = errors.New("disk full")
	seedPath := writeSeedFile(t, seedWithEnrollmentJSON)

	boxID, key, enr := loadHostedEnvironment(profile.Hosted, bm, seedPath)
	if boxID != "" || key != "" || enr.Complete() {
		t.Fatalf("enrollment-persist failure = (%q,%q,%+v); want empty", boxID, key, enr)
	}
	if _, ok := bm.m[store.BoxMetaBoxID]; ok {
		t.Error("box-id committed despite enrollment-persist failure")
	}
}

// A seed with no enrollment still provisions SSO; the cert pass is skipped
// (incomplete enrollment) and no enrollment row is written.
func TestLoadHostedEnvironment_FirstBootNoEnrollmentSkips(t *testing.T) {
	bm := newFakeBoxMeta()
	seedPath := writeSeedFile(t, validSeedJSON) // no enrollment block

	boxID, _, enr := loadHostedEnvironment(profile.Hosted, bm, seedPath)
	if boxID != "cindy-fox" {
		t.Fatalf("box_id = %q; want cindy-fox", boxID)
	}
	if enr.Complete() {
		t.Errorf("enrollment = %+v; want incomplete", enr)
	}
	if _, ok := bm.m[store.BoxMetaEnrollment]; ok {
		t.Error("enrollment row written for a seed with no enrollment")
	}
	if len(bm.sets) != 2 { // assertion key, box_id only
		t.Errorf("write order = %v; want [assertion_key, box_id]", bm.sets)
	}
}

// Frozen-identity boot: the persisted enrollment is reloaded (the seed is
// ignored) so the cert pass can reconfigure Caddy without re-reading the seed.
func TestLoadHostedEnvironment_FrozenIdentityLoadsEnrollment(t *testing.T) {
	bm := newFakeBoxMeta()
	bm.m[store.BoxMetaBoxID] = "cindy-fox"
	bm.m[store.BoxMetaAssertionKey] = "storedkey"
	bm.m[store.BoxMetaEnrollment] = `{"subdomain":"abc-123","username":"u","password":"p"}`

	_, _, enr := loadHostedEnvironment(profile.Hosted, bm, "/nonexistent/seed.json")
	if !enr.Complete() || enr.Subdomain != "abc-123" {
		t.Errorf("reloaded enrollment = %+v; want {abc-123 u p}", enr)
	}
	if len(bm.sets) != 0 {
		t.Errorf("frozen-identity boot wrote box_meta: %v", bm.sets)
	}
}

// decodeAssertionKey accepts a valid 32-byte standard-base64 key and rejects a
// wrong-length or non-base64 value (SSO disabled rather than minting unverifiable
// checks).
func TestDecodeAssertionKey(t *testing.T) {
	// 32 bytes of base64-std = 44 chars; "AAAA..." (32 zero bytes) is valid.
	valid := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if k, err := decodeAssertionKey(valid); err != nil || len(k) != 32 {
		t.Fatalf("decode valid key = (%v, %v); want 32-byte key, no error", len(k), err)
	}
	if _, err := decodeAssertionKey("not base64!!"); err == nil {
		t.Error("decode non-base64 = nil error; want error")
	}
	if _, err := decodeAssertionKey("dG9vc2hvcnQ="); err == nil {
		t.Error("decode wrong-length key = nil error; want error")
	}
}
