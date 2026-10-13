package catalog

import (
	"github.com/onmoose/os/internal/fit"
	"github.com/onmoose/os/internal/manifest"
)

// FitCheck computes one app's fit on this box from the memory and CPU it
// recommends. The API builds it once per request from a single reading of the
// box (lifecycle.FitBox), so every card on a page is judged against the same
// figures.
type FitCheck func(id string, rec manifest.Recommended) fit.AppFit

// WithFit returns a view of the catalog that fills Entry.Fit on every entry
// it returns, in the lists (List, and so Home, Category and Search, plus the
// featured row, the spotlight, the groups, the packs and the landing sections)
// and in the by-id Entry and Detail (#599). It hides nothing: an app that is
// too big for this box still shows, with the state on its card, so the owner
// sees it before opening it.
func (c *Catalog) WithFit(check FitCheck) *Catalog {
	return &Catalog{src: fitView{source: c.src, check: check}}
}

// fitView wraps a source and fills Fit on the entries it returns. Load and the
// asset paths are the embedded source's, unchanged.
type fitView struct {
	source
	check FitCheck
}

func (f fitView) fill(e *Entry) {
	r := f.check(e.ID, e.recommended)
	e.Fit = &r
}

func (f fitView) fillAll(in []Entry) []Entry {
	for i := range in {
		f.fill(&in[i])
	}
	return in
}

func (f fitView) fillPacks(in []Pack) []Pack {
	for i := range in {
		f.fillAll(in[i].Apps)
	}
	return in
}

func (f fitView) List() ([]Entry, error) {
	apps, err := f.source.List()
	return f.fillAll(apps), err
}

func (f fitView) Entry(id string) (Entry, error) {
	e, err := f.source.Entry(id)
	if err == nil {
		f.fill(&e)
	}
	return e, err
}

func (f fitView) Detail(id string) (Detail, error) {
	d, err := f.source.Detail(id)
	if err == nil {
		f.fill(&d.Entry)
	}
	return d, err
}

func (f fitView) featured() ([]Entry, error) {
	apps, err := f.source.featured()
	return f.fillAll(apps), err
}

func (f fitView) home() (*Entry, []HomeGroupView, error) {
	spotlight, groups, err := f.source.home()
	if err != nil {
		return nil, nil, err
	}
	if spotlight != nil {
		f.fill(spotlight)
	}
	for i := range groups {
		f.fillAll(groups[i].Apps)
	}
	return spotlight, groups, nil
}

func (f fitView) packs() ([]Pack, error) {
	packs, err := f.source.packs()
	return f.fillPacks(packs), err
}

func (f fitView) sections() ([]HomeSection, error) {
	sections, err := f.source.sections()
	if err != nil {
		return nil, err
	}
	for i := range sections {
		sec := &sections[i]
		for j := range sec.Slides {
			f.fill(&sec.Slides[j].App)
		}
		f.fillPacks(sec.Packs)
		for j := range sec.Groups {
			f.fillAll(sec.Groups[j].Apps)
		}
	}
	return sections, nil
}
