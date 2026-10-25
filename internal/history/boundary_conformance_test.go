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
			view := boundaryView{rows: test.rows, firstRow: test.firstRow, contexts: boundaryRowContexts(test.rows)}
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
		view := boundaryView{rows: rows, contexts: boundaryRowContexts(rows)}
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

func TestBoundaryPromptBlockBounds(t *testing.T) {
	for _, test := range []struct {
		name                          string
		rows                          []string
		firstRow, before, first, last int
		found                         bool
	}{
		{"wrapped-with-layout", []string{"❯ explain the harbor", "and its keepers", "", "---"}, 7, 11, 7, 8, true},
		{"last-marker", []string{"❯ earlier request", "❯ later request", "wrapped continuation"}, 4, 7, 5, 6, true},
		{"blank-ends-group", []string{"❯ earlier request", "", "answer prose"}, 0, 3, 0, 0, true},
		{"status-separated-request", []string{"❯ earlier request", "", "status words", "", "answer prose"}, 0, 5, 0, 0, true},
		{"punctuation-continuation", []string{"❯ earlier request", "?", "", "status"}, 3, 7, 3, 4, true},
		{"clipped-continuation", []string{"wrapped continuation", "more words"}, 9, 11, 0, 0, false},
		{"before-view", []string{"❯ request"}, 5, 5, 0, 0, false},
		{"no-substantive", []string{"", "---"}, 0, 2, 0, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := boundaryView{rows: test.rows, firstRow: test.firstRow, contexts: boundaryRowContexts(test.rows)}
			first, last, found := boundaryPromptBlock(view, test.before)
			if first != test.first || last != test.last || found != test.found {
				t.Errorf("prompt block=%d..%d/%v, want %d..%d/%v", first, last, found, test.first, test.last, test.found)
			}
		})
	}
}

func TestBoundaryWrappedRequestRetainsNewOccurrence(t *testing.T) {
	incoming := append([]string{"❯ Please repeat the previous answer exactly", "with the same words and no introduction.", ""}, boundaryProse...)
	if result := matchBoundarySuffix(boundaryProse, 0, incoming); result.reason != boundaryIncomingPrompt {
		t.Errorf("wrapped request reason=%s cut=%d rows=%d", result.reason, result.cut, result.rows)
	}
	assertBoundaryCopyRetained(t, boundaryProse, incoming)
}

func TestBoundaryRewrappedPromptBlockAnchorsRerender(t *testing.T) {
	prompt := []string{"❯ Explain how patient harbor keepers maintain bright lamps", "through long winter storms and guide the ships safely."}
	old := append(append(slices.Clone(prompt), "", "---"), boundaryProse...)
	incoming := append([]string{strings.Join(prompt, " "), "", "------"}, boundaryProse...)
	result := matchBoundarySuffix(old, 0, incoming)
	if result.refused || result.cut != 4 || result.rows != 3 || result.seam != 0 {
		t.Errorf("rewrapped prompt did not anchor the suffix: %+v", result)
	}
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	ctx.Columns = 62
	got, _, d := m.AdmitSnapshot("p", snapshotPage(incoming...), ctx, 0)
	if d.BoundaryReconciledRows != 3 || d.BoundaryRefused != 0 || got != snapshotPage(append(slices.Clone(old[:4]), incoming...)...) {
		t.Errorf("rewrapped prompt boundary did not preserve prefix plus raw page: %+v", d)
	}
	for _, kind := range []string{"changed-prompt-word", "archive-without-prompt"} {
		t.Run(kind, func(t *testing.T) {
			archive, next := slices.Clone(old), slices.Clone(incoming)
			if kind == "changed-prompt-word" {
				next[0] = strings.Replace(next[0], "patient", "careful", 1)
			} else {
				archive[0] = strings.TrimPrefix(archive[0], "❯ ")
				next = append(append(slices.Clone(prompt), "", "------"), boundaryProse...)
			}
			if result := matchBoundarySuffix(archive, 0, next); result.reason != boundaryIncomingPrompt {
				t.Errorf("unanchored block reason=%s cut=%d rows=%d", result.reason, result.cut, result.rows)
			}
			assertBoundaryCopyRetained(t, archive, next)
		})
	}
}

func TestBoundaryIdenticalWrappedPromptLayoutKeepsOutcome(t *testing.T) {
	prompt := []string{"❯ Explain how patient harbor keepers maintain bright lamps", "through long winter storms and guide the ships safely.", "", "---"}
	rows := append(slices.Clone(prompt), boundaryProse...)
	result := matchBoundarySuffix(rows, 0, rows)
	if result.refused || result.cut != 0 || result.rows != len(rows) || result.seam != 0 {
		t.Errorf("identical wrapped prompt outcome changed: %+v", result)
	}
}

func TestBoundaryMatchStartingInsideIncomingPromptRefuses(t *testing.T) {
	for _, test := range []struct {
		name     string
		incoming []string
	}{
		{"prompt-row", append([]string{"❯ Repeat this answer: " + boundaryProse[0]}, boundaryProse[1:]...)},
		{"prompt-row-then-blank", append([]string{"❯ Repeat this answer: " + boundaryProse[0], ""}, boundaryProse[1:]...)},
		{"continuation-row", append([]string{"❯ Repeat this answer:", "with the existing words and no introduction"}, boundaryProse...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := matchBoundarySuffix(boundaryProse, 0, test.incoming)
			if !result.refused || result.reason != boundaryIncomingPrompt {
				t.Errorf("prompt echo result=%+v, want incoming-prompt", result)
			}
			assertBoundaryCopyRetained(t, boundaryProse, test.incoming)
		})
	}
}

func TestBoundaryPromptContinuationCannotReuseEarlierAnchor(t *testing.T) {
	prompt := "❯ Repeat this answer:"
	old := append([]string{prompt, "", "---"}, boundaryProse...)
	incoming := append([]string{prompt}, boundaryProse...)
	result := matchBoundarySuffix(old, 0, incoming)
	if !result.refused || result.reason != boundaryIncomingPrompt {
		t.Errorf("matched prompt continuation reused the earlier anchor: %+v", result)
	}
	assertBoundaryCopyRetained(t, old, incoming)
}

func TestBoundaryMatchStartingAtPromptMarkerKeepsEarlierProse(t *testing.T) {
	incoming := append([]string{"❯ Explain how patient harbor keepers maintain bright lamps", "through long winter storms and guide the ships safely.", ""}, boundaryProse...)
	old := append([]string{"earlier unrelated answer prose", ""}, incoming...)
	result := matchBoundarySuffix(old, 0, incoming)
	if result.refused || result.cut != 2 || result.rows != len(incoming) || result.seam != 0 {
		t.Errorf("match beginning at the prompt marker changed: %+v", result)
	}
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	ctx.Columns = 62
	got, _, d := m.AdmitSnapshot("p", snapshotPage(incoming...), ctx, 0)
	if d.BoundaryRefused != 0 || d.BoundaryReconciledRows != len(incoming) || got != snapshotPage(old...) {
		t.Errorf("prompt-marker match changed earlier prose or raw output: %+v", d)
	}
}

func TestBoundaryMatchedPromptMarkersMustAlign(t *testing.T) {
	inline := append([]string{"Context ❯ " + boundaryProse[0]}, boundaryProse[1:]...)
	rowStart := append([]string{"Context", "❯ " + boundaryProse[0]}, boundaryProse[1:]...)
	for _, test := range []struct {
		name          string
		old, incoming []string
		reason        boundaryRefusalReason
	}{
		{"inline-to-row-start", inline, rowStart, boundaryIncomingPrompt},
		// The archive marker row starts a prompt block, so the proved suffix has one prose row.
		{"row-start-to-inline", rowStart, inline, boundaryPromptContext},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := matchBoundarySuffix(test.old, 0, test.incoming)
			if !result.refused || result.reason != test.reason {
				t.Errorf("unaligned marker result=%+v, want %s", result, test.reason)
			}
			assertBoundaryCopyRetained(t, test.old, test.incoming)
		})
	}
}

func TestBoundaryWholeRewrappedPromptBlockAndAnswerMatch(t *testing.T) {
	prompt := []string{"❯ Explain how patient harbor keepers maintain bright lamps", "through long winter storms and guide the ships safely."}
	old := append(append(slices.Clone(prompt), "", "---"), boundaryProse...)
	incoming := append([]string{strings.Join(prompt, " "), "", "---"}, boundaryProse...)
	result := matchBoundarySuffix(old, 0, incoming)
	if result.refused || result.cut != 0 || result.rows != len(old) || result.seam != 0 {
		t.Errorf("whole rewrapped prompt and answer changed: %+v", result)
	}
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	ctx.Columns = 62
	got, _, d := m.AdmitSnapshot("p", snapshotPage(incoming...), ctx, 0)
	if d.BoundaryRefused != 0 || d.BoundaryReconciledRows != len(old) || got != snapshotPage(incoming...) {
		t.Errorf("whole prompt rewrap did not preserve exact incoming raw page: %+v", d)
	}
}

func TestBoundaryRowContextsTrackBlankAndPrompt(t *testing.T) {
	rows := []string{"leading unknown", "---", "", "known prose", "  ❯ request", "wrapped continuation", "?", "", "more prose", "❯ next request"}
	want := []boundaryContext{boundaryContextUnknown, boundaryContextUnknown, boundaryContextProse, boundaryContextProse, boundaryContextPrompt, boundaryContextPrompt, boundaryContextPrompt, boundaryContextProse, boundaryContextProse, boundaryContextPrompt}
	if got := boundaryRowContexts(rows); !slices.Equal(got, want) {
		t.Errorf("row contexts=%v, want %v", got, want)
	}
	work := boundaryWork{}
	incoming, ok := boundaryIncoming(rows, &work)
	if !ok || !slices.Equal(incoming.contexts, want) {
		t.Fatalf("incoming context view=%v ok=%v", incoming.contexts, ok)
	}
	work = boundaryWork{}
	archive, ok := boundaryArchive(append([]string{"earlier segment"}, rows...), 1, 10000, &work)
	if !ok || archive.firstRow != 1 || !slices.Equal(archive.contexts, want) {
		t.Errorf("archive context view first=%d contexts=%v ok=%v", archive.firstRow, archive.contexts, ok)
	}
}

func TestBoundaryMatchedKnownPromptContextsMustAgree(t *testing.T) {
	prompt := "❯ Describe the coastal lights."
	old := append([]string{prompt, ""}, boundaryProse...)
	incoming := append([]string{prompt}, boundaryProse...)
	incoming = append(incoming, "", "A different response starts now.")
	result := matchBoundarySuffix(old, 0, incoming)
	if !result.refused || result.reason != boundaryIncomingPrompt {
		t.Errorf("known answer-to-prompt match result=%+v, want incoming-prompt", result)
	}
	assertBoundaryCopyRetained(t, old, incoming)
}

func TestBoundaryIncomingUnknownPromptContinuationCanMatch(t *testing.T) {
	old := append([]string{"❯ Explain how patient harbor keepers maintain bright lamps", "through long winter storms and guide the ships safely.", ""}, boundaryProse...)
	incoming := slices.Clone(old[1:])
	// The incoming continuation has UNKNOWN context, so the whole-row match keeps archive row 0 and cuts at 1.
	result := matchBoundarySuffix(old, 0, incoming)
	if result.refused || result.cut != 1 || result.rows != len(old)-1 || result.seam != 0 {
		t.Errorf("unknown leading continuation changed: %+v", result)
	}
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	ctx.Columns = 62
	got, _, d := m.AdmitSnapshot("p", snapshotPage(incoming...), ctx, 0)
	if d.BoundaryRefused != 0 || d.BoundaryReconciledRows != len(old)-1 || m.states["p"].LiveStart != 1 || got != snapshotPage(old...) {
		t.Errorf("unknown continuation lost the prompt row or raw page: %+v", d)
	}
}

func TestBoundaryMatchedBlankGapsMustAgree(t *testing.T) {
	prompt := []string{"❯ Explain how patient harbor keepers maintain bright lamps", "through long winter storms and guide the ships safely."}
	old := append(append(slices.Clone(prompt), ""), boundaryProse...)
	incoming := append([]string{prompt[1]}, boundaryProse...)
	incoming = append(incoming, "", "A different response starts now.")
	for _, test := range []struct {
		name          string
		old, incoming []string
	}{
		{"removed-prompt-answer-blank", old, incoming},
		{"added-answer-blank", boundaryProse, []string{boundaryProse[0], "", boundaryProse[1], boundaryProse[2]}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := matchBoundarySuffix(test.old, 0, test.incoming)
			if !result.refused || result.reason != boundaryUnsafeStructure {
				t.Errorf("blank-gap mismatch result=%+v, want unsafe-structure", result)
			}
			assertBoundaryCopyRetained(t, test.old, test.incoming)
		})
	}
}

func TestBoundaryBlankGapUsesViewOffset(t *testing.T) {
	view := boundaryView{firstRow: 7, rows: []string{"first", "---", "", "last"}}
	for _, test := range []struct {
		previous, current int
		want              bool
	}{
		{7, 7, false}, {7, 8, false}, {7, 10, true}, {9, 10, false},
	} {
		if got := boundaryBlankGap(view, test.previous, test.current); got != test.want {
			t.Errorf("gap %d..%d=%v, want %v", test.previous, test.current, got, test.want)
		}
	}
}

func TestBoundaryPairedBlankGapLayout(t *testing.T) {
	m := NewManager(t.TempDir())
	for i, page := range boundaryReplayPages(t) {
		body, _ := splitSnapshot(strings.Join(page.Rows, "\n"))
		if i == 2 || i == 4 || i == 5 || i == 6 || i == 7 {
			state := m.states["p"]
			result := matchBoundarySuffix(state.History, state.SegmentStart, body)
			if result.refused {
				work := boundaryWork{}
				next, ok := boundaryIncoming(body, &work)
				if !ok {
					t.Fatal("diagnostic incoming view exceeded budget")
				}
				limit := 0
				for _, row := range next.rows {
					limit += len(row)
				}
				old, ok := boundaryArchive(state.History, state.SegmentStart, limit, &work)
				if !ok {
					t.Fatal("diagnostic archive view exceeded budget")
				}
				for end := range next.tokens {
					n := 0
					for n < len(old.tokens) && n <= end && old.tokens[len(old.tokens)-1-n].text == next.tokens[end-n].text {
						n++
					}
					if n < MinBoundaryWords {
						continue
					}
					start, newStart := len(old.tokens)-n, end-n+1
					promptView := old
					archiveFirst, archiveLast, archiveFound, _ := boundaryArchivePrompt(state.History, &promptView, old.tokens[start].row, &work)
					incomingFirst, incomingLast, incomingFound := boundaryPromptBlock(next, next.tokens[newStart].row)
					t.Logf("transition=%d..%d reason=%s archive_prompt=%d..%d/%v incoming_prompt=%d..%d/%v", i-1, i, result.reason, archiveFirst, archiveLast, archiveFound, incomingFirst, incomingLast, incomingFound)
					for k := 1; k < n; k++ {
						a0, a1 := old.tokens[start+k-1].row, old.tokens[start+k].row
						i0, i1 := next.tokens[newStart+k-1].row, next.tokens[newStart+k].row
						archiveBlanks, incomingBlanks := 0, 0
						for row := a0 + 1; row < a1; row++ {
							if old.rows[row-old.firstRow] == "" {
								archiveBlanks++
							}
						}
						for row := i0 + 1; row < i1; row++ {
							if next.rows[row-next.firstRow] == "" {
								incomingBlanks++
							}
						}
						if (archiveBlanks > 0) != (incomingBlanks > 0) {
							t.Logf("transition=%d..%d token_offset=%d archive_rows=%d..%d archive_blanks=%d incoming_rows=%d..%d incoming_blanks=%d", i-1, i, k, a0, a1, archiveBlanks, i0, i1, incomingBlanks)
						}
					}
				}
				t.Fatalf("paired transition=%d..%d refused=%s", i-1, i, result.reason)
			}
		}
		ctx := physicalContext(uint64(i+1), page.Columns)
		ctx.ReadSource = page.Source
		if page.Source == SourceLegacy {
			ctx.Columns = 0
		}
		m.AdmitSnapshot("p", strings.Join(page.Rows, "\n"), ctx, 0)
	}
}

func TestBoundaryResidualUnrelatedPromptPrefixRefuses(t *testing.T) {
	// Accepted refusal residual (class 1): an unrelated preceding prompt keeps the copy.
	prompt := "❯ Describe the coastal lights."
	old := append([]string{prompt, "", "---"}, boundaryProse...)
	incoming := append([]string{"❯ An earlier unrelated request.", "", prompt, "", "---"}, boundaryProse...)
	result := matchBoundarySuffix(old, 0, incoming)
	if !result.refused || result.reason != boundaryIncomingPrompt {
		t.Errorf("unrelated-prefix refusal changed: %+v", result)
	}
	assertBoundaryCopyRetained(t, old, incoming)
}

func TestBoundaryPunctuationPromptContinuationAnchors(t *testing.T) {
	prompt := "❯ Explain why patient harbor keepers maintain bright lamps now"
	old := append([]string{prompt + " ?", "", "---"}, boundaryProse...)
	incoming := append([]string{prompt, "?", "", "------"}, boundaryProse...)
	result := matchBoundarySuffix(old, 0, incoming)
	if result.refused || result.cut != 3 || result.rows != 3 || result.seam != 0 {
		t.Errorf("equal prompt punctuation tokens did not anchor the answer suffix: %+v", result)
	}
}

func TestBoundaryControllingRequestAcrossStatusGroups(t *testing.T) {
	prompt := "❯ Explain the coastal lights."
	old := append([]string{prompt, ""}, boundaryProse...)
	incoming := append([]string{"❯ Please give the same harbor answer again, word for word.", "", "Thought for 1s", ""}, boundaryProse...)
	result := matchBoundarySuffix(old, 0, incoming)
	if !result.refused || result.reason != boundaryIncomingPrompt {
		t.Errorf("new controlling request result=%+v, want incoming-prompt", result)
	}
	assertBoundaryCopyRetained(t, old, incoming)
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	ctx.Columns = 62
	m.AdmitSnapshot("p", snapshotPage(incoming...), ctx, 0)
	for _, answerRow := range boundaryProse {
		occurrences := 0
		for _, row := range m.states["p"].History {
			if row == answerRow {
				occurrences++
			}
		}
		if occurrences != 2 {
			t.Errorf("requested-repeat answer row occurrences=%d, want 2", occurrences)
		}
	}
}

func TestBoundarySameControllingRequestKeepsSuffixProof(t *testing.T) {
	prompt := "❯ Explain the coastal lights."
	for _, test := range []struct {
		name                      string
		oldPrefix, incomingPrefix []string
		cut                       int
	}{
		{"different-status", []string{prompt, "", "Thought for 2s", ""}, []string{prompt, "", "Thought for 1s", ""}, 4},
		{"changed-paragraph", []string{prompt, "", "Thought for 2s", "", "Earlier explanation differs.", ""}, []string{prompt, "", "Thought for 1s", "", "Revised discussion changes.", ""}, 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			old := append(slices.Clone(test.oldPrefix), boundaryProse...)
			incoming := append(slices.Clone(test.incomingPrefix), boundaryProse...)
			result := matchBoundarySuffix(old, 0, incoming)
			if result.refused || result.cut != test.cut || result.rows != 3 || result.seam != 0 {
				t.Errorf("same controlling request did not prove the B suffix: %+v", result)
			}
		})
	}
}

func TestBoundaryControllingRequestBeforeArchiveViewAnchors(t *testing.T) {
	prompt := "❯ Explain the coastal lights."
	old := []string{prompt, ""}
	old = append(old, slices.Repeat([]string{"Earlier distinct context stays before the current answer.", ""}, 30)...)
	old = append(old, boundaryProse...)
	incoming := append([]string{prompt, "", "Thought for 1s", ""}, boundaryProse...)
	work := boundaryWork{}
	next, ok := boundaryIncoming(incoming, &work)
	if !ok {
		t.Fatal("synthetic incoming view exceeded budget")
	}
	limit := 0
	for _, row := range next.rows {
		limit += len(row)
	}
	view, ok := boundaryArchive(old, 0, limit, &work)
	if !ok || view.firstRow <= 1 {
		t.Fatalf("archive view did not exclude the request: first=%d ok=%v", view.firstRow, ok)
	}
	result := matchBoundarySuffix(old, 0, incoming)
	if result.refused || result.cut != 62 || result.rows != 3 || result.seam != 0 {
		t.Errorf("stored request before the view did not anchor the suffix: %+v", result)
	}
	result = matchBoundarySuffix(old, 62, incoming)
	if result.refused || result.cut != 62 || result.rows != 3 {
		t.Errorf("request before the segment floor did not anchor the segment suffix: %+v", result)
	}
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 150)
	m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
	m.states["p"].SegmentStart = 62
	ctx.Columns = 62
	_, _, d := m.AdmitSnapshot("p", snapshotPage(incoming...), ctx, 0)
	state := m.states["p"]
	if d.BoundaryRefused != 0 || d.BoundaryReconciledRows != 3 || state.LiveStart != 62 || !slices.Equal(state.History[:62], old[:62]) {
		t.Errorf("request search changed the prefix before the departing segment: %+v", d)
	}
}

func TestBoundaryArchiveRequestSearchChargesAndReusesRows(t *testing.T) {
	prompt := "❯ Explain the coastal lights."
	old := append([]string{strings.Repeat(" ", MaxBoundaryRawBytes+1), prompt, "?", "", "status words", ""}, boundaryProse...)
	work := boundaryWork{}
	view, ok := boundaryArchive(old, 6, 188, &work)
	if !ok || view.firstRow != 6 {
		t.Fatalf("synthetic view first=%d ok=%v", view.firstRow, ok)
	}
	before := work
	first, last, found, ok := boundaryArchivePrompt(old, &view, 6, &work)
	if !ok || !found || first != 1 || last != 2 || view.firstRow != 1 {
		t.Fatalf("earlier request block=%d..%d found=%v ok=%v view=%d", first, last, found, ok, view.firstRow)
	}
	if work.normalizedRows != before.normalizedRows+5 || work.rawBytes <= before.rawBytes || work.tokens <= before.tokens || work.normalizedBytes <= before.normalizedBytes {
		t.Errorf("earlier stored rows were not charged: before=%+v after=%+v", before, work)
	}
	if tokens := boundaryBlockTokens(view, first, last); len(tokens) != len(strings.Fields(prompt))+1 || tokens[0].row != 1 || tokens[len(tokens)-1].row != 2 {
		t.Errorf("request token coordinates changed: count=%d", len(tokens))
	}
	charged := work
	first, last, found, ok = boundaryArchivePrompt(old, &view, 6, &work)
	if !ok || !found || first != 1 || last != 2 || work != charged {
		t.Errorf("cached request rescanned raw rows: before=%+v after=%+v", charged, work)
	}
}

func TestBoundaryArchiveRequestSearchExhaustionRefuses(t *testing.T) {
	prompt := "❯ Explain the coastal lights."
	incoming := append([]string{prompt, "", "Thought for 1s", ""}, boundaryProse...)
	for _, test := range []struct {
		name, earlier string
	}{
		{"raw", strings.Repeat(" ", MaxBoundaryRawBytes+1)},
		{"normalized", strings.Repeat("x", MaxBoundaryNormalizedBytes)},
		{"tokens", strings.Repeat("x ", MaxBoundaryTokens)},
	} {
		t.Run(test.name, func(t *testing.T) {
			old := append([]string{prompt, test.earlier, ""}, boundaryProse...)
			result := matchBoundarySuffix(old, 3, incoming)
			if !result.refused || result.reason != boundaryBudget || result.rows != 0 || result.work.rawBytes > MaxBoundaryRawBytes || result.work.normalizedBytes > MaxBoundaryNormalizedBytes || result.work.tokens > MaxBoundaryTokens {
				t.Errorf("earlier request search escaped a work limit: %+v", result)
			}
			m := NewManager(t.TempDir())
			ctx := physicalContext(1, 150)
			m.AdmitSnapshot("p", snapshotPage(old...), ctx, 0)
			m.states["p"].SegmentStart = 3
			ctx.Columns = 62
			_, _, d := m.AdmitSnapshot("p", snapshotPage(incoming...), ctx, 0)
			state := m.states["p"]
			if d.BoundaryRefused != 1 || d.BoundaryReconciledRows != 0 || state.LiveStart != len(old) || !slices.Equal(state.History[:state.LiveStart], old) {
				t.Errorf("request-search exhaustion changed archived rows: %+v", d)
			}
		})
	}
}

func TestBoundaryResidualUnclosedPromptCanReconcile(t *testing.T) {
	// Accepted residual: answer text without a blank after a prompt counts as prompt text.
	old := append([]string{"❯ Original request"}, boundaryProse[1:]...)
	old = append(append(old, ""), boundaryProse...)
	result := matchBoundarySuffix(old, 0, boundaryProse)
	if result.refused || result.cut != 4 || result.rows != 3 || result.seam != 0 {
		t.Errorf("unclosed-prompt residual outcome changed: %+v", result)
	}
}

func TestBoundaryResidualRequestOutsideIncomingPageCanReconcile(t *testing.T) {
	// Accepted residual: a repeat with no request on the incoming page can match a re-render.
	result := matchBoundarySuffix(boundaryProse, 0, boundaryProse)
	if result.refused || result.cut != 0 || result.rows != 3 || result.seam != 0 {
		t.Errorf("promptless residual outcome changed: %+v", result)
	}
}

func TestBoundaryResidualIdenticalRepeatedRequestCanReconcile(t *testing.T) {
	// Accepted residual: an identical request and answer below the floor match a re-render.
	request := []string{"❯ Please give the same harbor answer again, word for word.", ""}
	rows := append(slices.Clone(request), boundaryProse...)
	result := matchBoundarySuffix(rows, len(request), slices.Clone(rows))
	if result.refused || result.cut != len(request) || result.rows != 3 || result.seam != 0 {
		t.Errorf("identical repeated request residual outcome changed: %+v", result)
	}
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
