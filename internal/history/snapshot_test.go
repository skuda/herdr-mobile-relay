package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func physicalContext(epoch uint64, columns int) SnapshotContext {
	return SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceRecent, GeometryEpoch: epoch, LeaseRevision: 1, Columns: columns, Rows: 82}
}

func snapshotPage(body ...string) string {
	return strings.Join(append(slices.Clone(body), "footer1", "footer2", "footer3", "footer4", "footer5", "footer6"), "\n")
}

func assertSnapshotContent(t *testing.T, got string, body ...string) {
	t.Helper()
	if want := snapshotPage(body...); got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestSnapshotEpochBoundaryAppendsWholePage(t *testing.T) {
	m := NewManager(t.TempDir())
	m.MergeSnapshot("p", snapshotPage("A", "B", "old1", "old2"), physicalContext(1, 120))
	got := m.MergeSnapshot("p", snapshotPage("prefix", "A", "B", "suffix"), physicalContext(2, 40))
	assertSnapshotContent(t, got, "A", "B", "old1", "old2", "prefix", "A", "B", "suffix")
	if state := m.states["p"]; state.SegmentStart != 4 || state.LiveStart != 4 {
		t.Fatalf("boundary offsets = %+v", state)
	}
}

func TestSnapshotCannotMatchEarlierEpochAtSameWidth(t *testing.T) {
	m := NewManager(t.TempDir())
	for i, cols := range []int{120, 40, 120} {
		m.MergeSnapshot("p", snapshotPage("A", "B", fmt.Sprint(i)), physicalContext(uint64(i+1), cols))
	}
	assertSnapshotContent(t, m.Content("p", 0), "A", "B", "0", "A", "B", "1", "A", "B", "2")
}

func TestSnapshotServesArchiveAndExactCurrentRawPage(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage("old A", "old B", "old C", "old D"), ctx)
	raw := "\x1b[44mnew A\x1b[0m\nnew B\n\nnew C\n\x1b[31mf1\x1b[0m\nf2\nf3\nf4\nf5\nf6\n"
	got := m.MergeSnapshot("p", raw, ctx)
	// Replaces the old mandatory fallback prefix with separately held residue.
	if !slices.Equal(physicalHeldRows(t, m.states["p"]), []string{"old A", "old B", "old C", "old D"}) {
		t.Fatal("old raw body was not held")
	}
	if want := raw; got != want {
		t.Fatalf("composition = %q, want %q", got, want)
	}
}

func TestSnapshotFixedHeaderScrollPreservesDisplacedRows(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage("H1", "H2", "A", "B", "C", "D", "E"), ctx)
	got := m.MergeSnapshot("p", snapshotPage("H1", "H2", "C", "D", "E", "F", "G"), ctx)
	assertSnapshotContent(t, got, "A", "B", "H1", "H2", "C", "D", "E", "F", "G")
}

func TestLegacyFixedHeaderFixtureRetainsExistingResult(t *testing.T) {
	m := NewManager(t.TempDir())
	m.Merge("p", snapshotPage("H1", "H2", "A", "B", "C", "D", "E"))
	got := m.Merge("p", snapshotPage("H1", "H2", "C", "D", "E", "F", "G"))
	assertSnapshotContent(t, got, "H1", "H2", "A", "B", "C", "D", "E", "F", "G")
}

func TestSnapshotScrollWithoutHeaderCommitsDisplacedPrefix(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage("A", "B", "C", "D", "E"), ctx)
	got := m.MergeSnapshot("p", snapshotPage("C", "D", "E", "F", "G"), ctx)
	assertSnapshotContent(t, got, "A", "B", "C", "D", "E", "F", "G")
}

func TestSnapshotTimerOnlyUpdatesHaveZeroArchiveGrowth(t *testing.T) {
	for tail := 1; tail <= 3; tail++ {
		t.Run(fmt.Sprint(tail), func(t *testing.T) {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 40)
			body := make([]string, 76)
			for i := range body {
				body[i] = fmt.Sprintf("marker-%03d", i)
			}
			for frame := 0; frame < 150; frame++ {
				for i := 76 - tail; i < 76; i++ {
					body[i] = fmt.Sprintf("progress row %d: (%ds, %d tokens)", i, frame, frame*3)
				}
				got := m.MergeSnapshot("p", snapshotPage(body...), ctx)
				if state := m.states["p"]; len(state.History) != 76 || state.LiveStart != 0 || got != snapshotPage(body...) {
					t.Fatalf("frame %d history=%d live=%d content=%q", frame, len(state.History), state.LiveStart, got)
				}
			}
		})
	}
}

func TestSnapshotScrollWithChangedProgressTailArchivesDisplacedRowsOnce(t *testing.T) {
	for _, header := range [][]string{nil, {"H1", "H2"}} {
		for tail := 1; tail <= 3; tail++ {
			t.Run(fmt.Sprintf("header%d/tail%d", len(header), tail), func(t *testing.T) {
				m := NewManager(t.TempDir())
				ctx := physicalContext(1, 40)
				old := append(slices.Clone(header), "A", "B", "C", "D", "E")
				next := append(slices.Clone(header), "C", "D", "E", "F", "G")
				for i := range tail {
					old = append(old, fmt.Sprintf("old spinner %d", i))
					next = append(next, fmt.Sprintf("new spinner %d", i))
				}
				m.MergeSnapshot("p", snapshotPage(old...), ctx)
				got := m.MergeSnapshot("p", snapshotPage(next...), ctx)
				assertSnapshotContent(t, got, append([]string{"A", "B"}, next...)...)
				if strings.Contains(got, "old spinner") || m.states["p"].LiveStart != 2 {
					t.Fatalf("old tail archived: %q", got)
				}
			})
		}
	}
}

// Replaces TestSnapshotTailLongerThanBoundFallsBack's whole-page fallback assertion.
func TestSnapshotTailLongerThanBoundStaysHeld(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	old := []string{"A", "B", "old1", "old2", "old3", "old4"}
	next := []string{"A", "B", "new1", "new2", "new3", "new4"}
	m.MergeSnapshot("p", snapshotPage(old...), ctx)
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(next...), ctx), next...)
	if !slices.Equal(physicalHeldRows(t, m.states["p"]), old[2:]) {
		t.Fatal("unexplained old rows were not held")
	}
}

func TestSnapshotChangedTailMajorityFreezesAtMinimumEight(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	old := []string{"A", "B", "C", "old1", "old2", "old3", "old4", "old5"}
	next := []string{"A", "B", "C", "new1", "new2", "new3", "new4", "new5"}
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	got, _, d := m.AdmitSnapshot("p", snapshotPage(next...), ctx, 0)
	state := m.states["p"]
	if d.Class != "freeze" || d.Reason != "held_majority" || d.HeldNonblank != 5 || d.OldNonblank != 8 {
		t.Fatalf("changed-tail diagnostic=%+v", d)
	}
	if !slices.Equal(state.History[:state.LiveStart], old) || len(state.Held) != 0 {
		t.Fatal("majority changed tail did not freeze the old body once")
	}
	assertSnapshotContent(t, got, append(slices.Clone(old), next...)...)
}

// Replaces TestSnapshotEquivalentCandidatesChooseSmallestTailThenDisplacementThenHeader.
func TestSnapshotConsensusDoesNotChooseDisputedOccurrence(t *testing.T) {
	result := physicalConsensus([]string{"A", "B", "A"}, []string{"A", "B"}, true)
	if !slices.Equal(result.agreed, []physicalPair{{0, 0}, {1, 1}}) {
		t.Fatalf("mandatory pairs=%v", result.agreed)
	}
	disputed := physicalConsensus([]string{"A", "A"}, []string{"A"}, true)
	if len(disputed.agreed) != 0 || len(disputed.feasible) != 2 {
		t.Fatal("competing equal-support occurrences were selected")
	}
}

// Replaces TestSnapshotConflictingDisplacedRangesFallBack's helper conflict and page copy.
func TestSnapshotEqualPrefixTimerReplacesMiddle(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	old := []string{"A", "B", "A", "B", "A", "B", "old"}
	next := []string{"A", "B", "A", "B", "A", "B", "new"}
	m.MergeSnapshot("p", snapshotPage(old...), ctx)
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(next...), ctx), next...)
	if m.states["p"].LiveStart != 0 || len(m.states["p"].Held) != 0 {
		t.Fatal("timer update grew archive or holds")
	}
}

func TestSnapshotStreamingExtensionKeepsArchive(t *testing.T) {
	for _, pair := range [][2][]string{
		{{"A", "B"}, {"A", "B", "C"}},
		{{"A", "partial"}, {"A", "partial completion"}},
	} {
		m := NewManager(t.TempDir())
		ctx := physicalContext(1, 40)
		m.MergeSnapshot("p", snapshotPage(pair[0]...), ctx)
		assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(pair[1]...), ctx), pair[1]...)
		if m.states["p"].LiveStart != 0 {
			t.Fatal("streaming extension grew archive")
		}
	}
}

// Replaces TestSnapshotStrictlyShorterPageRetainsPreviousBody's whole-page fallback assertion.
func TestSnapshotStrictlyShorterPageHoldsContraction(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	old := []string{"A", "B", "C", "D", "E"}
	next := []string{"A", "B"}
	m.MergeSnapshot("p", snapshotPage(old...), ctx)
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(next...), ctx), next...)
	if !slices.Equal(physicalHeldRows(t, m.states["p"]), old[2:]) {
		t.Fatal("unexplained old rows were not held")
	}
}

// Replaces TestSnapshotReverseMovementFallsBackWithoutDeletingPriorPage's mandatory page copy.
func TestSnapshotReverseMovementHoldsPushedRows(t *testing.T) {
	for _, header := range [][]string{nil, {"H1", "H2"}} {
		for _, tail := range []bool{false, true} {
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 40)
			old := append(slices.Clone(header), "C", "D", "E", "F", "G")
			next := append(slices.Clone(header), "A", "B", "C", "D", "E")
			if tail {
				old, next = append(old, "new spinner"), append(next, "old spinner")
			}
			m.MergeSnapshot("p", snapshotPage(old...), ctx)
			assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(next...), ctx), next...)
			wantHeld := []string{"F", "G"}
			if tail {
				wantHeld = append(wantHeld, "new spinner")
			}
			if !slices.Equal(physicalHeldRows(t, m.states["p"]), wantHeld) {
				t.Fatalf("reverse residue=%q, want %q", physicalHeldRows(t, m.states["p"]), wantHeld)
			}
		}
	}
}

// Replaces TestSnapshotAmbiguousRepeatedAlignmentFreezesWholePage's whole-page fallback assertion.
func TestSnapshotAmbiguousRepeatedAlignmentStaysHeldBelowEight(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	old := []string{"A", "B", "A", "B", "A", "B"}
	next := []string{"A", "B", "A", "B", "C", "D"}
	m.MergeSnapshot("p", snapshotPage(old...), ctx)
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(next...), ctx), next...)
	if !slices.Equal(physicalHeldRows(t, m.states["p"]), old) {
		t.Fatal("unexplained old rows were not held")
	}
}

// Replaces TestSnapshotWeakScrollAnchorDoesNotBecomeStreamingRedraw's whole-page fallback assertion.
func TestSnapshotWeakScrollAnchorHoldsMissingRows(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	old := []string{"A", "B", "C", "", ""}
	next := []string{"C", "", "", "D", "E"}
	m.MergeSnapshot("p", snapshotPage(old...), ctx)
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(next...), ctx), next...)
	if !slices.Equal(physicalHeldRows(t, m.states["p"]), old[:2]) {
		t.Fatal("unexplained old rows were not held")
	}
}

func TestSnapshotLegacyMatchingDoesNotSearchFrozenPrefix(t *testing.T) {
	m := NewManager(t.TempDir())
	m.MergeSnapshot("p", snapshotPage("A", "B", "frozen suffix"), physicalContext(1, 40))
	ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, LeaseRevision: 2}
	m.MergeSnapshot("p", snapshotPage("current X", "current Y"), ctx)
	got := m.MergeSnapshot("p", snapshotPage("prefix", "A", "B", "new suffix"), ctx)
	assertSnapshotContent(t, got, "A", "B", "frozen suffix", "current X", "current Y", "prefix", "A", "B", "new suffix")
}

func TestSnapshotLegacyMatcherChangesSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	ctx := physicalContext(1, 40)
	ctx.ReadSource = SourceLegacy
	old := snapshotPage("A", "B", "old1", "old2", "old3", "old4")
	next := snapshotPage("A", "B", "new1", "new2", "new3")
	m.AdmitSnapshot("p", old, ctx, 0)
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	state := m.states["p"]
	if state.dirty {
		t.Fatal("initial save left state dirty")
	}
	version := state.mutationVersion
	content, _, admission := m.AdmitSnapshot("p", next, ctx, 0)
	if content != old || !admission.Admitted || admission.ContentChanged || admission.MetadataChanged || state.StaleRefusals != 1 {
		t.Fatalf("first divergent admission = %+v content=%q stale_refusals=%d", admission, content, state.StaleRefusals)
	}
	if !state.dirty || state.mutationVersion != version+1 {
		t.Errorf("matcher-only admission: dirty=%v mutationVersion=%d, want dirty=true mutationVersion=%d", state.dirty, state.mutationVersion, version+1)
	}
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(dir)
	content, _, admission = restarted.AdmitSnapshot("p", next, ctx, 0)
	if content != next || !admission.Admitted || !admission.ContentChanged || !admission.MetadataChanged {
		t.Fatalf("second divergent admission after restart = %+v content=%q, want %q", admission, content, next)
	}
	if err := restarted.SaveAll(); err != nil {
		t.Fatal(err)
	}
	state = restarted.states["p"]
	version = state.mutationVersion
	_, _, admission = restarted.AdmitSnapshot("p", next, ctx, 0)
	if !admission.Admitted || admission.ContentChanged || admission.MetadataChanged || state.dirty || state.mutationVersion != version {
		t.Fatalf("unchanged admission = %+v dirty=%v mutationVersion=%d, want clean version %d", admission, state.dirty, state.mutationVersion, version)
	}
}

func TestSnapshotBlankOnlyOverlapDoesNotRemoveRows(t *testing.T) {
	for _, old := range [][]string{{"", "", ""}, append([]string{"old A", "old B"}, make([]string, 35)...)} {
		m := NewManager(t.TempDir())
		ctx := physicalContext(1, 40)
		m.MergeSnapshot("p", snapshotPage(old...), ctx)
		assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(old...), ctx), old...)
		next := append(make([]string, 35), "new A", "new B")
		// Replaces blank-overlap fallback copies with held nonblank accounting.
		assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage(next...), ctx), next...)
		if physicalNonblank(old) > 0 && physicalNonblank(physicalHeldRows(t, m.states["p"]))+physicalDiagnostic(t, planPhysicalTransition(comparisonBody(old), comparisonBody(next), true).diagnostic, "ReplacedNonblank") != physicalNonblank(old) {
			t.Fatal("blank-only overlap removed nonblank old rows outside replacement")
		}
	}
}

func TestFooterOnlyChangeRefreshesLegacyAndSnapshotContent(t *testing.T) {
	for _, physical := range []bool{false, true} {
		m := NewManager(t.TempDir())
		merge := func(raw string) string {
			if physical {
				return m.MergeSnapshot("p", raw, physicalContext(1, 40))
			}
			return m.Merge("p", raw)
		}
		merge(snapshotPage("A", "B", "C"))
		newPage := strings.ReplaceAll(snapshotPage("A", "B", "C"), "footer", "new-footer")
		if got := merge(newPage); got != newPage {
			t.Fatalf("physical=%v footer content = %q", physical, got)
		}
		if !m.states["p"].dirty {
			t.Fatal("footer update did not mark state dirty")
		}
	}
}

func TestSnapshotAdmissionDistinguishesChangesAndUnchangedSuccess(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	raw := snapshotPage("A", "B", "C")
	_, _, first := m.AdmitSnapshot("p", raw, ctx, 0)
	if !first.Admitted || !first.ContentChanged || !first.MetadataChanged {
		t.Fatalf("first admission = %+v", first)
	}
	version := m.states["p"].mutationVersion
	_, _, unchanged := m.AdmitSnapshot("p", raw, ctx, 0)
	if !unchanged.Admitted || unchanged.ContentChanged || unchanged.MetadataChanged || m.states["p"].mutationVersion != version {
		t.Fatalf("unchanged admission = %+v", unchanged)
	}
	styled := snapshotPage("\x1b[44mA\x1b[0m", "B", "C")
	content, _, style := m.AdmitSnapshot("p", styled, ctx, 0)
	if !style.Admitted || !style.ContentChanged || style.MetadataChanged || content != styled || m.states["p"].LiveStart != 0 {
		t.Fatalf("style admission = %+v content=%q", style, content)
	}
	ctx.Rows++
	_, _, metadata := m.AdmitSnapshot("p", styled, ctx, 0)
	if !metadata.Admitted || metadata.ContentChanged || !metadata.MetadataChanged {
		t.Fatalf("metadata admission = %+v", metadata)
	}
	_, _, empty := m.AdmitSnapshot("p", "", ctx, 0)
	if empty.Admitted || empty.ContentChanged || empty.MetadataChanged {
		t.Fatalf("empty admission = %+v", empty)
	}
}

func TestSnapshotShortPageAndFooterSizes(t *testing.T) {
	for _, count := range []int{1, 6, 7, 10} {
		for _, footerRows := range []int{3, 6, 10} {
			m := NewManager(t.TempDir())
			lines := make([]string, count)
			for i := range lines {
				lines[i] = fmt.Sprintf("row-%d", i)
			}
			for i := range footerRows {
				lines = append(lines, fmt.Sprintf("prompt wrapped %d", i))
			}
			raw := strings.Join(lines, "\n")
			if got := m.MergeSnapshot("p", raw, physicalContext(1, 40)); got != raw {
				t.Fatalf("short page output = %q, want %q", got, raw)
			}
		}
	}
	for _, raw := range []string{"one", "1\n2\n3\n4\n5\n6", "1\n2\n3\n4\n5\n6\n7"} {
		m := NewManager(t.TempDir())
		if got := m.MergeSnapshot("p", raw, physicalContext(1, 40)); got != raw {
			t.Fatalf("raw short page = %q", got)
		}
	}
}

func TestSnapshotLimitPreservesNewestRawRowsAndReportsClipping(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage("old A", "old B"), ctx)
	raw := snapshotPage("new A", "new B")
	ctx.GeometryEpoch++
	got, clipped := m.MergeSnapshotLimited("p", raw, ctx, 10)
	if clipped || got != "old A\nold B\n"+raw {
		t.Fatalf("exact bound = %q clipped=%v", got, clipped)
	}
	got, clipped = m.MergeSnapshotLimited("p", raw, ctx, 2)
	if !clipped || got != "footer5\nfooter6" || len(m.states["p"].History) != 4 {
		t.Fatalf("short bound = %q clipped=%v state=%+v", got, clipped, m.states["p"])
	}
	large := make([]string, MaxLines-1)
	for i := range large {
		large[i] = fmt.Sprint(i)
	}
	m.MergeSnapshot("large", snapshotPage(large...), physicalContext(1, 120))
	m.MergeSnapshot("large", snapshotPage("A", "B", "C", "D"), physicalContext(2, 40))
	state := m.states["large"]
	if len(state.History) != MaxLines || state.SegmentStart != MaxLines-4 || state.LiveStart != MaxLines-4 {
		t.Fatalf("trimmed offsets = %+v", state)
	}
}

func TestSnapshotLegacyLoadRestartAndSourceBoundary(t *testing.T) {
	for _, metadata := range []string{
		``,
		`,"snapshot_version":1,"read_source":"recent","geometry_epoch":1,"lease_revision":1,"columns":40,"rows":82,"segment_start":0,"live_start":0`,
		`,"snapshot_version":1,"read_source":"recent","segment_start":-1,"live_start":200`,
		`,"snapshot_version":99,"read_source":"recent","segment_start":100,"live_start":-5`,
	} {
		m := NewManager(t.TempDir())
		data := `{"history":["A","B","old"],"footer":["stale-footer"],"stale_refusals":1,"last_hash":"old"` + metadata + `}`
		if err := os.WriteFile(m.stateFile("p"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		got := m.MergeSnapshot("p", snapshotPage("A", "B", "new"), physicalContext(1, 40))
		assertSnapshotContent(t, got, "A", "B", "old", "A", "B", "new")
		legacy := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, LeaseRevision: 2}
		got = m.MergeSnapshot("p", snapshotPage("A", "B", "legacy"), legacy)
		assertSnapshotContent(t, got, "A", "B", "old", "A", "B", "new", "A", "B", "legacy")
		ctx := physicalContext(1, 40)
		ctx.LeaseRevision = 3
		got = m.MergeSnapshot("p", snapshotPage("A", "B", "physical"), ctx)
		assertSnapshotContent(t, got, "A", "B", "old", "A", "B", "new", "A", "B", "legacy", "A", "B", "physical")
		encoded, err := json.Marshal(m.states["p"])
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"history", "footer", "stale_refusals", "last_hash", "snapshot_version", "terminal_id", "read_source", "geometry_epoch", "lease_revision", "columns", "rows", "segment_start", "live_start"} {
			if _, ok := wire[key]; !ok {
				t.Fatalf("missing persisted field %q: %s", key, encoded)
			}
		}
	}
}

func TestSnapshotFrozenPrefixTrimmingClampsOffsets(t *testing.T) {
	m := NewManager(t.TempDir())
	first := make([]string, MaxLines)
	second := make([]string, MaxLines+2)
	for i := range first {
		first[i] = fmt.Sprintf("first-%d", i)
	}
	for i := range second {
		second[i] = fmt.Sprintf("second-%d", i)
	}
	m.MergeSnapshot("p", snapshotPage(first...), physicalContext(1, 120))
	raw := snapshotPage(second...)
	if got := m.MergeSnapshot("p", raw, physicalContext(2, 40)); got != raw {
		t.Fatal("retention trimming changed the raw response page")
	}
	state := m.states["p"]
	if len(state.History) != MaxLines || state.SegmentStart != 0 || state.LiveStart != 0 || state.History[0] != "second-2" {
		t.Fatalf("trimmed state offsets: segment=%d live=%d rows=%d", state.SegmentStart, state.LiveStart, len(state.History))
	}
}

func TestFirstRuntimeAdmissionFencesIdenticalNumericEpoch(t *testing.T) {
	for _, source := range []string{SourceRecent, SourceLegacy} {
		dir := t.TempDir()
		first := NewManager(dir)
		ctx := physicalContext(1, 40)
		ctx.ReadSource = source
		ctx.NoScrollback = true
		first.MergeSnapshot("p", snapshotPage("A", "B", "old"), ctx)
		first.SaveAll()
		restarted := NewManager(dir)
		if got := restarted.Content("p", 0); got != snapshotPage("A", "B", "old") {
			t.Fatalf("restart content before admission = %q", got)
		}
		assertSnapshotContent(t, restarted.MergeSnapshot("p", snapshotPage("A", "B", "new"), ctx), "A", "B", "old", "A", "B", "new")
		assertSnapshotContent(t, restarted.MergeSnapshot("p", snapshotPage("A", "B", "new"), ctx), "A", "B", "old", "A", "B", "new")
	}
}

func TestSourceGateCycleStartsNewSegmentWithoutIntermediateRead(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage("A", "B", "old"), ctx)
	ctx.LeaseRevision += 2
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage("A", "B", "new"), ctx), "A", "B", "old", "A", "B", "new")
}

func TestUnleasedNoScrollbackExistingFixturesRemainByteIdentical(t *testing.T) {
	// Expected bytes were measured with Manager.Merge at bc4dd58.
	fixtures := []struct {
		frames []string
		want   []string
	}{
		{
			frames: []string{snapshotPage("A", "B", "C"), snapshotPage("B", "C", "D")},
			want:   []string{"A\nB\nC\nfooter1\nfooter2\nfooter3\nfooter4\nfooter5\nfooter6", "A\nB\nC\nD\nfooter1\nfooter2\nfooter3\nfooter4\nfooter5\nfooter6"},
		},
		{
			frames: []string{snapshotPage("H1", "H2", "A", "B", "C", "D", "E"), snapshotPage("H1", "H2", "C", "D", "E", "F", "G")},
			want:   []string{"H1\nH2\nA\nB\nC\nD\nE\nfooter1\nfooter2\nfooter3\nfooter4\nfooter5\nfooter6", "H1\nH2\nA\nB\nC\nD\nE\nF\nG\nfooter1\nfooter2\nfooter3\nfooter4\nfooter5\nfooter6"},
		},
		{
			frames: []string{snapshotPage("A", "B", "\x1b[42mstyle\x1b[0m", "old1", "old2"), snapshotPage("A", "B", "\x1b[44mstyle\x1b[0m", "new1", "new2")},
			want:   []string{"A\nB\n\x1b[42mstyle\x1b[0m\nold1\nold2\nfooter1\nfooter2\nfooter3\nfooter4\nfooter5\nfooter6", "A\nB\n\x1b[44mstyle\x1b[0m\nnew1\nnew2\nfooter1\nfooter2\nfooter3\nfooter4\nfooter5\nfooter6"},
		},
	}
	for _, fixture := range fixtures {
		legacy := NewManager(t.TempDir())
		segmented := NewManager(t.TempDir())
		for index, raw := range fixture.frames {
			for name, got := range map[string]string{
				"Merge":         legacy.Merge("p", raw),
				"MergeSnapshot": segmented.MergeSnapshot("p", raw, SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy}),
			} {
				if got != fixture.want[index] {
					t.Fatalf("%s parity: got %q, want %q", name, got, fixture.want[index])
				}
			}
		}
	}
	m := NewManager(t.TempDir())
	m.MergeSnapshot("p", snapshotPage("physical A", "physical B"), physicalContext(1, 40))
	ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, LeaseRevision: 2}
	m.MergeSnapshot("p", snapshotPage("H1", "H2", "A", "B", "C", "D", "E"), ctx)
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage("H1", "H2", "C", "D", "E", "F", "G"), ctx), "physical A", "physical B", "H1", "H2", "A", "B", "C", "D", "E", "F", "G")
}

func TestSnapshotTerminalReplacementResetsButSessionDiscoveryDoesNot(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage("old A", "old B"), ctx)
	ctx.GeometryEpoch++
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage("same terminal A", "same terminal B"), ctx), "old A", "old B", "same terminal A", "same terminal B")
	ctx.TerminalID = "terminal-new"
	assertSnapshotContent(t, m.MergeSnapshot("p", snapshotPage("new A", "new B"), ctx), "new A", "new B")
	m.SaveAll()
	restarted := NewManager(filepath.Dir(m.dir))
	ctx.TerminalID = "terminal-replaced-again"
	assertSnapshotContent(t, restarted.MergeSnapshot("p", snapshotPage("replacement A", "replacement B"), ctx), "replacement A", "replacement B")
}
