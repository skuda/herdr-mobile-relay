package history

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

type boundaryReplayPage struct {
	Index   int      `json:"index"`
	Columns int      `json:"columns"`
	Source  string   `json:"source"`
	Rows    []string `json:"rows"`
}

func boundaryReplayPages(t *testing.T) []boundaryReplayPage {
	t.Helper()
	data, err := os.ReadFile("testdata/physical-boundary-150-62.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Schema int                  `json:"schema"`
		Pages  []boundaryReplayPage `json:"pages"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Schema != 1 || len(fixture.Pages) != 11 {
		t.Fatal("missing paired boundary fixture")
	}
	return fixture.Pages
}
func boundaryReplayOracleCut(t *testing.T, old []string, floor int, incoming []string) (int, int) {
	t.Helper()
	var words []string
	for _, row := range incoming {
		words = append(words, strings.Fields(physicalText(row))...)
	}
	for cut := floor; cut < len(old); cut++ {
		if !physicalSubstantive(physicalText(old[cut])) {
			continue
		}
		var covered []string
		rows := 0
		for _, row := range old[cut:] {
			text := physicalText(row)
			covered = append(covered, strings.Fields(text)...)
			if physicalSubstantive(text) {
				rows++
			}
		}
		if len(covered) < 12 || rows < 2 {
			continue
		}
		starts := []int{}
		for start := 0; start+len(covered) <= len(words); start++ {
			if slices.Equal(words[start:start+len(covered)], covered) {
				starts = append(starts, start)
			}
		}
		if len(starts) != 1 {
			continue
		}
		seam := 0
		if cut > floor && starts[0] > 0 {
			previous := strings.Fields(physicalText(old[cut-1]))
			prefix := words[:starts[0]]
			if len(prefix) < len(previous) && slices.Equal(previous[len(previous)-len(prefix):], prefix) {
				seam = 1
			}
		}
		return cut, seam
	}
	t.Fatal("paired prose boundary has no independent whole-row coverage")
	return 0, 0
}

func TestBoundaryPairedCaptureRules(t *testing.T) {
	pages := boundaryReplayPages(t)
	m := NewManager(t.TempDir())
	rules := []string{
		"seed legacy page",
		"legacy overlap appends the new answer",
		"source boundary removes the whole-row suffix covered by the phone page",
		"equal-column continuation retires the displaced prefix",
		"source boundary removes the departing-segment suffix covered by the desktop page",
		"source boundary retains one partial desktop row before the covered suffix",
		"width boundary removes the whole-row suffix covered by the landscape page",
		"width boundary retains one partial landscape row before the covered suffix",
		"equal-column continuation retires displaced rows before the structural answer",
		"source boundary refuses structural split-token uncertainty and retains the copy",
		"legacy unchanged admission leaves the retained copy unchanged",
	}
	for i, page := range pages {
		t.Logf("page=%d rule=%s", i, rules[i])
		before := m.states["p"]
		var priorRows []string
		priorSegment := 0
		if before != nil {
			priorRows = slices.Clone(before.History)
			priorSegment = before.SegmentStart
		}
		// Coordinates follow whole-row suffix coverage; pages 5 and 7 retain one partial seam row.
		cuts := map[int]int{2: 64, 4: 65, 5: 104, 6: 104, 7: 130}
		refusal := i == 9
		if cut, ok := cuts[i]; ok {
			probeBody, _ := splitSnapshot(strings.Join(page.Rows, "\n"))
			oracleCut, oracleSeam := boundaryReplayOracleCut(t, priorRows, priorSegment, probeBody)
			if oracleCut != cut || oracleSeam != map[int]int{5: 1, 7: 1}[i] {
				t.Fatalf("page %d independent rule cut=%d seam=%d", i, oracleCut, oracleSeam)
			}
			covered := make([]string, 0)
			substantiveRows := 0
			for _, row := range priorRows[cut:] {
				text := physicalText(row)
				covered = append(covered, strings.Fields(text)...)
				if physicalSubstantive(text) {
					substantiveRows++
				}
			}
			incomingWords := make([]string, 0)
			for _, row := range probeBody {
				incomingWords = append(incomingWords, strings.Fields(physicalText(row))...)
			}
			occurrences := 0
			for start := 0; start+len(covered) <= len(incomingWords); start++ {
				if slices.Equal(incomingWords[start:start+len(covered)], covered) {
					occurrences++
				}
			}
			if cut < priorSegment || occurrences != 1 || len(covered) < 12 || substantiveRows < 2 {
				t.Fatalf("page %d lacks independent ordered whole-row coverage: words=%d rows=%d occurrences=%d", i, len(covered), substantiveRows, occurrences)
			}
			t.Logf("boundary=%d words=%d substantive_rows=%d", i, len(covered), substantiveRows)
		}

		ctx := physicalContext(uint64(i+1), page.Columns)
		ctx.ReadSource = page.Source
		if page.Source == SourceLegacy {
			ctx.Columns = 0
		}
		raw := strings.Join(page.Rows, "\n")
		got, _, d := m.AdmitSnapshot("p", raw, ctx, 0)
		state := m.states["p"]
		body, footer := splitSnapshot(raw)
		if !slices.Equal(state.Footer, footer) || !strings.HasSuffix(got, raw) {
			t.Fatalf("page %d lost exact raw page/footer", i)
		}
		if page.Source == SourceRecent && !slices.Equal(state.History[state.LiveStart:], body) {
			t.Fatalf("page %d changed live raw body", i)
		}
		markers := [5]int{}
		for _, row := range state.History {
			row = physicalComparisonRow(row)
			for marker := range 5 {
				needle := "P" + string(rune('1'+marker))
				if strings.Contains(row, needle) {
					markers[marker]++
				}
			}
		}
		wantLive := []int{81, 103, 64, 92, 146, 104, 104, 130, 187, 349, 349}[i]
		wantSegment := []int{0, 0, 64, 64, 65, 104, 104, 130, 130, 268, 268}[i]
		wantMarkers := [5]int{}
		if i >= 1 {
			wantMarkers = [5]int{2, 2, 2, 0, 0}
		}
		if i >= 3 {
			wantMarkers = [5]int{2, 2, 2, 2, 2}
		}
		if i >= 9 {
			wantMarkers = [5]int{4, 4, 4, 4, 4}
		}
		if state.LiveStart != wantLive || state.SegmentStart != wantSegment || markers != wantMarkers {
			t.Errorf("page %d coordinate/occurrence result live=%d segment=%d markers=%v, want %d/%d/%v", i, state.LiveStart, state.SegmentStart, markers, wantLive, wantSegment, wantMarkers)
		}
		if cut, ok := cuts[i]; ok {
			if d.BoundaryReconciledRows != len(priorRows)-cut || d.BoundaryRefused != 0 {
				t.Errorf("page %d failed proved suffix rule: %+v", i, d)
			}
		} else if refusal {
			if d.BoundaryRefused != 1 || d.BoundaryReconciledRows != 0 {
				t.Errorf("structural split-token boundary must refuse: %+v", d)
			}
		} else if d.BoundaryReconciledRows != 0 || d.BoundaryRefused != 0 {
			t.Errorf("continuation page %d attempted reconciliation", i)
		}
		t.Logf("transition=%d source=%s width=%d segment=%d live=%d rows=%d reconciled=%d refused=%d seam=%d markers=%v", i, page.Source, page.Columns, state.SegmentStart, state.LiveStart, len(state.History), d.BoundaryReconciledRows, d.BoundaryRefused, d.BoundarySeamKept, markers)
	}
}
