package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// remap_base is additive and optional on the wire: an older host-agent (or the
// fake) sends no key, which decodes to nil, and a nil base encodes with no key
// at all, never as null or 0 (BRAIN_HOST_PROTOCOL.md # User info endpoints).
func TestWellKnownIdentityRemapBaseWire(t *testing.T) {
	var old WellKnownIdentityResponse
	if err := json.Unmarshal([]byte(`{"moose_app_uid":2000,"moose_app_gid":2000,"moose_shared_gid":2001}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.RemapBase != nil {
		t.Errorf("absent remap_base decoded to %d, want nil", *old.RemapBase)
	}

	var withBase WellKnownIdentityResponse
	if err := json.Unmarshal([]byte(`{"moose_app_uid":2000,"moose_app_gid":2000,"moose_shared_gid":2001,"remap_base":1000000}`), &withBase); err != nil {
		t.Fatal(err)
	}
	if withBase.RemapBase == nil || *withBase.RemapBase != 1000000 {
		t.Errorf("remap_base = %v, want 1000000", withBase.RemapBase)
	}

	b, err := json.Marshal(WellKnownIdentityResponse{MooseAppUID: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "remap_base") {
		t.Errorf("nil remap_base encoded as %s, want no key", b)
	}
}
