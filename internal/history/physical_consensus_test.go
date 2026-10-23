package history

import (
	"slices"
	"testing"
)

// RED: Each old nonblank row has one accounted outcome; only supported
// 1–3-physical-row replacement may drop old values before retention trimming.
func TestPhysicalOutcomeAccountingContract(t *testing.T) {
	for _, fixture := range physicalCases(t) {
		t.Run(fixture.Name, func(t *testing.T) { runPhysicalCase(t, fixture) })
	}
}

func TestPhysicalCoordinateOutcomeSets(t *testing.T) {
	for _, fixture := range physicalCases(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			plan := planPhysicalTransition(comparisonBody(fixture.Old), comparisonBody(fixture.Next), true)
			for class, indices := range map[uint8][]int{physicalMatched: fixture.Matched, physicalRetired: fixture.Retired, physicalReplaced: fixture.Replaced, physicalHeldOutcome: fixture.Held} {
				for _, i := range indices {
					if physicalText(fixture.Old[i]) != "" && plan.outcome[i] != class {
						t.Errorf("coordinate %d outcome=%d want=%d", i, plan.outcome[i], class)
					}
				}
			}
			d := plan.diagnostic
			if d.MatchedNonblank+d.RetiredNonblank+d.ReplacedNonblank+d.HeldNonblank != d.OldNonblank {
				t.Fatal("old nonblank accounting does not partition the body")
			}
		})
	}
}

func TestPhysicalEqualPunctuationSlotHasMatchedCoordinates(t *testing.T) {
	old := []string{"head", "x1", "---", "x2", "tail"}
	next := []string{"head", "y1", "---", "y2", "tail"}
	plan := planPhysicalTransition(old, next, true)
	if plan.outcome[2] != physicalMatched || plan.diagnostic.ReplacedRows != 2 || plan.diagnostic.MatchedNonblank != 3 {
		t.Fatal("equal punctuation lost its matched slot coordinate")
	}
}

func TestPhysicalVerifiedReverseWithHeaderAndCompetingOverlap(t *testing.T) {
	for _, test := range []struct {
		name               string
		archive, old, next []string
		retract            int
	}{
		{"header", []string{"A", "B"}, []string{"H1", "H2", "C", "D", "E", "F", "G"}, []string{"H1", "H2", "A", "B", "C", "D", "E"}, 2},
		{"competing", []string{"A", "B"}, []string{"A", "B", "C", "D"}, []string{"A", "B", "A", "B", "C", "D"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			m.AdmitSnapshot("p", snapshotPage(test.old...), ctx, 0)
			state := m.states["p"]
			state.History = append(slices.Clone(test.archive), state.History...)
			state.LiveStart = len(test.archive)
			_, _, d := m.AdmitSnapshot("p", snapshotPage(test.next...), ctx, 0)
			if d.RetractedRows != test.retract {
				t.Fatalf("retracted=%d want=%d", d.RetractedRows, test.retract)
			}
		})
	}
}

func TestPhysicalSparseAndLinearConsensusAgainstOracle(t *testing.T) {
	alphabet := []string{"A", "B", "", "{", " "}
	var pages [][]string
	var generate func([]string, int)
	generate = func(prefix []string, left int) {
		pages = append(pages, slices.Clone(prefix))
		if left == 0 {
			return
		}
		for _, row := range alphabet {
			generate(append(slices.Clone(prefix), row), left-1)
		}
	}
	generate(nil, 3)
	for _, old := range pages {
		for _, next := range pages {
			wantAgreed, wantFeasible := physicalConsensusOracle(old, next)
			got := physicalConsensus(old, next, false)
			if !slices.Equal(got.agreed, wantAgreed) || !slices.Equal(got.feasible, wantFeasible) {
				t.Fatalf("sparse old=%q next=%q agreed=%v feasible=%v want %v/%v", old, next, got.agreed, got.feasible, wantAgreed, wantFeasible)
			}
			fast := physicalConsensus(old, next, true)
			if !slices.Equal(fast.agreed, wantAgreed) || !slices.Equal(fast.feasible, wantFeasible) {
				t.Fatalf("linear old=%q next=%q got=%v/%v want=%v/%v", old, next, fast.agreed, fast.feasible, wantAgreed, wantFeasible)
			}
		}
	}
}
