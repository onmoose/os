package profile

import "testing"

func TestHostedHostsAndURLs(t *testing.T) {
	const box = "cindy-fox"
	if got, want := HostedAppHost(box, "photos"), "photos.cindy-fox.onmoose.io"; got != want {
		t.Errorf("HostedAppHost = %q, want %q", got, want)
	}
	if got, want := HostedAppURL(box, "photos"), "https://photos.cindy-fox.onmoose.io"; got != want {
		t.Errorf("HostedAppURL = %q, want %q", got, want)
	}
	if got, want := HostedDashboardHost(box), "cindy-fox.onmoose.io"; got != want {
		t.Errorf("HostedDashboardHost = %q, want %q", got, want)
	}
}

// The cert must cover the dashboard apex *and* the per-app wildcard: a
// "*.<box-id>" wildcard covers "<slug>.<box-id>" but not the bare "<box-id>"
// parent, so the apex is a distinct subject.
func TestCertSubjects(t *testing.T) {
	got := CertSubjects("cindy-fox")
	want := []string{"cindy-fox.onmoose.io", "*.cindy-fox.onmoose.io"}
	if len(got) != len(want) {
		t.Fatalf("CertSubjects = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CertSubjects[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
