package api

import (
	"testing"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

// toDTO surfaces the per-app URL by profile: hosted is public HTTPS at
// "<slug>.<box-id>.onmoose.io" (the sole scheme), appliance is plain-HTTP
// ".local" (the published mDNS name when present).
func TestToDTO_URLByProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile profile.Profile
		boxID   string
		inst    store.Instance
		wantURL string
	}{
		{
			name:    "appliance primary .local",
			profile: profile.Appliance,
			inst:    store.Instance{ID: "1", Slug: "photos"},
			wantURL: "http://photos.local",
		},
		{
			name:    "appliance prefers published mDNS name",
			profile: profile.Appliance,
			inst:    store.Instance{ID: "1", Slug: "photos", MDNSName: "photos-box.local"},
			wantURL: "http://photos-box.local",
		},
		{
			name:    "hosted public HTTPS, sole scheme",
			profile: profile.Hosted,
			boxID:   "cindy-fox",
			inst:    store.Instance{ID: "1", Slug: "photos", MDNSName: "ignored.local"},
			wantURL: "https://photos.cindy-fox.onmoose.io",
		},
		{
			name:    "hosted without box-id falls back to appliance scheme",
			profile: profile.Hosted,
			boxID:   "",
			inst:    store.Instance{ID: "1", Slug: "photos"},
			wantURL: "http://photos.local",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{profile: tt.profile, boxID: tt.boxID}
			got := s.toDTO(tt.inst, "", nil)
			if got.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", got.URL, tt.wantURL)
			}
		})
	}
}

// toDTO fills IconURL, IconGlyph, and ShortDescription from the catalog entry
// when one is given, and leaves them empty for a Door-2 custom app that has
// none (#487) — the installed-apps list must show no description line for
// those rather than a stale or blank one.
func TestToDTO_ShortDescriptionFromCatalogEntry(t *testing.T) {
	s := &Server{profile: profile.Appliance}
	inst := store.Instance{ID: "1", Slug: "photos"}

	withEntry := s.toDTO(inst, "", &catalog.Entry{
		IconURL:          "/api/v1/catalog/photos/icon",
		IconGlyph:        "camera",
		ShortDescription: "Self-hosted photo backup",
	})
	if withEntry.ShortDescription != "Self-hosted photo backup" {
		t.Errorf("ShortDescription = %q, want catalog tagline", withEntry.ShortDescription)
	}

	withoutEntry := s.toDTO(inst, "", nil)
	if withoutEntry.ShortDescription != "" {
		t.Errorf("ShortDescription = %q, want empty for custom app with no catalog entry", withoutEntry.ShortDescription)
	}
}
