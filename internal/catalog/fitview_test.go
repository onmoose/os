package catalog

import (
	"context"
	"testing"

	"github.com/onmoose/os/internal/fit"
	"github.com/onmoose/os/internal/manifest"
)

// WithFit fills every entry's fit from the resources its browse record
// carries, in the lists and in the by-id views, and hides nothing.
func TestWithFitFillsEveryView(t *testing.T) {
	apps := segApps()
	for i := range apps {
		if apps[i].ID == "beta" {
			apps[i].Resources = &manifest.Resources{Recommended: manifest.Recommended{Memory: "8G", CPU: 2}}
		}
	}
	srv := newFakeCPHome(t, apps, homeFixture()).server()
	t.Cleanup(srv.Close)
	rs := newRemote(srv.URL, "hosted", t.TempDir())
	if err := rs.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce: %v", err)
	}
	box := fit.Box{MemTotalBytes: 4 << 30, Cores: 4}
	var seen []manifest.Recommended
	c := (&Catalog{src: rs}).WithFit(func(id string, rec manifest.Recommended) fit.AppFit {
		if id == "beta" {
			seen = append(seen, rec)
		}
		mem, _ := rec.MemoryBytes()
		return fit.Check(box, mem, rec.CPU)
	})

	check := func(where string, e *Entry) {
		t.Helper()
		if e.Fit == nil {
			t.Errorf("%s: %s carries no fit", where, e.ID)
			return
		}
		want := fit.Fits
		if e.ID == "beta" {
			want = fit.TooBig
		}
		if e.Fit.State != want {
			t.Errorf("%s: %s fit = %s, want %s", where, e.ID, e.Fit.State, want)
		}
	}
	list, err := c.List()
	if err != nil || len(list) != len(apps) {
		t.Fatalf("List = %d entries, %v; want every app", len(list), err)
	}
	for i := range list {
		check("list", &list[i])
	}
	home, err := c.Home()
	if err != nil {
		t.Fatalf("Home: %v", err)
	}
	if home.Spotlight == nil {
		t.Fatal("home has no spotlight")
	}
	check("spotlight", home.Spotlight)
	for i := range home.Featured {
		check("featured", &home.Featured[i])
	}
	for _, g := range home.Groups {
		for i := range g.Apps {
			check("group "+g.Category, &g.Apps[i])
		}
	}
	d, err := c.Detail("beta")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	check("detail", &d.Entry)
	if len(seen) == 0 || seen[0] != (manifest.Recommended{Memory: "8G", CPU: 2}) {
		t.Errorf("beta's check saw %+v, want the record's 8G and 2 cores", seen)
	}
}
