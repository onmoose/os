package catalog

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// remapApps is segApps on the hosted surface with beta marked root_setup and
// a new delta marked image_user. beta is featured, the spotlight and the only
// app of the media group; delta is the only app of the notes category.
func remapApps() []wireApp {
	apps := segApps()
	for i := range apps {
		if apps[i].ID == "beta" {
			apps[i].RootSetup = true
		}
	}
	return append(apps, wireApp{
		ID: "delta", Name: "Delta", Version: "4.0",
		ShortDescription: "takes notes",
		Categories:       []string{"notes"},
		Featured:         true, Rank: rankPtr(3),
		ImageUser:   true,
		ManifestURL: "/catalog/apps/delta/manifest",
		ComposeURL:  "/catalog/apps/delta/compose",
	})
}

func remapHome() wireHomePage {
	h := homeFixture()
	h.Groups = append(h.Groups, wireHomeGroup{Category: "notes", Apps: []string{"delta", "gamma"}})
	return h
}

// syncedRemapCatalog serves every remapApps record, with no environment
// filter, from a fake catalog service.
func syncedRemapCatalog(t *testing.T) *Catalog {
	t.Helper()
	srv := newFakeCPHome(t, remapApps(), remapHome()).server()
	t.Cleanup(srv.Close)
	rs := newRemote(srv.URL, "hosted", t.TempDir())
	if err := rs.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce: %v", err)
	}
	return &Catalog{src: rs}
}

// WithoutRemapApps leaves the root_setup and image_user apps out of every
// store list, and the unfiltered catalog still shows them.
func TestWithoutRemapAppsHidesFromLists(t *testing.T) {
	full := syncedRemapCatalog(t)
	c := full.WithoutRemapApps()

	all, _ := full.List()
	if got := ids(all); !reflect.DeepEqual(got, []string{"alpha", "beta", "delta", "gamma"}) {
		t.Fatalf("unfiltered List = %v, want every app", got)
	}
	list, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(list); !reflect.DeepEqual(got, []string{"alpha", "gamma"}) {
		t.Errorf("List = %v, want [alpha gamma]", got)
	}

	h, err := c.Home()
	if err != nil {
		t.Fatal(err)
	}
	if h.Spotlight != nil {
		t.Errorf("spotlight = %q, want none (beta is root_setup)", h.Spotlight.ID)
	}
	if got := ids(h.Featured); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Errorf("featured = %v, want [alpha]", got)
	}
	var groups []string
	for _, g := range h.Groups {
		groups = append(groups, g.Category+":"+joinIDs(g.Apps))
	}
	if want := []string{"tools:alpha,gamma", "notes:gamma"}; !reflect.DeepEqual(groups, want) {
		t.Errorf("groups = %v, want %v (media emptied and dropped)", groups, want)
	}
	// media's only other app is alpha, so media keeps its pill; notes had
	// only delta, so its pill goes.
	if got := catIDs(h.Categories); !reflect.DeepEqual(got, []string{"media", "tools"}) {
		t.Errorf("categories = %v, want [media tools]", got)
	}

	if _, err := c.Category("notes"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Category(notes) = %v, want ErrNotFound", err)
	}
	media, err := c.Category("media")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(media.Apps); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Errorf("Category(media) = %v, want [alpha]", got)
	}
	if got, _ := c.Search("takes notes"); len(got.Apps) != 0 {
		t.Errorf("Search(takes notes) = %v, want nothing", ids(got.Apps))
	}
	if got, _ := full.Search("takes notes"); !reflect.DeepEqual(ids(got.Apps), []string{"delta"}) {
		t.Errorf("unfiltered Search = %v, want [delta]", ids(got.Apps))
	}
}

// A direct link still resolves: the by-id lookups are not filtered.
func TestWithoutRemapAppsKeepsByID(t *testing.T) {
	c := syncedRemapCatalog(t).WithoutRemapApps()
	if _, err := c.Detail("beta"); err != nil {
		t.Errorf("Detail(beta) = %v, want it to resolve", err)
	}
	if _, err := c.Entry("beta"); err != nil {
		t.Errorf("Entry(beta) = %v, want it to resolve", err)
	}
}

// The disk source reads the two fields from the manifest itself.
func TestWithoutRemapAppsDisk(t *testing.T) {
	root := t.TempDir()
	write := func(id, extra string) {
		writeApp(t, root, id, "id: "+id+"\nmanifest_version: 1\nname: "+id+"\nversion: \"1.0\"\n"+
			"compose_file: compose.yml\nmain_service: web\nmain_port: 80\n"+extra)
	}
	write("plain", "")
	write("caps", "root_setup: true\n")
	write("own", "image_user: true\n")
	c := New(root)
	all, _ := c.List()
	if len(all) != 3 {
		t.Fatalf("unfiltered List = %v, want 3 apps", ids(all))
	}
	got, _ := c.WithoutRemapApps().List()
	if !reflect.DeepEqual(ids(got), []string{"plain"}) {
		t.Errorf("List = %v, want [plain]", ids(got))
	}
}

func joinIDs(es []Entry) string {
	s := ""
	for i, e := range es {
		if i > 0 {
			s += ","
		}
		s += e.ID
	}
	return s
}
