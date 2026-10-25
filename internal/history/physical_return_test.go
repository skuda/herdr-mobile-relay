package history

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func admitPhysicalOccurrence(t *testing.T, m *Manager, body []string, ctx SnapshotContext, marker string, want int) SnapshotAdmission {
	t.Helper()
	raw := snapshotPage(body...)
	content, _, d := m.AdmitSnapshot("p", raw, ctx, 0)
	state := m.states["p"]
	count := 0
	for _, row := range state.History {
		if physicalText(row) == marker {
			count++
		}
	}
	if count != want {
		t.Errorf("body=%q: %s occurrences=%d, want %d; archive rows=%d", body, marker, count, want, state.LiveStart)
	}
	if !d.Admitted || !slices.Equal(state.History[state.LiveStart:], body) {
		t.Errorf("admission=%+v did not install raw body", d)
	}
	if wantContent := strings.Join(append(slices.Clone(state.History[:state.LiveStart]), raw), "\n"); content != wantContent {
		t.Error("response differs from archive plus exact raw page and footer")
	}
	physicalHeld(t, state)
	return d
}

func physicalReturnPending(state *PaneState) bool {
	return state.zeroDisplacementReturn != nil
}

func physicalArchiveGeneration(state *PaneState) uint64 {
	return state.archiveGeneration
}

func TestPhysicalOneRowReverseRSequence(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	pages := [][]string{
		{"R", "A", "B", "C", "S1", "S2"},
		{"A", "B", "C", "S1'", "S2'", "N"},
		{"R", "A", "B", "C", "N"},
		{"A", "B", "C", "N", "M"},
	}
	for i, page := range pages {
		d := admitPhysicalOccurrence(t, m, page, ctx, "R", 1)
		if i != 2 {
			continue
		}
		if d.Class != "reverse" || d.RetractedRows != 1 || d.RetractionCount != 1 {
			t.Errorf("R return=%+v, want reverse with one retracted row", d)
		}
		state := m.states["p"]
		if !slices.Equal(physicalHeldRows(t, state), []string{"S1'", "S2'"}) || len(state.Held) != 1 || state.Held[0].Position != 0 {
			t.Errorf("spinner residue/pin=%+v, want held off-display at zero", state.Held)
		}
	}
}

func TestPhysicalOneRowReversePQSequence(t *testing.T) {
	for _, frozen := range []int{0, 383} {
		t.Run(fmt.Sprintf("frozen%d", frozen), func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			pages := [][]string{
				{"P", "Q", "A", "B", "C", "D", "E", "F"},
				{"Q", "A", "B", "C", "D", "E", "F", "X"},
				{"P", "Q", "A", "B", "C", "D", "E", "Y"},
				{"Q", "A", "B", "C", "D", "E", "Y", "Z"},
			}
			admitPhysicalOccurrence(t, m, pages[0], ctx, "P", 1)
			state := m.states["p"]
			prefix := frozen
			if frozen > 0 {
				prefix += 9
				archive := make([]string, prefix)
				for i := range archive {
					archive[i] = fmt.Sprintf("prior-%03d", i)
				}
				state.History = append(archive, state.History...)
				state.SegmentStart, state.LiveStart = frozen, prefix
			}
			for i := 1; i < len(pages); i++ {
				d := admitPhysicalOccurrence(t, m, pages[i], ctx, "P", 1)
				if i == 2 {
					if d.RetractedRows != 1 || d.RetractionCount != 1 || d.Class != "reverse" || state.LiveStart != prefix {
						t.Errorf("P return=%+v live=%d, want reverse to %d", d, state.LiveStart, prefix)
					}
					if len(state.Held) != 1 || state.Held[0].Position != prefix || !slices.Equal(physicalHeldRows(t, state), []string{"F", "X"}) {
						t.Errorf("held pin=%+v, want F,X clamped from %d to %d", state.Held, prefix+1, prefix)
					}
				}
			}
		})
	}
}

func TestPhysicalZeroDisplacementReturn(t *testing.T) {
	for _, test := range []struct {
		name        string
		old, blank  []string
		forwardRows int
	}{
		{"forward-a-equals-b", []string{"H", "X", "T", "U"}, []string{"H", "", "T", "U"}, 1},
		{"cleared-slot", []string{"H", "timer0", "V", "X", "T", "U"}, []string{"H", "timer1", "V", "", "T", "U"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			plan := planPhysicalTransition(test.old, test.blank, true)
			if test.forwardRows > 0 {
				run := plan.anchorRun(test.old, test.blank, physicalPair{2, 2})
				if run.oldStart != run.newStart || run.pairs < 2 {
					t.Fatal("fixture does not exercise a == b forward support")
				}
			} else {
				if plan.header != 1 || plan.outcome[1] != physicalReplaced || plan.outcome[3] != physicalRetired {
					t.Fatal("fixture does not isolate cleared-slot retirement after a nonblank changed gap")
				}
			}
			admitPhysicalOccurrence(t, m, test.old, ctx, "X", 1)
			admitPhysicalOccurrence(t, m, test.blank, ctx, "X", 1)
			state := m.states["p"]
			if !physicalReturnPending(state) {
				t.Error("zero-displacement retirement did not establish a runtime record")
			}
			generation := physicalArchiveGeneration(state)
			returned := slices.Clone(test.blank)
			for i, row := range test.old {
				if row == "X" {
					returned[i] = row
				}
			}
			d := admitPhysicalOccurrence(t, m, returned, ctx, "X", 1)
			if d.Class != "reverse" || d.RetractedRows != 1 || d.RetractionCount != 1 || state.LiveStart != 0 || physicalReturnPending(state) {
				t.Errorf("slot return=%+v live=%d pending=%v", d, state.LiveStart, physicalReturnPending(state))
			}
			if physicalArchiveGeneration(state) <= generation {
				t.Error("slot retraction did not advance archive generation")
			}
			admitPhysicalOccurrence(t, m, test.blank, ctx, "X", 1)
			if !slices.Equal(state.History[:state.LiveStart], []string{"X"}) {
				t.Errorf("second clear archive=%q, want X once", state.History[:state.LiveStart])
			}
		})
	}
}

func TestPhysicalReturnAcrossNonSubstantiveLayout(t *testing.T) {
	for _, layout := range []string{"", "---", "both-sides"} {
		t.Run(fmt.Sprintf("layout=%q", layout), func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			old := []string{"H", layout, "X", "T", "U"}
			blank := []string{"H", layout, "", "T", "U"}
			if layout == "both-sides" {
				old = []string{"H", "---", "", "X", "===", "", "T", "U"}
				blank = []string{"H", "---", "", "", "===", "", "T", "U"}
			}
			for i, page := range [][]string{old, blank, old, blank} {
				d := admitPhysicalOccurrence(t, m, page, ctx, "X", 1)
				if i == 1 && !physicalReturnPending(m.states["p"]) {
					t.Error("unchanged layout blocked the zero-displacement record")
				}
				if i == 2 && (d.RetractedRows != 1 || physicalReturnPending(m.states["p"])) {
					t.Errorf("return across unchanged layout=%+v pending=%v", d, physicalReturnPending(m.states["p"]))
				}
			}
		})
	}
}

func TestPhysicalReturnLayoutCloneOwnsBothSides(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	admitPhysicalOccurrence(t, m, []string{"H", "---", "", "X", "===", "", "T", "U"}, ctx, "X", 1)
	admitPhysicalOccurrence(t, m, []string{"H", "---", "", "", "===", "", "T", "U"}, ctx, "X", 1)
	record := m.states["p"].zeroDisplacementReturn
	if record == nil || record.neighbourCoordinates != [2]int{0, 6} || record.slot != 3 || len(record.rows) != 1 ||
		!slices.Equal(record.layout[0], []string{"---", ""}) || !slices.Equal(record.layout[1], []string{"===", ""}) {
		t.Fatalf("layout record=%+v", record)
	}
	clone := clonePhysicalReturn(record)
	clone.layout[0][0], clone.layout[1][0] = "changed left", "changed right"
	clone.neighbourCoordinates[0]++
	if record.layout[0][0] != "---" || record.layout[1][0] != "===" || record.neighbourCoordinates[0] != 0 {
		t.Error("layout clone aliases comparison rows or anchor coordinates")
	}
}

func TestPhysicalReturnChangedInterveningLayoutRefuses(t *testing.T) {
	for _, change := range []struct {
		name      string
		old, next []string
	}{
		{"left", []string{"H", "", "X", "T", "U"}, []string{"H", "---", "", "T", "U"}},
		{"right", []string{"H", "X", "---", "T", "U"}, []string{"H", "", "===", "T", "U"}},
	} {
		t.Run(change.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			admitPhysicalOccurrence(t, m, change.old, ctx, "X", 1)
			blank := slices.Clone(change.old)
			for i, row := range blank {
				if row == "X" {
					blank[i] = ""
				}
			}
			admitPhysicalOccurrence(t, m, blank, ctx, "X", 1)
			if !physicalReturnPending(m.states["p"]) {
				t.Error("missing layout-aware record before layout change")
			}
			admitPhysicalOccurrence(t, m, change.next, ctx, "X", 1)
			if physicalReturnPending(m.states["p"]) {
				t.Error("changed intervening layout left the record valid")
			}
			returned := slices.Clone(change.next)
			for i, row := range change.old {
				if row == "X" {
					returned[i] = "X"
				}
			}
			d := admitPhysicalOccurrence(t, m, returned, ctx, "X", 2)
			if d.RetractedRows != 0 {
				t.Error("changed layout authorized an unsupported return")
			}
		})
	}
}

func TestPhysicalReturnRecordCloneOwnsNestedRows(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	admitPhysicalOccurrence(t, m, []string{"H", "X", "T", "U"}, ctx, "X", 1)
	admitPhysicalOccurrence(t, m, []string{"H", "", "T", "U"}, ctx, "X", 1)
	state := m.states["p"]
	clone := clonePhysicalReturn(state.zeroDisplacementReturn)
	if clone == nil || clone == state.zeroDisplacementReturn {
		t.Fatal("return clone does not own the record")
	}
	clone.rows[0] = "changed"
	clone.neighbours[0] = "changed"
	clone.slot++
	clone.generation++
	if state.zeroDisplacementReturn.rows[0] != "X" || state.zeroDisplacementReturn.neighbours[0] != "H" ||
		state.zeroDisplacementReturn.slot != 1 || state.zeroDisplacementReturn.generation != state.archiveGeneration {
		t.Fatal("return clone aliases live record rows or metadata")
	}
	if clonePhysicalReturn(nil) != nil {
		t.Error("nil record clone is not nil")
	}
}

func TestPhysicalReturnRetractsRecordedTailOnly(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	old := []string{"H", "X", "", "second row", "T", "U"}
	blank := []string{"H", "", "", "", "T", "U"}
	admitPhysicalOccurrence(t, m, old, ctx, "X", 1)
	state := m.states["p"]
	state.History = append([]string{"frozen", "earlier"}, state.History...)
	state.SegmentStart, state.LiveStart = 1, 2
	admitPhysicalOccurrence(t, m, blank, ctx, "X", 1)
	if record := state.zeroDisplacementReturn; record == nil || len(record.rows) != 3 {
		t.Fatalf("missing three-row return record: %+v", record)
	}
	state.Held = []HeldRecord{{Rows: []string{"held"}, Position: 5, Segment: 4, FirstSeenAt: time.Now().UTC()}}
	stamp := state.Held[0].FirstSeenAt
	d := admitPhysicalOccurrence(t, m, old, ctx, "X", 1)
	if d.RetractedRows != 3 || state.LiveStart != 2 || !slices.Equal(state.History[:state.LiveStart], []string{"frozen", "earlier"}) {
		t.Errorf("slot return removed more than its recorded suffix: %+v archive=%q", d, state.History[:state.LiveStart])
	}
	if state.Held[0].Position != 2 || state.Held[0].Segment != 2 || !state.Held[0].FirstSeenAt.Equal(stamp) || state.Held[0].Rows[0] != "held" {
		t.Errorf("slot return changed held rows/timestamp or failed two-stage clamp: %+v", state.Held)
	}
}

func TestPhysicalReturnArchiveMutationTwoXGuard(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	pages := [][]string{
		{"H", "X", "T", "U", "Y", "X", "A", "B", "C"},
		{"H", "", "T", "U", "Y", "X", "A", "B", "C"},
		{"H", "", "T", "U", "A", "B", "C", "N", "M"},
		{"H", "X", "T", "U", "A", "B", "C", "N", "M"},
	}
	for i, page := range pages {
		want := 2
		if i == 3 {
			want = 3
		}
		d := admitPhysicalOccurrence(t, m, page, ctx, "X", want)
		if i == 1 && !physicalReturnPending(m.states["p"]) {
			t.Error("first X retirement did not establish the record being invalidated")
		}
		if i >= 2 && physicalReturnPending(m.states["p"]) {
			t.Error("intervening archive append left the earlier return record valid")
		}
		if i == 3 && (d.RetractedRows != 0 || !slices.Equal(m.states["p"].History[:m.states["p"].LiveStart], []string{"X", "Y", "X"})) {
			t.Errorf("unsupported two-X return=%+v archive=%q", d, m.states["p"].History[:m.states["p"].LiveStart])
		}
	}
}

func TestPhysicalReturnBlankFramesAndPreviewIsolation(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	admitPhysicalOccurrence(t, m, []string{"H", "X", "T", "U"}, ctx, "X", 1)
	blank := []string{"H", "", "T", "U"}
	admitPhysicalOccurrence(t, m, blank, ctx, "X", 1)
	state := m.states["p"]
	generation := physicalArchiveGeneration(state)
	for _, body := range [][]string{blank, {"\x1b[31mH\x1b[0m", " ", "T", "U"}} {
		admitPhysicalOccurrence(t, m, body, ctx, "X", 1)
		if !physicalReturnPending(state) || physicalArchiveGeneration(state) != generation {
			t.Error("identical/ANSI-only blank frame changed record or archive generation")
		}
	}
	before := fmt.Sprintf("%#v", state)
	for _, source := range []string{SourceRecent, SourceLegacy} {
		previewContext := ctx
		previewContext.ReadSource = source
		if _, ok := m.PreviewSnapshot("p", snapshotPage("H", "X", "T", "U"), previewContext); !ok {
			t.Error("matching-terminal preview refused")
		}
		if fmt.Sprintf("%#v", state) != before || physicalArchiveGeneration(state) != generation || !physicalReturnPending(state) {
			t.Error("preview changed live record, generation or state")
		}
	}
	d := admitPhysicalOccurrence(t, m, []string{"H", "X", "T", "U"}, ctx, "X", 1)
	if d.RetractedRows != 1 {
		t.Errorf("return after blank/style/preview frames retracted=%d, want 1", d.RetractedRows)
	}
}

func TestPhysicalReturnInterruptedSlotAndGenuineRepeatGuard(t *testing.T) {
	for _, interruption := range [][]string{
		{"H", "new occurrence", "T", "U"},
		{"different H", "", "T", "U"},
		{"H", "H", "", "T", "U"},
		{"H", "", "T", "T", "U"},
	} {
		t.Run(fmt.Sprint(interruption), func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			admitPhysicalOccurrence(t, m, []string{"H", "X", "T", "U"}, ctx, "X", 1)
			admitPhysicalOccurrence(t, m, []string{"H", "", "T", "U"}, ctx, "X", 1)
			admitPhysicalOccurrence(t, m, interruption, ctx, "X", 1)
			if physicalReturnPending(m.states["p"]) {
				t.Error("nonblank slot or lost/disputed neighbour did not invalidate the record")
			}
			returned := slices.Clone(interruption)
			returned[1] = "X"
			d := admitPhysicalOccurrence(t, m, returned, ctx, "X", 2)
			if d.RetractedRows != 0 {
				t.Error("genuinely new X occurrence was deduplicated")
			}
			returned[1] = ""
			admitPhysicalOccurrence(t, m, returned, ctx, "X", 2)
		})
	}
}

func TestPhysicalReturnInvalidationLifecycle(t *testing.T) {
	for _, change := range []string{"reload", "terminal-reset", "discard", "boundary", "archive-retraction", "prefix-trim"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			m := NewManager(root)
			ctx := physicalContext(1, 54)
			admitPhysicalOccurrence(t, m, []string{"H", "X", "T", "U"}, ctx, "X", 1)
			state := m.states["p"]
			if change == "prefix-trim" {
				prefix := slices.Repeat([]string{"frozen"}, MaxLines-5)
				state.History = append(prefix, state.History...)
				state.SegmentStart, state.LiveStart = len(prefix), len(prefix)
			} else if change == "archive-retraction" {
				state.History = append([]string{"A", "B"}, state.History...)
				state.LiveStart = 2
			}
			blank := []string{"H", "", "T", "U"}
			admitPhysicalOccurrence(t, m, blank, ctx, "X", 1)
			if !physicalReturnPending(state) {
				t.Error("missing pending record before invalidation")
			}
			generation := physicalArchiveGeneration(state)
			switch change {
			case "reload":
				if err := m.SaveAll(); err != nil {
					t.Fatal(err)
				}
				m = NewManager(root)
				state = m.loadState("p")
				if physicalArchiveGeneration(state) != 0 {
					t.Error("reload retained runtime archive generation")
				}
			case "terminal-reset":
				ctx.TerminalID = "replacement"
				admitPhysicalOccurrence(t, m, blank, ctx, "X", 0)
				state = m.states["p"]
			case "discard":
				m.Discard("p")
				state = m.loadState("p")
			case "boundary":
				ctx.Columns, ctx.GeometryEpoch = 184, 2
				admitPhysicalOccurrence(t, m, blank, ctx, "X", 1)
			case "archive-retraction":
				d := admitPhysicalOccurrence(t, m, []string{"A", "B", "X", "H", "", "T", "U"}, ctx, "X", 1)
				if d.RetractedRows != 3 || physicalArchiveGeneration(state) <= generation {
					t.Errorf("ordinary reverse did not invalidate by archive mutation: %+v", d)
				}
			case "prefix-trim":
				admitPhysicalOccurrence(t, m, append(blank, "N"), ctx, "X", 1)
				if state.History[state.LiveStart-1] != "X" || physicalArchiveGeneration(state) <= generation {
					t.Error("trim did not advance generation while preserving equal suffix bytes")
				}
			}
			if physicalReturnPending(state) {
				t.Error("lifecycle/archive mutation retained a return record")
			}
		})
	}
}

func TestPhysicalReverseRefusalGuards(t *testing.T) {
	for _, test := range []struct {
		name               string
		archive, old, next []string
		want               int
	}{
		{"one-punctuation", []string{"---"}, []string{"A", "B", "C"}, []string{"---", "A", "B", "C"}, 0},
		{"one-blank", []string{""}, []string{"A", "B", "C"}, []string{"", "A", "B", "C"}, 0},
		{"two-rows", []string{"P", "Q"}, []string{"A", "B", "C"}, []string{"P", "Q", "A", "B", "C"}, 2},
		{"two-rows-one-substantive", []string{"P", "---"}, []string{"A", "B", "C"}, []string{"P", "---", "A", "B", "C"}, 0},
		{"one-row-competing", []string{"A"}, []string{"A", "B", "C"}, []string{"A", "A", "B", "C"}, 0},
		{"competing", []string{"A", "B"}, []string{"A", "B", "C", "D"}, []string{"A", "B", "A", "B", "C", "D"}, 0},
		{"missing-anchor", []string{"P"}, []string{"A", "B", "C"}, []string{"P", "A", "other"}, 0},
		{"segment-floor", []string{"P"}, []string{"A", "B", "C"}, []string{"P", "A", "B", "C"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &PaneState{History: append(slices.Clone(test.archive), test.old...), LiveStart: len(test.archive)}
			if test.name == "segment-floor" {
				state.SegmentStart = state.LiveStart
			}
			plan := planPhysicalTransition(test.old, test.next, true)
			if got := verifiedPhysicalReverse(state, test.old, test.next, &plan); got != test.want {
				t.Errorf("retracted=%d, want %d", got, test.want)
			}
		})
	}
}

func TestPhysicalReverseCombinedProofBudgetGuard(t *testing.T) {
	archive := slices.Repeat([]string{"R"}, 256)
	old := []string{"T", "U", "R"}
	next := append(slices.Clone(archive), "T", "U")
	plan := planPhysicalTransition(old, next, true)
	proof := physicalConsensus(append(slices.Clone(archive), old...), next, true)
	if plan.consensus.budget || !proof.budget || proof.pairs <= MaxPhysicalMatchPairs {
		t.Fatal("fixture does not isolate combined-proof budget refusal")
	}
	state := &PaneState{History: append(slices.Clone(archive), old...), LiveStart: len(archive)}
	if got := verifiedPhysicalReverse(state, old, next, &plan); got != 0 {
		t.Errorf("over-budget combined proof retracted %d rows", got)
	}
}

func TestPhysicalReturnRefusesChangedArchiveIdentity(t *testing.T) {
	for _, change := range []string{"generation", "segment", "suffix"} {
		t.Run(change, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 54)
			admitPhysicalOccurrence(t, m, []string{"H", "X", "T", "U"}, ctx, "X", 1)
			state := m.states["p"]
			state.History = append([]string{"prior"}, state.History...)
			state.LiveStart = 1
			admitPhysicalOccurrence(t, m, []string{"H", "", "T", "U"}, ctx, "X", 1)
			switch change {
			case "generation":
				state.archiveGeneration++
			case "segment":
				state.SegmentStart++
			case "suffix":
				state.History[state.LiveStart-1] = "not X"
			}
			want := 2
			if change == "suffix" {
				want = 1
			}
			d := admitPhysicalOccurrence(t, m, []string{"H", "X", "T", "U"}, ctx, "X", want)
			if d.RetractedRows != 0 || physicalReturnPending(state) {
				t.Error("changed archive identity authorized a slot return")
			}
		})
	}
}

func TestPhysicalArchiveGenerationIgnoresLiveStyleAndFooter(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	admitPhysicalOccurrence(t, m, []string{"H", "X", "T", "U"}, ctx, "X", 1)
	admitPhysicalOccurrence(t, m, []string{"H", "", "T", "U"}, ctx, "X", 1)
	state := m.states["p"]
	generation := state.archiveGeneration
	for _, raw := range []string{
		strings.ReplaceAll(snapshotPage("H", "", "T", "U"), "footer", "fresh-footer"),
		snapshotPage("\x1b[31mH\x1b[0m", "", "T", "U"),
	} {
		m.AdmitSnapshot("p", raw, ctx, 0)
		if state.archiveGeneration != generation || !physicalReturnPending(state) {
			t.Error("live style/footer change mutated archive generation or invalidated its return record")
		}
	}
}

func TestPhysicalArchiveGenerationMutationSites(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	admitPhysicalOccurrence(t, m, []string{"A", "B", "C", "D"}, ctx, "A", 1)
	state := m.states["p"]
	generation := physicalArchiveGeneration(state)
	admitPhysicalOccurrence(t, m, []string{"B", "C", "D", "E"}, ctx, "A", 1)
	if physicalArchiveGeneration(state) <= generation {
		t.Error("forward append did not advance archive generation")
	}
	generation = physicalArchiveGeneration(state)
	admitPhysicalOccurrence(t, m, []string{"A", "B", "C", "D"}, ctx, "A", 1)
	if physicalArchiveGeneration(state) <= generation {
		t.Error("one-row retraction did not advance archive generation")
	}
	generation = physicalArchiveGeneration(state)
	ctx.GeometryEpoch, ctx.Columns = 2, 184
	admitPhysicalOccurrence(t, m, []string{"new A", "new B"}, ctx, "A", 1)
	if physicalArchiveGeneration(state) <= generation {
		t.Error("boundary archive append did not advance archive generation")
	}
	generation = physicalArchiveGeneration(state)
	ctx.ReadSource = SourceLegacy
	m.AdmitSnapshot("p", snapshotPage("legacy A", "legacy B"), ctx, 0)
	if physicalArchiveGeneration(state) <= generation {
		t.Error("legacy archive mutation did not advance archive generation")
	}
	generation = physicalArchiveGeneration(state)
	m.AdmitSnapshot("p", snapshotPage("legacy B", "legacy C"), ctx, 0)
	if physicalArchiveGeneration(state) <= generation {
		t.Error("legacy append did not advance archive generation")
	}
	m = NewManager(t.TempDir())
	ctx = physicalContext(1, 54)
	old := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
	admitPhysicalOccurrence(t, m, old, ctx, "A", 1)
	state = m.states["p"]
	generation = physicalArchiveGeneration(state)
	d := admitPhysicalOccurrence(t, m, []string{"new1", "new2", "new3", "new4", "new5", "new6", "new7", "new8"}, ctx, "A", 1)
	if d.Class != "freeze" || physicalArchiveGeneration(state) <= generation {
		t.Error("freeze archive append did not advance archive generation")
	}
	state.Held = []HeldRecord{{Rows: []string{"held"}, Position: state.LiveStart, Segment: state.SegmentStart, FirstSeenAt: time.Now().UTC()}}
	physicalHeld(t, state)
}

func TestFullHistoryClearedSlotReturn(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	old, blank := []string{"H", "X", "T", "U"}, []string{"H", "", "T", "U"}
	admitPhysicalOccurrence(t, m, old, ctx, "X", 1)
	state := m.states["p"]
	prefix := make([]string, MaxLines-len(old))
	for i := range prefix {
		prefix[i] = fmt.Sprintf("archive-%d", i)
	}
	state.History = append(prefix, state.History...)
	state.LiveStart = len(prefix)
	for cycle := range 3 {
		admitPhysicalOccurrence(t, m, blank, ctx, "X", 1)
		if state.zeroDisplacementReturn == nil {
			t.Errorf("cycle %d: same-admission trim cleared the new return record", cycle)
		}
		d := admitPhysicalOccurrence(t, m, old, ctx, "X", 1)
		if d.RetractedRows != 1 {
			t.Errorf("cycle %d: full-history return did not retract: %+v", cycle, d)
		}
	}
}
