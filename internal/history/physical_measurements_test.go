package history

import (
	"fmt"
	"testing"
)

func TestPhysicalReplayMeasurements(t *testing.T) {
	for _, name := range []string{"physical-pages-chunk17", "physical-pages-desktop-shapes", "physical-pages-markdown", "physical-clear-gap-fill"} {
		t.Run(name, func(t *testing.T) {
			f := loadPhysicalReplay(t, name)
			m := NewManager(t.TempDir())
			classes := map[string]int{}
			freezes, boundaries, retracted, added := 0, 0, 0, 0
			for i := range f.Pages {
				_, _, d := m.AdmitSnapshot("p", f.raw(i), f.context(i), 0)
				classes[d.Class]++
				if d.Class == "freeze" {
					freezes++
				}
				if i > 0 && d.Class == "boundary" {
					boundaries++
				}
				retracted += d.RetractedRows
				added += d.HeldAddedRows
				if name == "physical-pages-desktop-shapes" && (i == 23 || i == 261 || i == 266) || name == "physical-clear-gap-fill" && i == 1 {
					t.Logf("transition %d->%d class=%s H/N=%d/%d retired=%d replaced=%d", i-1, i, d.Class, d.HeldNonblank, d.OldNonblank, d.RetiredRows, d.ReplacedRows)
				}
				if name == "physical-pages-desktop-shapes" && d.HeldNonblank > 0 && i != 261 && i != 266 && 5*d.HeldNonblank > d.OldNonblank {
					t.Fatalf("held share exceeds measured 0.20 at %d", i)
				}
				if d.HeldNonblank > 0 {
					t.Logf("held transition %d->%d H/N=%d/%d physical=%d", i-1, i, d.HeldNonblank, d.OldNonblank, d.HeldAddedRows)
				}
			}
			state := m.states["p"]
			t.Logf("held nonblank=%d", physicalNonblank(physicalHeldRows(t, state)))
			t.Logf("%s archive=%d held=%d added=%d freezes=%d boundaries=%d retracted=%d classes=%v", name, state.LiveStart, heldRowCount(state.Held), added, freezes, boundaries, retracted, classes)
			if name == "physical-pages-chunk17" && freezes != 0 || name == "physical-pages-markdown" && freezes != 0 || name == "physical-pages-desktop-shapes" && (freezes != 1 || boundaries != 1) {
				t.Fatal("unexpected replay freeze/boundary count")
			}
			for _, bc := range physicalBenchCases() {
				if name == "physical-pages-chunk17" {
					r := planPhysicalTransition(comparisonBody(bc.old), comparisonBody(bc.next), true)
					t.Log(fmt.Sprintf("bench %s old=%d new=%d pairs=%d path=%s class=%s", bc.name, len(bc.old), len(bc.next), r.consensus.pairs, r.diagnostic.MatchPath, r.diagnostic.Class))
				}
			}
		})
	}
}
