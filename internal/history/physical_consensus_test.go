package history

import (
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"
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

func TestPhysicalReverseRetainsPhysicalHeaderAndBlankFirstDiagonal(t *testing.T) {
	for _, test := range []struct {
		name      string
		old, next []string
	}{
		{"blank-header", []string{"H1", "", "H2", "C", "D", "E", "F", "G"}, []string{"H1", "", "H2", "A", "B", "C", "D", "E"}},
		{"punctuation-header", []string{"H1", "---", "H2", "C", "D", "E", "F", "G"}, []string{"H1", "---", "H2", "A", "B", "C", "D", "E"}},
		{"blank-first-diagonal", []string{"H1", "H2", "", "C", "D", "E", "F", "G"}, []string{"H1", "H2", "A", "B", "", "C", "D", "E"}},
		{"blank-first-without-header", []string{"", "C", "D", "E", "F", "G"}, []string{"A", "B", "", "C", "D", "E"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			m.AdmitSnapshot("p", snapshotPage(test.old...), ctx, 0)
			state := m.states["p"]
			state.History = append([]string{"A", "B"}, state.History...)
			state.LiveStart = 2
			got, _, d := m.AdmitSnapshot("p", snapshotPage(test.next...), ctx, 0)
			if d.RetractedRows != 2 || state.LiveStart != 0 || len(state.History[:state.LiveStart]) != 0 {
				t.Errorf("reverse archive=%q live=%d retracted=%d, want empty archive and two retracted rows", state.History[:state.LiveStart], state.LiveStart, d.RetractedRows)
			}
			if !slices.Equal(physicalHeldRows(t, state), []string{"F", "G"}) {
				t.Errorf("reverse held=%q, want F,G", physicalHeldRows(t, state))
			}
			if got != snapshotPage(test.next...) || !slices.Equal(state.History, test.next) {
				t.Error("reverse composition does not equal exact new body")
			}
		})
	}
}

func TestPhysicalForwardBlankBoundaryUnionOnce(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	m.AdmitSnapshot("p", snapshotPage("A", "B", "", "C", "D"), ctx, 0)
	for _, body := range [][]string{{"", "C", "D", "E", "F"}, {"C", "D", "E", "F", "G"}} {
		got, _, d := m.AdmitSnapshot("p", snapshotPage(body...), ctx, 0)
		state := m.states["p"]
		if !slices.Equal(state.History[:state.LiveStart], []string{"A", "B", ""}) {
			t.Errorf("archived boundary=%q, want A,B and separator blank once", state.History[:state.LiveStart])
		}
		if got != snapshotPage(append([]string{"A", "B", ""}, body...)...) {
			t.Error("forward response lost blank-boundary layout")
		}
		if len(state.Held) != 0 || d.Class == "freeze" {
			t.Error("supported blank-boundary scroll created residue or freeze")
		}
	}
	if !slices.Equal(m.states["p"].History, []string{"A", "B", "", "C", "D", "E", "F", "G"}) {
		t.Fatal("final stored layout lost or repeated the separator")
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
			got := physicalConsensus(physicalComparisonRows(old), physicalComparisonRows(next), false)
			if !slices.Equal(got.agreed, wantAgreed) || !slices.Equal(got.feasible, wantFeasible) {
				t.Fatalf("sparse old=%q next=%q agreed=%v feasible=%v want %v/%v", old, next, got.agreed, got.feasible, wantAgreed, wantFeasible)
			}
			fast := physicalConsensus(physicalComparisonRows(old), physicalComparisonRows(next), true)
			if !slices.Equal(fast.agreed, wantAgreed) || !slices.Equal(fast.feasible, wantFeasible) {
				t.Fatalf("linear old=%q next=%q got=%v/%v want=%v/%v", old, next, fast.agreed, fast.feasible, wantAgreed, wantFeasible)
			}
		}
	}
}

func TestRetiredAndHeldRowsOwnTheirStringStorage(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	raw := snapshotPage("archive A", "archive B", "anchor C", "anchor D")
	m.AdmitSnapshot("p", raw, ctx, 0)
	old := m.states["p"].History[0]
	m.AdmitSnapshot("p", snapshotPage("anchor C", "anchor D", "new E", "new F"), ctx, 0)
	if unsafe.StringData(old) == unsafe.StringData(m.states["p"].History[0]) {
		t.Error("retired row still pins the sampled page string")
	}
	state := &PaneState{}
	page := "held row\n" + strings.Repeat("x", 4096)
	row := strings.Split(page, "\n")[0]
	appendHeldRows(state, []string{row}, 0, 0, time.Now())
	if unsafe.StringData(row) == unsafe.StringData(state.Held[0].Rows[0]) {
		t.Error("held row still pins the sampled page string")
	}
}
