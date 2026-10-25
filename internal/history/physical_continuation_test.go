package history

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

func TestPhysicalEqualColumnsLifecycleContinues(t *testing.T) {
	for _, lifecycle := range []string{"height", "lease", "restart", "restart-and-reacquire"} {
		for _, style := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/style=%v", lifecycle, style), func(t *testing.T) {
				root := t.TempDir()
				m := NewManager(root)
				ctx := physicalContext(1, 54)
				page := []string{"A", "B", "C", "D"}
				admitPhysicalOccurrence(t, m, page, ctx, "A", 1)
				if err := m.SaveAll(); err != nil {
					t.Fatal(err)
				}
				switch lifecycle {
				case "height":
					ctx.GeometryEpoch, ctx.Rows = 2, 100
				case "lease":
					ctx.LeaseRevision = 3
				case "restart":
					m = NewManager(root)
				case "restart-and-reacquire":
					m = NewManager(root)
					ctx.LeaseRevision, ctx.GeometryEpoch, ctx.Rows = 3, 8, 100
				}
				if style {
					page[0] = "\x1b[31mA\x1b[0m"
				}
				d := admitPhysicalOccurrence(t, m, page, ctx, "A", 1)
				state := m.states["p"]
				if d.Class == "boundary" || state.SegmentStart != 0 || state.LiveStart != 0 || state.loaded {
					t.Errorf("equal-column lifecycle forced boundary: class=%s segment=%d live=%d loaded=%v", d.Class, state.SegmentStart, state.LiveStart, state.loaded)
				}
				if state.snapshotContext() != ctx || !d.MetadataChanged && lifecycle != "restart" || !state.dirty && lifecycle != "restart" {
					t.Errorf("continued metadata not stored: got=%+v want=%+v admission=%+v", state.snapshotContext(), ctx, d)
				}
			})
		}
	}
}

func TestPhysicalEqualColumnsHeightForwardAndReverse(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	ctx.Rows = 12
	admitPhysicalOccurrence(t, m, []string{"P", "Q", "A", "B", "C", "D"}, ctx, "P", 1)
	ctx.GeometryEpoch, ctx.Rows = 2, 10
	d := admitPhysicalOccurrence(t, m, []string{"A", "B", "C", "D"}, ctx, "P", 1)
	state := m.states["p"]
	if d.Class != "forward" || state.LiveStart != 2 || state.SegmentStart != 0 || !slices.Equal(state.History[:state.LiveStart], []string{"P", "Q"}) {
		t.Errorf("shorter same-width viewport=%+v archive=%q segment=%d", d, state.History[:state.LiveStart], state.SegmentStart)
	}
	ctx.GeometryEpoch, ctx.Rows = 3, 12
	d = admitPhysicalOccurrence(t, m, []string{"P", "Q", "A", "B", "C", "D"}, ctx, "P", 1)
	if d.Class != "reverse" || d.RetractedRows != 2 || state.LiveStart != 0 || state.SegmentStart != 0 {
		t.Errorf("taller same-width viewport=%+v live=%d segment=%d", d, state.LiveStart, state.SegmentStart)
	}
}

func TestPhysicalEqualColumnsHeightKeepsReplacementAndFreezeRules(t *testing.T) {
	for _, test := range []struct {
		name      string
		old, next []string
		class     string
	}{
		{"bounded-replacement", []string{"H", "T", "old1", "old2", "old3"}, []string{"H", "T", "new1", "new2", "new3"}, "replaced"},
		{"majority-rewrap", []string{"H", "T", "U", "old1", "old2", "old3", "old4", "old5"}, []string{"H", "T", "U", "new1", "new2", "new3", "new4", "new5"}, "freeze"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			admitPhysicalOccurrence(t, m, test.old, ctx, "H", 1)
			ctx.GeometryEpoch, ctx.Rows = 2, 100
			wantH := 1
			if test.class == "freeze" {
				wantH = 2
			}
			d := admitPhysicalOccurrence(t, m, test.next, ctx, "H", wantH)
			if d.Class != test.class || m.states["p"].SegmentStart != 0 {
				t.Errorf("same-width transition=%+v segment=%d", d, m.states["p"].SegmentStart)
			}
			if test.class == "replaced" && (d.ReplacedRows != 3 || m.states["p"].LiveStart != 0) {
				t.Error("height-only change bypassed U7 replacement")
			}
			if test.class == "freeze" && (d.Reason != "held_majority" || d.HeldNonblank != 5 || d.OldNonblank != 8) {
				t.Error("height-only rewrap bypassed held-majority freeze")
			}
		})
	}
}

func TestPhysicalDistinctRepresentationBoundariesGuard(t *testing.T) {
	for _, kind := range []string{"width", "old-zero", "new-zero", "unknown-geometry", "both-zero", "source", "invalid", "missing-stored-width", "terminal"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			m := NewManager(root)
			ctx := physicalContext(1, 54)
			if kind == "missing-stored-width" {
				data := `{"history":["A","B","old"],"snapshot_version":1,"terminal_id":"terminal-1","read_source":"recent","geometry_epoch":1,"lease_revision":1,"live_start":0}`
				if err := os.WriteFile(m.stateFile("p"), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				admitPhysicalOccurrence(t, m, []string{"A", "B", "old"}, ctx, "A", 1)
			}
			state := m.loadState("p")
			switch kind {
			case "width":
				ctx.Columns = 184
			case "old-zero":
				state.Columns = 0
			case "new-zero":
				ctx.Columns = 0
			case "unknown-geometry":
				ctx.GeometryEpoch, ctx.Columns, ctx.Rows = 0, 0, 0
			case "both-zero":
				state.Columns, ctx.Columns = 0, 0
			case "source":
				ctx.ReadSource = SourceLegacy
				m.AdmitSnapshot("p", snapshotPage("A", "B", "legacy"), ctx, 0)
				ctx.ReadSource = SourceRecent
			case "invalid":
				state.snapshotInvalid = true
			case "terminal":
				ctx.TerminalID = "replacement"
			}
			got, _, d := m.AdmitSnapshot("p", snapshotPage("A", "B", "new"), ctx, 0)
			if kind == "terminal" {
				if got != snapshotPage("A", "B", "new") {
					t.Error("terminal replacement retained old identity")
				}
				return
			}
			state = m.states["p"]
			want := []string{"A", "B", "old", "A", "B", "new"}
			if kind == "source" {
				want = []string{"A", "B", "old", "A", "B", "legacy", "A", "B", "new"}
			}
			if d.Class != "boundary" || got != snapshotPage(want...) || state.SegmentStart != len(want)-3 {
				t.Errorf("representation boundary %s=%+v segment=%d content=%q", kind, d, state.SegmentStart, got)
			}
		})
	}
}
