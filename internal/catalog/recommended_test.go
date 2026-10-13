package catalog

import (
	"testing"

	"github.com/onmoose/os/internal/manifest"
)

// The browse record's resources block reaches Detail.Recommended, the input
// to the detail route's fit (#599). A record with no block recommends nothing.
func TestRecommendedFromRecord(t *testing.T) {
	rec := manifest.Recommended{Memory: "8G", CPU: 2}
	with := wireApp{ID: "a", Name: "A", Version: "1", Resources: &manifest.Resources{Recommended: rec}}
	if got := detailOfApp(&with).Recommended(); got != rec {
		t.Errorf("detail recommended = %+v, want %+v", got, rec)
	}
	without := wireApp{ID: "b", Name: "B", Version: "1"}
	if got := detailOfApp(&without).Recommended(); got != (manifest.Recommended{}) {
		t.Errorf("no block: recommended = %+v, want zero", got)
	}
}
