package history

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
)

func repeatedBlockFixture(t *testing.T) ([]string, []string) {
	t.Helper()
	data, err := os.ReadFile("testdata/physical-pages-repeated-block.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Previous []string `json:"previous_body"`
		Next     []string `json:"next_body"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Previous) != 76 || len(fixture.Next) != 76 ||
		!slices.Equal(fixture.Previous[:75], fixture.Next[:75]) || fixture.Previous[75] == fixture.Next[75] {
		t.Fatal("fixture must contain two 76-row pages differing at the last row")
	}
	return fixture.Previous, fixture.Next
}

func TestSnapshotRepeatedBlockLiveFixtureKeepsTailInPlace(t *testing.T) {
	previous, next := repeatedBlockFixture(t)
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	_, _, admission := m.AdmitSnapshot("p", snapshotPage(previous...), ctx, 0)
	if !admission.Admitted {
		t.Fatal("previous page was not admitted")
	}
	liveStart, historyLength := m.states["p"].LiveStart, len(m.states["p"].History)
	for frame := 0; frame < 150; frame++ {
		body := slices.Clone(next)
		if frame%2 != 0 {
			body = slices.Clone(previous)
		}
		if frame >= 2 {
			body[len(body)-1] = fmt.Sprintf("\x1b[33m* Germinating… (%ds · thinking)\x1b[0m\r", frame+31)
		}
		got, _, admission := m.AdmitSnapshot("p", snapshotPage(body...), ctx, 0)
		if !admission.Admitted || !admission.ContentChanged {
			t.Fatalf("frame %d admission = %+v", frame, admission)
		}
		state := m.states["p"]
		if state.LiveStart != liveStart || len(state.History) != historyLength {
			t.Fatalf("frame %d archive grew: live=%d rows=%d, want live=%d rows=%d", frame, state.LiveStart, len(state.History), liveStart, historyLength)
		}
		if !slices.Equal(state.History[state.LiveStart:], body) || got != snapshotPage(body...) {
			t.Fatalf("frame %d did not retain the exact current page", frame)
		}
		if len(physicalHeldRows(t, state)) != 0 {
			t.Fatalf("frame %d stationary spinner accumulated held rows", frame)
		}
	}
}

func TestSnapshotRepeatedBlockSyntheticKeepsTailInPlace(t *testing.T) {
	previous := []string{"H", "A", "B", "C", "A", "B", "old spinner"}
	next := []string{"H", "A", "B", "C", "A", "B", "new spinner"}
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	m.MergeSnapshot("p", snapshotPage(previous...), ctx)
	got := m.MergeSnapshot("p", snapshotPage(next...), ctx)
	state := m.states["p"]
	if state.LiveStart != 0 || len(state.History) != len(previous) {
		t.Fatalf("tail update archived repeated block: live=%d rows=%d, want live=0 rows=%d", state.LiveStart, len(state.History), len(previous))
	}
	if len(physicalHeldRows(t, state)) != 0 {
		t.Fatal("stationary repeated block accumulated held rows")
	}
	if !slices.Equal(state.History, next) || got != snapshotPage(next...) {
		t.Fatal("tail update did not retain the exact current page")
	}
}

func TestSnapshotRepeatedBlockScrollArchivesDisplacementOnce(t *testing.T) {
	previous := []string{"H1", "A", "B", "H2", "A", "B", "C", "D", "E", "F", "A", "B", "old spinner"}
	next := []string{"H1", "A", "B", "H2", "A", "B", "E", "F", "A", "B", "G", "H", "new spinner"}
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	m.MergeSnapshot("p", snapshotPage(previous...), ctx)
	for frame := range 3 {
		got := m.MergeSnapshot("p", snapshotPage(next...), ctx)
		state := m.states["p"]
		want := append([]string{"C", "D"}, next...)
		if state.LiveStart != 2 || !slices.Equal(state.History, want) {
			t.Fatalf("frame %d displaced rows = %q live=%d, want C,D once and live=2", frame, state.History[:state.LiveStart], state.LiveStart)
		}
		assertSnapshotContent(t, got, want...)
		if len(physicalHeldRows(t, state)) != 0 {
			t.Fatal("supported repeated-block scroll accumulated held rows")
		}
	}
}

// Replaces TestPhysicalAlignmentsRepeatedBlockDoNotConflict's candidate tuple
// assertions with stationary-slot behavior, independent of the old helpers.
func TestPhysicalRepeatedBlockStationaryOccurrenceAgreement(t *testing.T) {
	previous, next := repeatedBlockFixture(t)
	for name, pair := range map[string][2][]string{
		"forward": {previous, next},
		"reverse": {next, previous},
	} {
		t.Run(name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			m.AdmitSnapshot("p", snapshotPage(pair[0]...), ctx, 0)
			got, _, _ := m.AdmitSnapshot("p", snapshotPage(pair[1]...), ctx, 0)
			state := m.states["p"]
			if state.LiveStart != 0 || len(physicalHeldRows(t, state)) != 0 || got != snapshotPage(pair[1]...) {
				t.Fatal("interior repeats caused false retirement or held accumulation")
			}
		})
	}
}
