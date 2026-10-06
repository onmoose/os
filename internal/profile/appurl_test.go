package profile

import "testing"

// A box-id is the owner's chosen name used as given ("andrei"); a box named
// under the earlier rule keeps its dashed id ("cindy-fox"). Both must build
// the same shape of host and URL.
func TestHostedHostsAndURLs(t *testing.T) {
	tests := []struct {
		box, wantApp, wantURL, wantDash string
	}{
		{
			box:      "andrei",
			wantApp:  "photos.andrei.onmoose.io",
			wantURL:  "https://photos.andrei.onmoose.io",
			wantDash: "andrei.onmoose.io",
		},
		{
			box:      "cindy-fox",
			wantApp:  "photos.cindy-fox.onmoose.io",
			wantURL:  "https://photos.cindy-fox.onmoose.io",
			wantDash: "cindy-fox.onmoose.io",
		},
	}
	for _, tt := range tests {
		t.Run(tt.box, func(t *testing.T) {
			if got := HostedAppHost(tt.box, "photos"); got != tt.wantApp {
				t.Errorf("HostedAppHost = %q, want %q", got, tt.wantApp)
			}
			if got := HostedAppURL(tt.box, "photos"); got != tt.wantURL {
				t.Errorf("HostedAppURL = %q, want %q", got, tt.wantURL)
			}
			if got := HostedDashboardHost(tt.box); got != tt.wantDash {
				t.Errorf("HostedDashboardHost = %q, want %q", got, tt.wantDash)
			}
		})
	}
}

// The cert must cover the dashboard apex *and* the per-app wildcard: a
// "*.<box-id>" wildcard covers "<slug>.<box-id>" but not the bare "<box-id>"
// parent, so the apex is a distinct subject.
func TestCertSubjects(t *testing.T) {
	tests := []struct {
		box  string
		want []string
	}{
		{box: "andrei", want: []string{"andrei.onmoose.io", "*.andrei.onmoose.io"}},
		{box: "cindy-fox", want: []string{"cindy-fox.onmoose.io", "*.cindy-fox.onmoose.io"}},
	}
	for _, tt := range tests {
		t.Run(tt.box, func(t *testing.T) {
			got := CertSubjects(tt.box)
			if len(got) != len(tt.want) {
				t.Fatalf("CertSubjects = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("CertSubjects[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
