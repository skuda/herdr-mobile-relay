package history

import (
	"slices"
	"testing"
)

func TestSnapshotPaddedPagesDoNotRepeatContent(t *testing.T) {
	for _, pair := range [][2][]string{
		{{"A", "B", "partial"}, {"A", "B", "partial completion"}},
		{{"A", "B", "C"}, {"A", "B", "C", "D"}},
		{{"A", "B", "progress 1"}, {"A", "B", "progress 2"}},
	} {
		m := NewManager(t.TempDir())
		old, next := make([]string, 76), make([]string, 76)
		copy(old, pair[0])
		copy(next, pair[1])
		m.MergeSnapshot("p", snapshotPage(old...), physicalContext(1, 40))
		if got := m.MergeSnapshot("p", snapshotPage(next...), physicalContext(1, 40)); got != snapshotPage(next...) {
			t.Fatalf("padded update copied prior page: %q", got)
		}
		if state := m.states["p"]; state.LiveStart != 0 || !slices.Equal(state.History, next) {
			t.Fatalf("padded update changed raw retained rows: live=%d rows=%d", state.LiveStart, len(state.History))
		}
	}
}

func TestSnapshotBottomPaddingEquivalentCandidates(t *testing.T) {
	old, next := make([]string, 76), make([]string, 76)
	copy(old[73:], []string{"x1", "x2", "x3"})
	copy(next[72:], []string{"x1", "x2", "x3", "x4"})
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage(old...), ctx)
	want := "\n" + snapshotPage(next...)
	if got := m.MergeSnapshot("p", snapshotPage(next...), ctx); got != want {
		t.Fatalf("equivalent blank displacement copied page: got=%q want=%q", got, want)
	}
}

func TestSnapshotOrderedDisplacementConflictFallsBack(t *testing.T) {
	old := []string{"A", "B", "A", "C", "D"}
	next := []string{"A", "C", "D", "E", "F"}
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage(old...), ctx)
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(next...), ctx), append(old, next...)...)
}
