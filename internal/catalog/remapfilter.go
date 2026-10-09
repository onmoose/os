package catalog

// WithoutRemapApps returns a view of the catalog that leaves out every app
// that needs the daemon-wide userns-remap (root_setup or image_user) from the
// store lists: List, and so Home, Category and Search, plus the featured row,
// the spotlight, the home groups and the landing sections. A category left
// with no app loses its pill, and an emptied group is dropped, the same way
// the environment filter empties them. A pack holding such an app drops whole,
// from the pack list and from every section, because a pack is a promise of
// all its apps (APP_STORE.md # Apps this box cannot run).
//
// The by-id lookups are not filtered. Entry, Detail, Load and the asset routes
// resolve such an app as before, so a direct link to its detail page still
// loads, and the install plan (which reads the manifest itself) says why it
// cannot be installed. The install path refuses it on its own
// (lifecycle.ErrRootSetupNeedsRemap, ErrImageUserNeedsRemap).
//
// The API calls this only when it knows the box has no remap. It never hides
// on a guess.
func (c *Catalog) WithoutRemapApps() *Catalog {
	return &Catalog{src: remapFilter{c.src}}
}

// remapFilter wraps a source and drops the apps that need the remap from its
// list views. Every by-id method (Entry, Detail, Load, the asset paths) is
// the embedded source's, unchanged, on purpose: see WithoutRemapApps.
type remapFilter struct{ source }

func keepEntries(in []Entry) []Entry {
	if in == nil {
		return nil
	}
	out := make([]Entry, 0, len(in))
	for _, e := range in {
		if !e.needsRemap {
			out = append(out, e)
		}
	}
	return out
}

func (f remapFilter) List() ([]Entry, error) {
	apps, err := f.source.List()
	return keepEntries(apps), err
}

func (f remapFilter) featured() ([]Entry, error) {
	apps, err := f.source.featured()
	return keepEntries(apps), err
}

func (f remapFilter) home() (*Entry, []HomeGroupView, error) {
	spotlight, groups, err := f.source.home()
	if err != nil {
		return nil, nil, err
	}
	if spotlight != nil && spotlight.needsRemap {
		spotlight = nil
	}
	var kept []HomeGroupView
	for _, g := range groups {
		g.Apps = keepEntries(g.Apps)
		if len(g.Apps) > 0 {
			kept = append(kept, g)
		}
	}
	return spotlight, kept, nil
}

// packOK reports whether every app of a pack can run on this box.
func packOK(p Pack) bool {
	for _, a := range p.Apps {
		if a.needsRemap {
			return false
		}
	}
	return true
}

func keepPacks(in []Pack) []Pack {
	var out []Pack
	for _, p := range in {
		if packOK(p) {
			out = append(out, p)
		}
	}
	return out
}

func (f remapFilter) packs() ([]Pack, error) {
	packs, err := f.source.packs()
	return keepPacks(packs), err
}

// sections drops the apps that need the remap from slides and groups, and the
// packs that hold one. A slide list, group or section left empty drops whole,
// as when the snapshot is resolved. A search section stays.
func (f remapFilter) sections() ([]HomeSection, error) {
	sections, err := f.source.sections()
	if err != nil {
		return nil, err
	}
	var out []HomeSection
	for _, sec := range sections {
		if sec.Type == SectionSearch {
			out = append(out, sec)
			continue
		}
		var slides []Slide
		for _, sl := range sec.Slides {
			if !sl.App.needsRemap {
				slides = append(slides, sl)
			}
		}
		var groups []HomeGroupView
		for _, g := range sec.Groups {
			g.Apps = keepEntries(g.Apps)
			if len(g.Apps) > 0 {
				groups = append(groups, g)
			}
		}
		sec.Slides, sec.Packs, sec.Groups = slides, keepPacks(sec.Packs), groups
		if len(sec.Slides)+len(sec.Packs)+len(sec.Groups) > 0 {
			out = append(out, sec)
		}
	}
	return out, nil
}
