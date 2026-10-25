package history

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

var boundaryProse = []string{
	"Amber lanterns guide quiet ships beside distant rocky shores.",
	"Patient keepers tend bright lamps through heavy winter storms.",
	"Morning sunlight reveals calm waters beyond the sheltered harbor.",
}

func boundaryCounter(d SnapshotAdmission, name string) int {
	switch name {
	case "BoundaryReconciledRows":
		return d.BoundaryReconciledRows
	case "BoundaryRefused":
		return d.BoundaryRefused
	case "BoundarySeamKept":
		return d.BoundarySeamKept
	default:
		return 0
	}
}

func TestBoundaryRepeatedBlocksAcrossSeparatorsRefuse(t *testing.T) {
	for _, separator := range []string{"", "---", "·", "a distinct substantive separator"} {
		t.Run(fmt.Sprintf("separator=%q", separator), func(t *testing.T) {
			old := append(slices.Clone(boundaryProse), separator)
			old = append(old, boundaryProse...)
			result := matchBoundarySuffix(old, 0, boundaryProse)
			if !result.refused || result.rows != 0 {
				t.Errorf("repeated archive blocks authorized a cut across separator: cut=%d rows=%d", result.cut, result.rows)
			}
		})
	}
	old := append([]string{"an unrelated earlier transcript row", "---"}, boundaryProse...)
	if result := matchBoundarySuffix(old, 0, boundaryProse); result.refused || result.cut != 2 {
		t.Errorf("genuine non-repeat did not reconcile: %+v", result)
	}
}

func TestBoundaryClippedWrappedPromptRefuses(t *testing.T) {
	old := []string{"❯ explain this topic", "alpha bravo charlie delta echo foxtrot", "golf hotel india juliet kilo lima"}
	if result := matchBoundarySuffix(old, 0, old[1:]); !result.refused || result.rows != 0 {
		t.Errorf("wrapped prompt continuation counted as prose: cut=%d rows=%d", result.cut, result.rows)
	}
}

func TestBoundaryBoundedDiscoveryRefusesExhaustion(t *testing.T) {
	t.Run("distant-repeat-inside-window", func(t *testing.T) {
		old := append(slices.Clone(boundaryProse), "distinct intervening prose", "---", "")
		floor := len(old)
		old = append(old, boundaryProse...)
		incoming := append(slices.Clone(boundaryProse), "fresh output extends the comparison window beyond the repeated block")
		if result := matchBoundarySuffix(old, 0, incoming); !result.refused {
			t.Error("earlier occurrence inside the discovery window did not refuse")
		}
		if result := matchBoundarySuffix(old, floor, incoming); result.refused || result.cut != floor {
			t.Errorf("earlier segment affected the suffix proof: %+v", result)
		}
	})
	t.Run("comparisons", func(t *testing.T) {
		row := strings.Repeat("amber ", 255) + "blue"
		incoming := []string{strings.Repeat("amber ", 256), strings.Repeat("amber ", 255) + "blue"}
		old := append(slices.Repeat([]string{row}, 3), "")
		old = append(old, incoming...)
		result := matchBoundarySuffix(old, 0, incoming)
		if !result.refused || result.rows != 0 || result.work.comparisons != MaxBoundaryComparisons {
			t.Errorf("comparison budget authorized a clipped uniqueness proof: %+v", result)
		}
	})
	for _, test := range []struct {
		name, prefix string
	}{
		{"raw", strings.Repeat(" ", MaxBoundaryRawBytes+1)},
		{"normalized", strings.Repeat("x", MaxBoundaryNormalizedBytes)},
		{"tokens", strings.Repeat("x ", MaxBoundaryTokens)},
	} {
		t.Run(test.name, func(t *testing.T) {
			old := append([]string{test.prefix, "unrelated substantive context", "---", ""}, boundaryProse...)
			result := matchBoundarySuffix(old, 0, boundaryProse)
			if !result.refused || result.rows != 0 || result.work.rawBytes > MaxBoundaryRawBytes || result.work.normalizedBytes > MaxBoundaryNormalizedBytes || result.work.tokens > MaxBoundaryTokens {
				t.Errorf("incomplete uniqueness or prompt context authorized deletion: %+v", result)
			}
		})
	}
}

func TestBoundaryPromptContextStopsAtBlankOrFloor(t *testing.T) {
	old := append([]string{"❯ explain this topic", "wrapped prompt continuation", ""}, boundaryProse...)
	if result := matchBoundarySuffix(old, 0, boundaryProse); result.refused || result.cut != 3 {
		t.Errorf("blank did not end prompt context: %+v", result)
	}
	old = []string{"❯ explain this topic", "alpha bravo charlie delta echo foxtrot", "golf hotel india juliet kilo lima"}
	if result := matchBoundarySuffix(old, 1, old[1:]); result.refused || result.cut != 1 {
		t.Errorf("prompt before segment floor affected classification: %+v", result)
	}
}

func TestBoundaryLongLegacySegmentUsesBoundedDiscovery(t *testing.T) {
	old := make([]string, MaxLines-len(boundaryProse)-1)
	maxRow := 0
	for i := range old {
		old[i] = fmt.Sprintf("Earlier row %05d records distinct prose about patient harbor keepers.", i)
		maxRow = max(maxRow, len(old[i]))
	}
	old = append(old, "")
	old = append(old, boundaryProse...)
	limit := len(strings.Join(boundaryProse, ""))
	result := matchBoundarySuffix(old, 0, boundaryProse)
	archiveBytes := result.work.normalizedBytes - limit
	if result.refused || result.cut != len(old)-len(boundaryProse) || archiveBytes < 2*limit || archiveBytes > 2*limit+2*maxRow || result.work.normalizedRows > 20 || result.work.tokens > 200 {
		t.Fatalf("long segment did not use bounded discovery: %+v", result)
	}
	if result.work.rawBytes != 2*result.work.normalizedBytes {
		t.Errorf("plain-row inspection accounting=%+v", result.work)
	}
	t.Logf("rows=%d L=%d archive_normalized_bytes=%d work=%+v", len(old), limit, archiveBytes, result.work)
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	ctx.ReadSource = SourceLegacy
	ctx.Columns = 0
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	ctx.ReadSource = SourceRecent
	ctx.Columns = 62
	_, _, d := m.AdmitSnapshot("p", snapshotPage(boundaryProse...), ctx, 0)
	if d.BoundaryReconciledRows != len(boundaryProse) || d.BoundaryRefused != 0 || m.states["p"].LiveStart != len(old)-len(boundaryProse) {
		t.Errorf("long legacy-to-physical boundary retained a copy: %+v", d)
	}
}

func TestBoundaryRepeatBeyondWindowIsAcceptedResidual(t *testing.T) {
	limit := len(strings.Join(boundaryProse, ""))
	old := slices.Clone(boundaryProse)
	context := len(old)
	old = append(old, "Nearest preceding substantive context")
	window := strings.Repeat("x", limit)
	old = append(old, window, "")
	cut := len(old)
	old = append(old, boundaryProse...)
	work := boundaryWork{}
	view, ok := boundaryArchive(old, 0, limit, &work)
	if !ok || view.firstRow != context || !slices.Equal(view.rows, old[context:]) {
		t.Fatalf("discovery crossed the row before its bounded context: first=%d work=%+v", view.firstRow, work)
	}
	result := matchBoundarySuffix(old, 0, boundaryProse)
	if result.refused || result.cut != cut || result.rows != len(boundaryProse) {
		t.Errorf("repeat just beyond bounded discovery did not remain an accepted residual: %+v", result)
	}
}

func TestBoundaryDiscoveryExtendsThroughLayoutToSubstantiveRow(t *testing.T) {
	limit := len(strings.Join(boundaryProse, ""))
	old := []string{strings.Repeat(" ", MaxBoundaryRawBytes+1), "nearest substantive context", "", "---", "·", "", strings.Repeat("x", limit)}
	cut := len(old)
	old = append(old, boundaryProse...)
	work := boundaryWork{}
	view, ok := boundaryArchive(old, 0, limit, &work)
	if !ok || view.firstRow != 1 || !slices.Equal(view.rows, old[1:]) || work.normalizedRows != len(old)-1 {
		t.Fatalf("bounded discovery skipped layout context or read past it: first=%d work=%+v", view.firstRow, work)
	}
	if result := matchBoundarySuffix(old, 0, boundaryProse); result.refused || result.cut != cut {
		t.Errorf("bounded context read the oversized row beyond its substantive stop: %+v", result)
	}
}

func TestBoundaryClippedPromptContextWindowRefuses(t *testing.T) {
	old := slices.Repeat([]string{"Continuous prose without a blank leaves preceding prompt context unknown."}, 20)
	old = append(old, boundaryProse...)
	result := matchBoundarySuffix(old, 0, boundaryProse)
	if !result.refused || result.rows != 0 {
		t.Errorf("window start without a blank or segment floor authorized deletion: %+v", result)
	}
}

func TestBoundaryWholeRowSuffixReconciliation(t *testing.T) {
	for _, direction := range []string{"physical-width", "legacy-physical", "physical-legacy"} {
		t.Run(direction, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 150)
			if direction == "legacy-physical" {
				ctx.ReadSource = SourceLegacy
				ctx.Columns = 0
			}
			old := append([]string{"prior unrelated transcript"}, boundaryProse...)
			m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
			ctx.Columns, ctx.GeometryEpoch = 62, 2
			if direction == "legacy-physical" {
				ctx.ReadSource = SourceRecent
			}
			if direction == "physical-legacy" {
				ctx.ReadSource = SourceLegacy
				ctx.Columns = 0
			}
			incoming := []string{strings.Join(boundaryProse[:2], " "), boundaryProse[2], "fresh output continues here"}
			content, _, d := m.AdmitSnapshot("p", snapshotPage(incoming...), ctx, 0)
			state := m.states["p"]
			if state.LiveStart != 1 && ctx.ReadSource == SourceRecent || state.SegmentStart != 1 || !slices.Equal(state.History[:state.SegmentStart], old[:1]) {
				t.Errorf("boundary retained duplicate suffix: segment=%d live=%d archive=%q", state.SegmentStart, state.LiveStart, state.History[:state.SegmentStart])
			}
			if content != snapshotPage(append([]string{old[0]}, incoming...)...) {
				t.Error("reconciliation did not preserve prefix plus exact raw page")
			}
			if boundaryCounter(d, "BoundaryReconciledRows") != 3 || boundaryCounter(d, "BoundaryRefused") != 0 {
				t.Errorf("boundary diagnostics=%+v", d)
			}
		})
	}
}

func TestBoundaryPartialSeamKeepsOneArchiveRow(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	old := append([]string{"prior unrelated transcript", "earlier words " + boundaryProse[0]}, boundaryProse[1:]...)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	ctx.Columns = 62
	_, _, d := m.AdmitSnapshot("p", snapshotPage(boundaryProse...), ctx, 0)
	state := m.states["p"]
	if state.LiveStart != 2 || !slices.Equal(state.History[:state.LiveStart], old[:2]) || boundaryCounter(d, "BoundarySeamKept") != 1 || boundaryCounter(d, "BoundaryReconciledRows") != 2 {
		t.Errorf("partial seam live=%d diagnostics=%+v", state.LiveStart, d)
	}
}

func TestBoundaryDeletionClampsHeldAndInvalidatesReturn(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	old := []string{"prior", boundaryProse[0], "", boundaryProse[1]}
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	state := m.states["p"]
	stamp := time.Now().UTC()
	state.LiveStart = 4
	state.SegmentStart = 0
	state.Held = []HeldRecord{{Rows: []string{"before"}, Position: 0, FirstSeenAt: stamp}, {Rows: []string{"at"}, Position: 1, Segment: 1, FirstSeenAt: stamp}, {Rows: []string{"after"}, Position: 2, Segment: 2, FirstSeenAt: stamp}, {Rows: []string{"equal"}, Position: 2, Segment: 2, FirstSeenAt: stamp}}
	generation := state.archiveGeneration
	ctx.Columns = 62
	_, _, d := m.AdmitSnapshot("p", snapshotPage(boundaryProse[0], "", boundaryProse[1]), ctx, 0)
	if state.LiveStart != 1 || state.SegmentStart != 1 || state.archiveGeneration <= generation || boundaryCounter(d, "BoundaryReconciledRows") != 3 {
		t.Errorf("suffix deletion coordinates live=%d segment=%d generation=%d diagnostics=%+v", state.LiveStart, state.SegmentStart, state.archiveGeneration, d)
	}
	for i, record := range state.Held {
		if record.Position != min(i, 1) || record.Segment > record.Position || !record.FirstSeenAt.Equal(stamp) {
			t.Errorf("held clamp/order=%+v", state.Held)
		}
	}
}

func TestBoundaryWorkBudgets(t *testing.T) {
	t.Run("raw-before-normalization", func(t *testing.T) {
		for _, raw := range []string{strings.Repeat(" ", MaxBoundaryRawBytes+1), strings.Repeat("\x1b[31m", MaxBoundaryRawBytes/5+1)} {
			work := boundaryWork{}
			if _, ok := work.row(raw); ok || work.rawBytes != 0 || work.normalizedRows != 0 || work.normalizedBytes != 0 {
				t.Errorf("oversized raw row allocated normalized view: %+v", work)
			}
		}
	})
	t.Run("raw-at-limit", func(t *testing.T) {
		work := boundaryWork{}
		if _, ok := work.row(strings.Repeat(" ", MaxBoundaryRawBytes)); !ok || work.rawBytes != MaxBoundaryRawBytes {
			t.Errorf("raw edge=%+v", work)
		}
		if _, ok := work.row("x"); ok || work.normalizedRows != 1 {
			t.Error("cumulative raw exhaustion normalized an uncharged row")
		}
	})
	t.Run("normalized", func(t *testing.T) {
		work := boundaryWork{}
		if _, ok := work.row(strings.Repeat("x", MaxBoundaryNormalizedBytes)); !ok {
			t.Fatal("normalized edge refused")
		}
		if _, ok := work.row("x"); ok || work.normalizedRows != 1 {
			t.Errorf("normalized overflow created view: %+v", work)
		}
	})
	t.Run("tokens", func(t *testing.T) {
		work := boundaryWork{}
		if _, ok := work.addTokens(strings.Repeat("x ", MaxBoundaryTokens), 0); !ok {
			t.Fatal("token edge refused")
		}
		if _, ok := work.addTokens("x", 1); ok || work.tokens != MaxBoundaryTokens {
			t.Errorf("token overflow=%+v", work)
		}
	})
	t.Run("comparisons", func(t *testing.T) {
		work := boundaryWork{}
		for range MaxBoundaryComparisons {
			if _, ok := work.equal("x", "x"); !ok {
				t.Fatal("comparison edge refused")
			}
		}
		if _, ok := work.equal("x", "x"); ok || work.comparisons != MaxBoundaryComparisons {
			t.Error("comparison overflow accepted")
		}
	})
	t.Run("candidates", func(t *testing.T) {
		incoming := slices.Repeat([]string{"last"}, MaxBoundaryCandidates+1)
		result := matchBoundarySuffix([]string{"first words with enough support for a candidate", "last"}, 0, incoming)
		if !result.refused || result.work.candidates != MaxBoundaryCandidates {
			t.Errorf("candidate exhaustion=%+v", result)
		}
	})
}

func TestBoundaryRawBudgetRefusalLeavesArchive(t *testing.T) {
	for _, side := range []string{"archive", "incoming"} {
		for _, kind := range []string{"ansi", "whitespace"} {
			t.Run(side+"/"+kind, func(t *testing.T) {
				m := NewManager(t.TempDir())
				ctx := physicalContext(1, 150)
				oversized := strings.Repeat(" ", MaxBoundaryRawBytes+1)
				if kind == "ansi" {
					oversized = strings.Repeat("\x1b[31m", MaxBoundaryRawBytes/5+1)
				}
				old, next := slices.Clone(boundaryProse), slices.Clone(boundaryProse)
				if side == "archive" {
					old = append(old, oversized)
				} else {
					next = append(next, oversized)
				}
				m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
				ctx.Columns = 62
				_, _, d := m.AdmitSnapshot("p", snapshotPage(next...), ctx, 0)
				state := m.states["p"]
				if d.BoundaryRefused != 1 || d.BoundaryReconciledRows != 0 || !slices.Equal(state.History[:state.LiveStart], old) {
					t.Error("raw-budget refusal modified archive or counted incorrectly")
				}
			})
		}
	}
}

func TestBoundaryNormalizationKeepsRawBytes(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	old := []string{"\x1b[31m" + strings.ReplaceAll(boundaryProse[0], " ", " ") + "\x1b[0m\r", boundaryProse[1]}
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	ctx.Columns = 62
	raw := snapshotPage(boundaryProse[:2]...)
	got, _, d := m.AdmitSnapshot("p", raw, ctx, 0)
	if got != raw || d.BoundaryReconciledRows != 2 || old[0][0] != '\x1b' {
		t.Errorf("normalization changed raw rows or failed agreement: %+v", d)
	}
}

func TestBoundaryLookbackFloorAndFirstMismatch(t *testing.T) {
	for _, floor := range []int{0, 2} {
		old := append([]string{boundaryProse[0], "substantive mismatch"}, boundaryProse[1:]...)
		result := matchBoundarySuffix(old, floor, boundaryProse)
		if result.refused || result.cut != 2 || result.rows != 2 {
			t.Errorf("lookback floor=%d crossed substantive mismatch: %+v", floor, result)
		}
	}
	old := append(slices.Clone(boundaryProse), "new segment unrelated")
	result := matchBoundarySuffix(old, len(boundaryProse), boundaryProse)
	if !result.refused || result.rows != 0 {
		t.Error("boundary search crossed earlier segment")
	}
}

func TestBoundaryPendingReturnInvalidatedBySuffixDeletion(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage("H", "X", "T", "U"), ctx, 0)
	m.AdmitSnapshot("p", snapshotPage("H", "", "T", "U"), ctx, 0)
	state := m.states["p"]
	if state.zeroDisplacementReturn == nil {
		t.Fatal("missing pending return")
	}
	state.History = append(slices.Clone(boundaryProse), state.History...)
	state.LiveStart += len(boundaryProse)
	state.History = state.History[:len(boundaryProse)]
	state.LiveStart = len(state.History)
	generation := state.archiveGeneration
	result := reconcileBoundary(state, boundaryProse, true)
	if result.rows != 3 || state.archiveGeneration <= generation || state.zeroDisplacementReturn != nil {
		t.Error("reconciliation left pending return or generation unchanged")
	}
}

func TestBoundaryPreviewSaveAndReloadCoordinates(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	ctx := physicalContext(1, 150)
	old := append([]string{"prior"}, boundaryProse...)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	state := m.states["p"]
	state.Held = []HeldRecord{{Rows: []string{"held"}, Position: 3, Segment: 2, FirstSeenAt: time.Now().UTC()}}
	state.LiveStart = 4
	before := slices.Clone(state.History)
	generation := state.archiveGeneration
	ctx.ReadSource = SourceLegacy
	ctx.Columns = 0
	if _, ok := m.PreviewSnapshot("p", snapshotPage(boundaryProse...), ctx); !ok || !slices.Equal(state.History, before) || state.archiveGeneration != generation || state.Held[0].Position != 3 {
		t.Fatal("boundary preview changed live state")
	}
	_, _, d := m.AdmitSnapshot("p", snapshotPage(boundaryProse...), ctx, 0)
	if d.BoundaryReconciledRows != 3 || !state.dirty || state.Held[0].Position != 1 || state.Held[0].Segment != 1 {
		t.Fatalf("deletion metadata not durable: %+v held=%+v", d, state.Held)
	}
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	loaded := NewManager(root).loadState("p")
	if loaded.SegmentStart != 1 || loaded.LiveStart != 4 || loaded.Held[0].Position != 1 || loaded.Held[0].Segment != 1 {
		t.Fatalf("reload coordinates=%+v", loaded)
	}
	loaded.History = append(make([]string, MaxLines), loaded.History...)
	loaded.LiveStart += MaxLines
	loaded.SegmentStart += MaxLines
	loaded.Held[0].Position += MaxLines
	loaded.Held[0].Segment += MaxLines
	trimSnapshotState(loaded)
	physicalHeld(t, loaded)
}

func TestBoundaryQueuedSaveKeepsNewerDeletionDirty(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage(append([]string{"prior"}, boundaryProse...)...), ctx, 0)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	m.beforeRename = func() { close(entered); <-release }
	go func() { done <- m.SaveAll() }()
	<-entered
	ctx.Columns = 62
	m.AdmitSnapshot("p", snapshotPage(boundaryProse...), ctx, 0)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !m.states["p"].dirty {
		t.Fatal("old save cleared suffix deletion dirty state")
	}
	m.beforeRename = nil
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	loaded := persistedState(t, m, "p")
	if loaded.SegmentStart != 1 || loaded.LiveStart != 1 {
		t.Fatalf("final save missed deletion: %+v", loaded)
	}
}

func TestBoundaryDiscardReplacementFencesDeletionSave(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 150)
			m.AdmitSnapshot("p", snapshotPage(append([]string{"prior"}, boundaryProse...)...), ctx, 0)
			ctx.Columns = 62
			m.AdmitSnapshot("p", snapshotPage(boundaryProse...), ctx, 0)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			m.beforeRename = func() { close(entered); <-release }
			go func() { done <- m.SaveAll() }()
			<-entered
			if replace {
				ctx.TerminalID = "replacement"
				m.AdmitSnapshot("p", snapshotPage("new"), ctx, 0)
			} else {
				m.Discard("p")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(m.stateFile("p")); !os.IsNotExist(err) {
				t.Errorf("obsolete deletion save recreated file: %v", err)
			}
		})
	}
}

func TestBoundaryMinimumRunAtAndBelowLimits(t *testing.T) {
	for _, words := range []int{11, 12} {
		rows := []string{strings.Repeat("amber ", 6), strings.Repeat("blue ", words-6)}
		result := matchBoundarySuffix(rows, 0, rows)
		if words == 12 && (result.refused || result.words != 12 || result.substantiveRows != 2) {
			t.Errorf("minimum run edge=%+v", result)
		}
		if words == 11 && !result.refused {
			t.Error("eleven-word run accepted")
		}
	}
}

func TestBoundaryNormalizedViewRescanIsCharged(t *testing.T) {
	work := boundaryWork{rawBytes: MaxBoundaryRawBytes - 5}
	if _, ok := work.row("four"); ok || work.normalizedRows != 0 || work.rawBytes != MaxBoundaryRawBytes-1 {
		t.Errorf("second pass escaped raw charge: %+v", work)
	}
	work = boundaryWork{rawBytes: MaxBoundaryRawBytes - 8}
	if row, ok := work.row("four"); !ok || row != "four" || work.rawBytes != MaxBoundaryRawBytes {
		t.Errorf("second-pass raw edge refused: %+v", work)
	}
}

func TestBoundaryRawDiscoveryChargesOversizedPrecedingRow(t *testing.T) {
	old := append([]string{strings.Repeat(" ", MaxBoundaryRawBytes+1)}, boundaryProse...)
	result := matchBoundarySuffix(old, 0, boundaryProse)
	if !result.refused || result.work.rawBytes > MaxBoundaryRawBytes || result.work.normalizedRows != 6 {
		t.Errorf("backward raw discovery bypassed budget: %+v", result)
	}
}

func TestBoundaryUnknownProvenanceRefuses(t *testing.T) {
	for _, kind := range []string{"unknown-old-width", "unknown-new-width", "invalid"} {
		m := NewManager(t.TempDir())
		ctx := physicalContext(1, 150)
		m.AdmitSnapshot("p", snapshotPage(boundaryProse...), ctx, 0)
		state := m.states["p"]
		if kind == "unknown-old-width" {
			state.Columns = 0
		}
		if kind == "invalid" {
			state.snapshotInvalid = true
		}
		ctx.Columns = 62
		if kind == "unknown-new-width" {
			ctx.Columns = 0
		}
		_, _, d := m.AdmitSnapshot("p", snapshotPage(boundaryProse...), ctx, 0)
		if d.BoundaryRefused != 1 || state.LiveStart != 3 {
			t.Errorf("unknown provenance cut archive: %s %+v", kind, d)
		}
	}
}

func TestBoundaryRefusalGuards(t *testing.T) {
	for _, test := range []struct {
		name      string
		old, next []string
	}{
		{"too-short", []string{"one two three", "four five six"}, []string{"one two three four five six"}},
		{"one-archive-row", []string{strings.Join(boundaryProse, " ")}, boundaryProse},
		{"repeated-incoming", boundaryProse, append(slices.Clone(boundaryProse), boundaryProse...)},
		{"repeated-archive", append(slices.Clone(boundaryProse), boundaryProse...), boundaryProse},
		{"case-change", boundaryProse, []string{strings.ToLower(boundaryProse[0]), boundaryProse[1], boundaryProse[2]}},
		{"punctuation-change", boundaryProse, []string{boundaryProse[0], boundaryProse[1], strings.TrimSuffix(boundaryProse[2], ".") + "!"}},
		{"code", []string{"func lanterns() { return bright }", "the remaining words remain in this code block"}, []string{"func lanterns() { return bright } the remaining words remain in this code block"}},
		{"table", []string{"Name | Path | Purpose", "Lamp | synthetic/file.go | guides quiet ships through dark waters"}, []string{"Name | Path | Purpose Lamp | synthetic/file.go | guides quiet ships through dark waters"}},
		{"split-token", []string{"a long synthetic https://example.com/part", "remaining words stretch across another row with more words"}, []string{"a long synthetic https://example.com/partremaining words stretch across another row with more words"}},
		{"unicode-cut", []string{"Lanterns guide quiet ships beside distant rocky shores.", "Café keepers tend bright lamps through heavy winter storms."}, []string{"Lanterns guide quiet ships beside distant rocky shores. Cafe", "́ keepers tend bright lamps through heavy winter storms."}},
		{"logo-clear", boundaryProse, []string{"LOGO ROW", "----------", "❯ clear the screen and begin a different conversation"}},
		{"noncontinuing-tail", append(slices.Clone(boundaryProse), "different substantive chrome"), boundaryProse},
		{"unmatched-trailing-layout", append(slices.Clone(boundaryProse), ""), boundaryProse},
		{"prompt-only", []string{"❯ explain why lanterns guide quiet ships beside distant shores", "  and describe patient keepers through heavy winter storms"}, []string{"❯ explain why lanterns guide quiet ships beside distant shores and describe patient keepers through heavy winter storms"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 150)
			m.AdmitSnapshot("p", snapshotPage(test.old...), ctx, 0)
			ctx.Columns = 62
			_, _, d := m.AdmitSnapshot("p", snapshotPage(test.next...), ctx, 0)
			state := m.states["p"]
			if state.LiveStart != len(test.old) || boundaryCounter(d, "BoundaryRefused") != 1 {
				t.Errorf("unsupported boundary deleted rows or omitted refusal: live=%d want=%d diagnostics=%+v", state.LiveStart, len(test.old), d)
			}
		})
	}
}
