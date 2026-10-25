package history

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func assertBoundaryCopyRetained(t *testing.T, old, incoming []string) {
	t.Helper()
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	ctx.Columns, ctx.GeometryEpoch = 62, 2
	got, _, d := m.AdmitSnapshot("p", snapshotPage(incoming...), ctx, 0)
	state := m.states["p"]
	if d.BoundaryRefused != 1 || d.BoundaryReconciledRows != 0 || d.BoundarySeamKept != 0 {
		t.Errorf("boundary must refuse once without deleting rows: %+v", d)
	}
	if state.LiveStart != len(old) || state.SegmentStart != len(old) || !slices.Equal(state.History[:state.LiveStart], old) {
		t.Errorf("refusal changed archive: live=%d segment=%d archive=%q", state.LiveStart, state.SegmentStart, state.History[:state.LiveStart])
	}
	want := snapshotPage(append(slices.Clone(old), incoming...)...)
	if got != want {
		t.Error("refusal did not retain the archive plus exact incoming raw page")
	}
}

func TestBoundaryRequestedRepeatRetainsNewOccurrence(t *testing.T) {
	incoming := []string{"❯ Please give the same harbor answer again, word for word.", ""}
	incoming = append(incoming, boundaryProse...)
	if result := matchBoundarySuffix(boundaryProse, 0, incoming); result.reason != boundaryIncomingPrompt {
		t.Errorf("new prompt refusal reason=%s", result.reason)
	}
	assertBoundaryCopyRetained(t, boundaryProse, incoming)
}

func TestBoundaryDistinctCompetingOverlapsRefuse(t *testing.T) {
	incoming := append(slices.Clone(boundaryProse[1:]), "different intervening output", "")
	incoming = append(incoming, boundaryProse...)
	for _, overlap := range [][]string{boundaryProse[1:], boundaryProse} {
		if len(strings.Fields(strings.Join(overlap, " "))) < MinBoundaryWords || len(overlap) < MinBoundaryRows {
			t.Fatal("competing overlap does not meet the minimum run")
		}
	}
	if result := matchBoundarySuffix(boundaryProse, 0, incoming); result.reason != boundaryAmbiguousCandidate {
		t.Errorf("qualifying competing overlaps reason=%s", result.reason)
	}
	assertBoundaryCopyRetained(t, boundaryProse, incoming)
}

func TestBoundaryPartialCompetingRunsRefuse(t *testing.T) {
	for _, test := range []struct {
		name string
		old  []string
	}{
		{"clipped-by-view", append(append(slices.Clone(boundaryProse), strings.Repeat("x", 123), ""), boundaryProse...)},
		{"partial-at-floor", append(slices.Clone(boundaryProse[1:]), boundaryProse...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if result := matchBoundarySuffix(test.old, 0, boundaryProse); result.reason != boundaryPartialRun || result.competing.lastRow <= result.competing.firstRow {
				t.Errorf("two-row partial repeat reason=%s run=%+v", result.reason, result.competing)
			}
			assertBoundaryCopyRetained(t, test.old, boundaryProse)
		})
	}
}

func TestBoundarySingleRepeatedPromptRowDoesNotRefuse(t *testing.T) {
	prompt := "❯ Explain how patient harbor keepers maintain bright lamps through long winter storms."
	if len(strings.Fields(prompt)) < MinBoundaryWords {
		t.Fatal("repeated row has fewer than twelve words")
	}
	incoming := append([]string{prompt, ""}, boundaryProse...)
	control := append([]string{"unrelated prior transcript", ""}, incoming...)
	repeated := append([]string{prompt, ""}, incoming...)
	baseline := matchBoundarySuffix(control, 0, incoming)
	result := matchBoundarySuffix(repeated, 0, incoming)
	if baseline.refused || result.refused || baseline.cut != 2 || result.cut != baseline.cut {
		t.Errorf("single repeated prompt row changed the cut: control=%+v repeated=%+v", baseline, result)
	}
}

func TestBoundaryRepeatedPromptBlockDoesNotRefuse(t *testing.T) {
	prompt := []string{"❯ Explain how patient harbor keepers maintain bright lamps", "through long winter storms and guide the ships safely.", ""}
	incoming := append(slices.Clone(prompt), boundaryProse...)
	control := append([]string{"an unrelated earlier request", "with a different continuation", ""}, incoming...)
	repeated := append(slices.Clone(prompt), incoming...)
	baseline := matchBoundarySuffix(control, 0, incoming)
	result := matchBoundarySuffix(repeated, 0, incoming)
	if baseline.refused || result.refused || baseline.cut != len(prompt) || result.cut != baseline.cut {
		t.Errorf("repeated prompt block changed the cut: control=%+v repeated=%+v", baseline, result)
	}
}

func TestBoundaryCompetingPromptClassification(t *testing.T) {
	for _, test := range []struct {
		name     string
		rows     []string
		firstRow int
		want     bool
	}{
		{"prompt-block", []string{"❯ alpha bravo charlie delta echo", "foxtrot golf hotel india juliet kilo lima"}, 0, false},
		{"clipped-view-is-prose", []string{"alpha bravo charlie delta echo foxtrot", "golf hotel india juliet kilo lima"}, 7, true},
		{"prompt-then-prose", []string{"❯ alpha bravo charlie delta echo", "foxtrot golf hotel india juliet kilo lima", "", boundaryProse[0], boundaryProse[1]}, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := boundaryView{rows: test.rows, firstRow: test.firstRow}
			work := boundaryWork{}
			for i, row := range test.rows {
				tokens, ok := work.addTokens(row, test.firstRow+i)
				if !ok {
					t.Fatal("synthetic prompt fixture exhausted tokens")
				}
				view.tokens = append(view.tokens, tokens...)
			}
			run, ok := boundaryCompetingRun(view.tokens, view.tokens, view, &work)
			if !ok || (run.words > 0) != test.want {
				t.Errorf("prompt classification run=%+v ok=%v want_competing=%v", run, ok, test.want)
			}
		})
	}
}

func TestBoundaryTwoSubstantiveRowCompetingRunRefuses(t *testing.T) {
	for _, rows := range [][]string{
		{"alpha bravo charlie delta echo foxtrot", "golf hotel india juliet kilo lima"},
		{"alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima", "mike november"},
	} {
		view := boundaryView{rows: rows}
		work := boundaryWork{}
		for i, row := range rows {
			tokens, ok := work.addTokens(row, i)
			if !ok {
				t.Fatal("synthetic two-row fixture exhausted tokens")
			}
			view.tokens = append(view.tokens, tokens...)
		}
		run, ok := boundaryCompetingRun(view.tokens, view.tokens, view, &work)
		if !ok || run.words < MinBoundaryWords || run.firstRow != 0 || run.lastRow != 1 {
			t.Errorf("two-row competing run was not qualified: run=%+v ok=%v", run, ok)
		}
	}
	old := append(slices.Clone(boundaryProse[1:]), "distinct separator", "")
	old = append(old, boundaryProse...)
	if result := matchBoundarySuffix(old, 0, boundaryProse); result.reason != boundaryPartialRun {
		t.Errorf("two-row competing repeat did not refuse: %+v", result)
	}
	assertBoundaryCopyRetained(t, old, boundaryProse)
}

func TestBoundaryFloatingOverlapMismatchRefuses(t *testing.T) {
	old := append([]string{"❯ Give the original harbor answer.", ""}, boundaryProse...)
	incoming := append([]string{"❯ Please repeat that harbor answer exactly.", ""}, boundaryProse...)
	if result := matchBoundarySuffix(old, 0, incoming); result.reason != boundaryIncomingPrompt {
		t.Errorf("different prompt refusal reason=%s", result.reason)
	}
	assertBoundaryCopyRetained(t, old, incoming)
}

func TestBoundarySplitLongWordAndPathAt62ColumnsRefuse(t *testing.T) {
	for _, test := range []struct {
		name, token string
	}{
		{"long-word", strings.Repeat("abcdefghij", 9)},
		{"path", "synthetic/" + strings.Repeat("directory", 9) + "/report.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			old := append(slices.Clone(boundaryProse[:2]), test.token)
			incoming := slices.Clone(boundaryProse[:2])
			for token := test.token; token != ""; {
				width := min(62, len(token))
				incoming = append(incoming, token[:width])
				token = token[width:]
			}
			if len(incoming[2]) != 62 || incoming[2]+incoming[3] != test.token {
				t.Fatal("fixture does not split the token at column 62")
			}
			assertBoundaryCopyRetained(t, old, incoming)
		})
	}
}

func TestBoundaryEqualHeldPinsPreserveAdmissionOrder(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	old := append([]string{"prior unchanged transcript"}, boundaryProse...)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	state := m.states["p"]
	state.LiveStart = len(old)
	for i, raw := range []string{"\x1b[31mfirst held\x1b[0m\r", "second held", "  third held  "} {
		state.Held = append(state.Held, HeldRecord{
			Rows: []string{raw, ""}, Position: 3, Segment: 2,
			FirstSeenAt: time.Unix(1000+int64(i), int64(i+1)).UTC(),
		})
	}
	before := cloneHeldRecords(state.Held)
	ctx.Columns = 62
	_, _, d := m.AdmitSnapshot("p", snapshotPage(boundaryProse...), ctx, 0)
	if d.BoundaryReconciledRows != 3 || state.LiveStart != 1 || state.SegmentStart != 1 || len(state.Held) != len(before) {
		t.Fatalf("equal-pin suffix deletion coordinates=%+v held=%+v", d, state.Held)
	}
	for i, record := range state.Held {
		if record.Position != 1 || record.Segment != 1 || !slices.Equal(record.Rows, before[i].Rows) || !record.FirstSeenAt.Equal(before[i].FirstSeenAt) {
			t.Errorf("equal pin %d changed admission order, raw rows, timestamp, or coordinates: %+v", i, record)
		}
	}
}

func TestBoundaryOldSegmentStartClampsToCut(t *testing.T) {
	state := &PaneState{History: append([]string{"prior"}, boundaryProse...), LiveStart: 4, SegmentStart: 3}
	d := 1
	if state.SegmentStart <= d {
		t.Fatal("fixture does not place the old segment after the cut")
	}
	// The actual match cannot cross its segment floor. Check the coordinate clamp
	// separately at the same retention boundary used after reconciliation.
	state.History = state.History[:d]
	state.LiveStart = d
	trimSnapshotState(state)
	if state.SegmentStart != d || state.LiveStart != d {
		t.Errorf("old segment was not clamped: segment=%d live=%d", state.SegmentStart, state.LiveStart)
	}
}

func TestBoundaryDeletionInvalidatesRecordWithEqualSurvivingSuffix(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage("H", "X", "T", "U"), ctx, 0)
	m.AdmitSnapshot("p", snapshotPage("H", "", "T", "U"), ctx, 0)
	state := m.states["p"]
	if state.zeroDisplacementReturn == nil {
		t.Fatal("missing pending return record")
	}
	compared := slices.Clone(state.zeroDisplacementReturn.rows)
	state.History = append([]string{"frozen prefix", "X"}, boundaryProse...)
	state.History = append(state.History, "X")
	state.LiveStart = len(state.History)
	state.SegmentStart = 0
	if !slices.Equal(physicalComparisonRows(state.History[state.LiveStart-len(compared):state.LiveStart]), compared) {
		t.Fatal("pre-cut suffix differs from the record")
	}
	generation := state.archiveGeneration
	incoming := append(slices.Clone(boundaryProse), "X")
	result := reconcileBoundary(state, incoming, true)
	if result.refused || result.cut != 2 || result.rows != 4 {
		t.Fatalf("expected whole-row suffix deletion: %+v", result)
	}
	if !slices.Equal(physicalComparisonRows(state.History[state.LiveStart-len(compared):state.LiveStart]), compared) {
		t.Fatal("surviving suffix does not preserve compared bytes")
	}
	if state.archiveGeneration <= generation || state.zeroDisplacementReturn != nil {
		t.Error("equal surviving suffix hid archive mutation or retained the return record")
	}
}

func TestBoundaryPairedFirstBoundaryReason(t *testing.T) {
	pages := boundaryReplayPages(t)
	m := NewManager(t.TempDir())
	for i := range 2 {
		ctx := physicalContext(uint64(i+1), pages[i].Columns)
		ctx.ReadSource, ctx.Columns = SourceLegacy, 0
		m.AdmitSnapshot("p", strings.Join(pages[i].Rows, "\n"), ctx, 0)
	}
	state := m.states["p"]
	incoming, _ := splitSnapshot(strings.Join(pages[2].Rows, "\n"))
	result := matchBoundarySuffix(state.History, state.SegmentStart, incoming)
	t.Logf("refused=%v reason=%s reconciled=%d", result.refused, result.reason, result.rows)
	if result.refused || result.rows != 39 || result.reason != "" {
		t.Errorf("paired source boundary changed: refused=%v rows=%d reason=%s", result.refused, result.rows, result.reason)
	}
}

func TestBoundaryANSIHeavyRawBudgetEdge(t *testing.T) {
	for _, extra := range []int{0, 1} {
		name := "exact-edge"
		if extra != 0 {
			name = "over-edge"
		}
		t.Run(name, func(t *testing.T) {
			length := MaxBoundaryRawBytes + extra
			row := strings.Repeat("\x1b[31m", length/5) + strings.Repeat(" ", length%5)
			if len(row) != length {
				t.Fatal("ANSI-heavy fixture has the wrong raw length")
			}
			work := boundaryWork{}
			text, ok := work.row(row)
			if extra == 0 {
				if !ok || text != "" || work.rawBytes != MaxBoundaryRawBytes || work.normalizedRows != 1 || work.normalizedBytes != 0 {
					t.Errorf("ANSI edge was not charged before stripping: ok=%v work=%+v", ok, work)
				}
				if _, ok := work.row("x"); ok || work.rawBytes != MaxBoundaryRawBytes || work.normalizedRows != 1 {
					t.Error("exhausted raw budget admitted another row")
				}
			} else if ok || work.rawBytes != 0 || work.normalizedRows != 0 || work.normalizedBytes != 0 {
				t.Errorf("oversized ANSI row entered normalization: ok=%v work=%+v", ok, work)
			}
			incoming := append([]string{row}, boundaryProse...)
			assertBoundaryCopyRetained(t, boundaryProse, incoming)
		})
	}
}
