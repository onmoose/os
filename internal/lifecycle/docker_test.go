package lifecycle

import (
	"testing"
	"time"
)

// TestParseManagedContainers reads captured `docker inspect` output in the
// ManagedContainers format, covering each health status Docker prints and a
// container with no healthcheck ("none").
func TestParseManagedContainers(t *testing.T) {
	out := `app1 web true 2026-10-12T23:28:10.876641533Z starting
app1 db true 2026-10-12T23:28:11.043329872Z none
app2 web true 2026-10-12T23:00:00Z healthy
app3 web false 0001-01-01T00:00:00Z unhealthy
malformed line
`
	got := parseManagedContainers(out)
	want := []ManagedContainer{
		{InstanceID: "app1", Service: "web", Running: true, StartedAt: time.Date(2026, 10, 12, 23, 28, 10, 876641533, time.UTC), Health: "starting"},
		{InstanceID: "app1", Service: "db", Running: true, StartedAt: time.Date(2026, 10, 12, 23, 28, 11, 43329872, time.UTC), Health: ""},
		{InstanceID: "app2", Service: "web", Running: true, StartedAt: time.Date(2026, 10, 12, 23, 0, 0, 0, time.UTC), Health: "healthy"},
		{InstanceID: "app3", Service: "web", Running: false, StartedAt: time.Time{}, Health: "unhealthy"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d containers, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].StartedAt.Equal(want[i].StartedAt) {
			t.Errorf("[%d] StartedAt = %v, want %v", i, got[i].StartedAt, want[i].StartedAt)
		}
		got[i].StartedAt, want[i].StartedAt = time.Time{}, time.Time{}
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if parseManagedContainers("") != nil {
		t.Error("empty output must parse to no containers")
	}
}
