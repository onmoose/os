package hostagent

import (
	"errors"
	"net/http"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

// stubTZSetter records the last zone and returns a canned error, standing in
// for the real timedatectl-backed Setter (which only exists on a booted host).
type stubTZSetter struct {
	zone   string
	called bool
	err    error
}

func (s *stubTZSetter) SetTimezone(zone string) error {
	s.called = true
	s.zone = zone
	return s.err
}

// With no setter wired (the cmd/host-agent fake / dev loop), set-timezone is an
// accepted no-op so the brain's wizard flow works end-to-end without a host.
func TestSetTimezone_FakeNoOp(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{valid: true})
	if a.Timezone != nil {
		t.Fatal("fake agent unexpectedly has a Timezone setter wired")
	}
	w := post(t, mux, "/v1/system/set-timezone", protocol.SetTimezoneRequest{Zone: "Europe/Stockholm"})
	if w.Code != http.StatusOK {
		t.Fatalf("fake set-timezone = %d; want 200", w.Code)
	}
}

func TestSetTimezone_EmptyZone400(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{valid: true})
	w := post(t, mux, "/v1/system/set-timezone", protocol.SetTimezoneRequest{Zone: ""})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty zone = %d; want 400", w.Code)
	}
}

func TestSetTimezone_WiredSetterApplies(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{valid: true})
	setter := &stubTZSetter{}
	a.Timezone = setter
	w := post(t, mux, "/v1/system/set-timezone", protocol.SetTimezoneRequest{Zone: "UTC"})
	if w.Code != http.StatusOK {
		t.Fatalf("wired set-timezone = %d; want 200", w.Code)
	}
	if !setter.called || setter.zone != "UTC" {
		t.Errorf("setter called=%v zone=%q; want called with UTC", setter.called, setter.zone)
	}
}

func TestSetTimezone_WiredSetterError500(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{valid: true})
	a.Timezone = &stubTZSetter{err: errors.New("timedatectl exploded")}
	w := post(t, mux, "/v1/system/set-timezone", protocol.SetTimezoneRequest{Zone: "UTC"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("setter error = %d; want 500", w.Code)
	}
}
