package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type physicalReplayPage struct {
	Index     int    `json:"index"`
	Epoch     uint64 `json:"epoch"`
	ElapsedMS int    `json:"elapsed_ms"`
	Rows      []int  `json:"rows"`
}

type physicalReplayFixture struct {
	Schema int                  `json:"schema"`
	Rows   []string             `json:"rows"`
	Pages  []physicalReplayPage `json:"pages"`
}

func loadPhysicalReplay(t *testing.T, name string) physicalReplayFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture physicalReplayFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Schema != 1 || len(fixture.Pages) == 0 || len(fixture.Rows) == 0 {
		t.Fatalf("invalid dictionary fixture %s", name)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Rows {
		if seen[row] {
			t.Fatalf("duplicate dictionary row in %s", name)
		}
		seen[row] = true
	}
	for i, page := range fixture.Pages {
		if page.Index != i || page.Epoch == 0 || len(page.Rows) == 0 {
			t.Fatalf("invalid page %d in %s", i, name)
		}
		for _, row := range page.Rows {
			if row < 0 || row >= len(fixture.Rows) {
				t.Fatalf("row index %d outside dictionary in %s", row, name)
			}
		}
	}
	return fixture
}

func (fixture physicalReplayFixture) raw(index int) string {
	rows := make([]string, len(fixture.Pages[index].Rows))
	for i, row := range fixture.Pages[index].Rows {
		rows[i] = fixture.Rows[row]
	}
	return strings.Join(rows, "\n")
}

func (fixture physicalReplayFixture) body(index int) []string {
	body, _ := splitSnapshot(fixture.raw(index))
	return body
}
func (fixture physicalReplayFixture) context(index int) SnapshotContext {
	columns := 54
	if len(fixture.Pages) == 577 {
		columns = 149
		if index >= 172 {
			columns = 195
		}
	}
	ctx := physicalContext(fixture.Pages[index].Epoch, columns)
	ctx.Rows = len(fixture.Pages[index].Rows)
	return ctx
}

func TestPhysicalFixtureParsing(t *testing.T) {
	for name, count := range map[string]int{
		"physical-pages-chunk17": 23, "physical-clear-gap-fill": 3,
		"physical-pages-desktop-shapes": 577, "physical-pages-markdown": 27,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := loadPhysicalReplay(t, name)
			if len(fixture.Pages) != count {
				t.Fatalf("pages=%d, want %d", len(fixture.Pages), count)
			}
			for i, page := range fixture.Pages {
				if len(strings.Split(fixture.raw(i), "\n")) != len(page.Rows) {
					t.Fatal("dictionary lost physical rows")
				}
				if i > 0 && page.ElapsedMS < fixture.Pages[i-1].ElapsedMS {
					t.Fatal("timestamps moved backward")
				}
			}
		})
	}
	physicalCases(t)
	repeatedBlockFixture(t)
}

func physicalPrivacyPatterns() []*regexp.Regexp {
	var patterns []*regexp.Regexp
	for _, pattern := range []string{
		`(?i)/(?:home|users|var|tmp|mnt|scratchpad)/`, `(?i)/[^\s/]+/(?:scratchpad|pane[^\s/]*)`,
		`(?i)pane-cwd|model-status|UserPromptSubmit`, `(?i)\b(?:billing|account|subscription)\b`,
		`(?i)\b(?:opus|haiku|sonnet|gpt|glm|deepseek|grok)(?:[- .\d]|\b)`,
		`(?i)\b(?:writer|reviewer|triage)--[a-z]`, `\bcf-[a-z]`,
		`\bw\d+:p\d+\b`, `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`,
	} {
		patterns = append(patterns, regexp.MustCompile(pattern))
	}
	return patterns
}

func TestPhysicalFixturesContainNoEnvironmentText(t *testing.T) {
	paths, err := filepath.Glob("testdata/physical-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 6 {
		t.Fatalf("privacy coverage found %d fixtures, want 6", len(paths))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		var scan func(any)
		scan = func(value any) {
			switch value := value.(type) {
			case string:
				for i, pattern := range physicalPrivacyPatterns() {
					if pattern.MatchString(NormalizeLine(value)) {
						t.Errorf("%s matches environment pattern %d", path, i)
					}
				}
			case []any:
				for _, child := range value {
					scan(child)
				}
			case map[string]any:
				for key, child := range value {
					scan(key)
					scan(child)
				}
			}
		}
		scan(value)
	}
}

func TestPhysicalPrivacyPatternsPositiveControl(t *testing.T) {
	for _, text := range []string{"/home/example/project", "/var/tmp/example", "pane-cwd", "model-status", "API Usage Billing", "account example", "Haiku 0.0", "cf-example", "reviewer--example", "w12:p34", "00000000-0000-0000-0000-000000000000"} {
		found := false
		for _, pattern := range physicalPrivacyPatterns() {
			found = found || pattern.MatchString(text)
		}
		if !found {
			t.Errorf("privacy positive control missed %q", text)
		}
	}
	for _, pattern := range physicalPrivacyPatterns() {
		if pattern.MatchString("❯ /clear") {
			t.Fatal("privacy scan rejects command echo")
		}
	}
}

func assertPhysicalRawState(t *testing.T, m *Manager, raw, got string) {
	t.Helper()
	state := m.states["p"]
	body, footer := splitSnapshot(raw)
	if !slices.Equal(state.History[state.LiveStart:], body) || !slices.Equal(state.Footer, footer) {
		t.Fatal("raw live body/footer changed")
	}
	want := strings.Join(append(slices.Clone(state.History[:state.LiveStart]), strings.Split(raw, "\n")...), "\n")
	if got != want {
		t.Fatal("response differs from frozen archive plus exact raw page")
	}
}

func physicalReplay(t *testing.T, fixture physicalReplayFixture, indices []int, inserts bool, allowedFreezes map[int]bool) (*Manager, string) {
	t.Helper()
	m := NewManager(t.TempDir())
	var got string
	previous := -1
	for _, index := range indices {
		state := m.states["p"]
		beforeLive, beforeRows := 0, 0
		if state != nil {
			beforeLive, beforeRows = state.LiveStart, len(state.History)
		}
		raw := fixture.raw(index)
		var admission SnapshotAdmission
		got, _, admission = m.AdmitSnapshot("p", raw, fixture.context(index), 0)
		if !admission.Admitted {
			t.Fatalf("page %d was not admitted", index)
		}
		assertPhysicalRawState(t, m, raw, got)
		state = m.states["p"]
		boundary := previous >= 0 && fixture.Pages[previous].Epoch != fixture.Pages[index].Epoch
		if boundary && (state.LiveStart != beforeRows || state.SegmentStart != beforeRows) {
			t.Fatalf("page %d did not fence geometry change", index)
		}
		if previous >= 0 && !boundary && state.LiveStart-beforeLive >= len(fixture.body(previous)) && !allowedFreezes[index] {
			t.Errorf("transition %d->%d copied old page: archive grew %d rows", previous, index, state.LiveStart-beforeLive)
		}
		if inserts {
			live, held, rows := state.LiveStart, len(physicalHeldRows(t, state)), len(state.History)
			for _, duplicate := range []string{raw, "\x1b[32m" + raw + "\x1b[0m", raw} {
				copyContent, _, _ := m.AdmitSnapshot("p", duplicate, fixture.context(index), 0)
				assertPhysicalRawState(t, m, duplicate, copyContent)
				if state.LiveStart != live || len(state.History) != rows || len(physicalHeldRows(t, state)) != held {
					t.Fatalf("page %d duplicate/ANSI-only frame grew archive or holds", index)
				}
			}
		}
		previous = index
	}
	return m, got
}

func physicalIndices(first, last, stride int) []int {
	var indices []int
	for i := first; i <= last; i += stride {
		indices = append(indices, i)
	}
	if indices[len(indices)-1] != last {
		indices = append(indices, last)
	}
	return indices
}

var physicalAnswerStart = regexp.MustCompile(`^\s*(?:●\s*)?(L\d{3}) `)

func physicalAnswerRows(rows []string) []string {
	var answers []string
	for _, row := range rows {
		text := physicalText(row)
		if physicalAnswerStart.MatchString(text) {
			answers = append(answers, strings.TrimPrefix(strings.TrimSpace(text), "● "))
		}
	}
	return answers
}

func assertPhysicalObservedAnswers(t *testing.T, fixture physicalReplayFixture, indices []int, stored []string) {
	t.Helper()
	// Final observed versions replace incomplete streaming prefixes in this oracle.
	observed := make(map[string]string)
	var order []string
	for _, index := range indices {
		for _, row := range physicalAnswerRows(fixture.body(index)) {
			marker := physicalAnswerStart.FindStringSubmatch(row)[1]
			if _, found := observed[marker]; !found {
				order = append(order, marker)
			}
			observed[marker] = row
		}
	}
	got := physicalAnswerRows(stored)
	var want []string
	for _, marker := range order {
		want = append(want, observed[marker])
	}
	if !slices.Equal(got, want) {
		t.Errorf("sampled answer rows differ: got %d rows, want %d once in observed order", len(got), len(want))
	}
}

func assertPhysicalAnswerContinuations(t *testing.T, fixture physicalReplayFixture, indices []int, stored []string) {
	t.Helper()
	observed := make(map[string]bool)
	for _, index := range indices {
		inAnswer := index == 0
		for _, row := range fixture.body(index) {
			text := physicalText(row)
			if physicalAnswerStart.MatchString(text) {
				inAnswer = true
			}
			if text == "" || strings.HasPrefix(text, "❯") {
				inAnswer = false
			}
			if inAnswer && strings.HasPrefix(text, "  ") && !physicalAnswerStart.MatchString(text) {
				observed[text] = true
			}
		}
	}
	for row := range observed {
		count := 0
		for _, value := range stored {
			if physicalText(value) == row {
				count++
			}
		}
		if count != 1 {
			t.Errorf("observed answer continuation occurs %d times, want once: %q", count, row)
		}
	}
}

func TestPhysicalChunk17Replay(t *testing.T) {
	fixture := loadPhysicalReplay(t, "physical-pages-chunk17")
	for _, stride := range []int{1, 2, 4, 7} {
		for _, inserts := range []bool{false, true} {
			t.Run(fmt.Sprintf("stride%d/inserts%v", stride, inserts), func(t *testing.T) {
				indices := physicalIndices(0, 22, stride)
				m, _ := physicalReplay(t, fixture, indices, inserts, nil)
				state := m.states["p"]
				assertPhysicalObservedAnswers(t, fixture, indices, state.History)
				if stride == 1 {
					assertPhysicalAnswerContinuations(t, fixture, indices, state.History)
				}
				planet := 0
				for _, row := range state.History {
					if strings.TrimSpace(physicalText(row)) == "planet." {
						planet++
					}
				}
				if planet != 1 {
					t.Errorf("planet. continuation occurs %d times, want once", planet)
				}
			})
		}
	}
}

func TestPhysicalChunk17IsolatedTransitions(t *testing.T) {
	fixture := loadPhysicalReplay(t, "physical-pages-chunk17")
	for _, test := range []struct {
		old, next     int
		retired, held []int
	}{
		{1, 2, []int{0, 1, 2, 3}, []int{72}}, {12, 13, []int{0, 1, 2, 3, 4}, nil},
		{13, 14, []int{1, 2, 3}, nil}, {15, 16, []int{0, 1, 2, 3, 4}, nil},
	} {
		t.Run(fmt.Sprintf("%d-%d", test.old, test.next), func(t *testing.T) {
			m, _ := physicalReplay(t, fixture, []int{test.old, test.next}, false, nil)
			state := m.states["p"]
			if !slices.Equal(state.History[:state.LiveStart], physicalRowsAt(fixture.body(test.old), test.retired)) {
				t.Fatalf("transition %d->%d retired wrong old-coordinate rows: live=%d want=%d", test.old, test.next, state.LiveStart, len(test.retired))
			}
			if !slices.Equal(physicalHeldRows(t, state), physicalRowsAt(fixture.body(test.old), test.held)) {
				t.Fatal("unexplained physical rows were not held")
			}
		})
	}
}

func TestPhysicalStoredClearAndGapFill(t *testing.T) {
	fixture := loadPhysicalReplay(t, "physical-clear-gap-fill")
	for name, indices := range map[string][]int{"clear": {0, 1, 2}, "gap-fill-only": {1, 2}} {
		t.Run(name, func(t *testing.T) {
			m, _ := physicalReplay(t, fixture, indices, false, map[int]bool{1: true})
			state := m.states["p"]
			want := []string(nil)
			if name == "clear" {
				want = fixture.body(0)
			}
			if !slices.Equal(state.History[:state.LiveStart], want) {
				t.Fatalf("clear/gap fill archive rows=%d want=%d; gap fill must not repeat the logo page", state.LiveStart, len(want))
			}
			if len(physicalHeldRows(t, state)) == 0 {
				t.Fatal("gap-fill residue was not stored separately")
			}
		})
	}
}

// Map steps.log wall times to the first page at/after that time. Desktop page 0
// is 06:42:14.795. The recorded zero-based windows are spinner 22->23
// (8.486/8.748s), thinking 260->261 (83.169/83.427s), clear 265->266
// (87.166/87.429s), prompt 344..433 (118.105..160.650s), grow 344..349,
// shrink 349..364, submit 364, queue 433..576, and second queued prompt 448
// (164.915s). Some driver steps fall between sampled changes, not on a page.
// Driver time mapping (first page >= step): chunk sends 1/83/145/211,
// clear sent 264, refill 268, grow start/append1 344, appends2..5
// 345/346/347/349, shrink start 349, shrink done/submit 364,
// queue first 433, queue second 448, and final marker checks 576.
// Record 172 (55.360s) injects the chief-classified geometry boundary, 87x149
// to 82x195. The capture has no PTY sample proving that emulator change.
// Markdown steps span 07:36:54.864..07:37:09.335; its 27 pages include the
// table-width redraws 20->21 and 21->22, not a hook-output collapse.
func TestPhysicalDesktopShapeWindows(t *testing.T) {
	fixture := loadPhysicalReplay(t, "physical-pages-desktop-shapes")
	for name, window := range map[string][2]int{"spinner-gap": {22, 23}, "prompt": {344, 433}, "queued": {433, 576}} {
		t.Run(name, func(t *testing.T) { physicalReplay(t, fixture, physicalIndices(window[0], window[1], 1), true, nil) })
	}
}

func TestPhysicalSpinnerInGapUsesChangedMiddleOnly(t *testing.T) {
	fixture := loadPhysicalReplay(t, "physical-pages-desktop-shapes")
	m := NewManager(t.TempDir())
	m.AdmitSnapshot("p", fixture.raw(22), fixture.context(22), 0)
	got, _, admission := m.AdmitSnapshot("p", fixture.raw(23), fixture.context(23), 0)
	assertPhysicalRawState(t, m, fixture.raw(23), got)
	if m.states["p"].LiveStart != 0 || len(physicalHeldRows(t, m.states["p"])) != 0 {
		t.Fatal("unchanged blank gap turned a one-row spinner update into archive/held growth")
	}
	if physicalDiagnostic(t, admission, "ReplacedRows") != 1 {
		t.Fatal("spinner update must replace one physical row")
	}
}

func TestPhysicalDesktopGeometryRewrapIsFence(t *testing.T) {
	fixture := loadPhysicalReplay(t, "physical-pages-desktop-shapes")
	m, _ := physicalReplay(t, fixture, []int{171, 172}, false, nil)
	state := m.states["p"]
	if state.SegmentStart != len(fixture.body(171)) || state.LiveStart != state.SegmentStart || len(physicalHeldRows(t, state)) != 0 {
		t.Fatal("geometry rewrap was not a separate segment fence")
	}
}

// Constructed clear: old N=72 includes 64 physical answer rows; H=70.
// The new logo has four nonblank physical rows, followed by /clear and chrome.
// Desktop clear: old N=59 includes 42 physical answer rows; H=58.
// Its new logo occupies three nonblank rows.
// These captured counts do not set the synthetic three-row-logo boundaries.
func TestPhysicalMeasuredThinkingCollapseAndLargeClears(t *testing.T) {
	for _, test := range []struct {
		name            string
		old, next, h, n int
		freeze          bool
	}{
		{"physical-pages-desktop-shapes", 260, 261, 10, 60, false},
		{"physical-pages-desktop-shapes", 265, 266, 58, 59, true},
		{"physical-clear-gap-fill", 0, 1, 70, 72, true},
	} {
		t.Run(fmt.Sprintf("%s/%d-%d", test.name, test.old, test.next), func(t *testing.T) {
			fixture := loadPhysicalReplay(t, test.name)
			m := NewManager(t.TempDir())
			m.AdmitSnapshot("p", fixture.raw(test.old), fixture.context(test.old), 0)
			got, _, admission := m.AdmitSnapshot("p", fixture.raw(test.next), fixture.context(test.next), 0)
			assertPhysicalRawState(t, m, fixture.raw(test.next), got)
			state := m.states["p"]
			if test.freeze {
				if !slices.Equal(state.History[:state.LiveStart], fixture.body(test.old)) || len(physicalHeldRows(t, state)) != 0 {
					t.Fatal("large clear must freeze old body once without duplicating its held residue")
				}
				physicalAssertReason(t, admission, "held_majority")
			} else if physicalNonblank(physicalHeldRows(t, state)) != test.h {
				t.Fatalf("thinking collapse held=%d, want %d (H/N=10/60)", physicalNonblank(physicalHeldRows(t, state)), test.h)
			}
			if n := physicalNonblank(fixture.body(test.old)); n != test.n {
				t.Fatalf("old nonblank N=%d want=%d", n, test.n)
			}
			if h := physicalDiagnostic(t, admission, "HeldNonblank", "HeldCandidateRows"); h != test.h {
				t.Fatalf("held numerator H=%d want=%d", h, test.h)
			}
		})
	}
}

func TestPhysicalDesktopReplay(t *testing.T) {
	fixture := loadPhysicalReplay(t, "physical-pages-desktop-shapes")
	indices := physicalIndices(0, 576, 1)
	m, _ := physicalReplay(t, fixture, indices, false, map[int]bool{266: true})
	// The epoch fence may repeat its one boundary page. Check answer sequence
	// within each geometry segment instead of treating that repetition as a copy.
	for _, window := range [][2]int{{0, 171}, {172, 576}} {
		indices := physicalIndices(window[0], window[1], 1)
		segment, _ := physicalReplay(t, fixture, indices, false, map[int]bool{266: true})
		assertPhysicalObservedAnswers(t, fixture, indices, segment.states["p"].History)
	}
	if m.states["p"].SegmentStart == 0 {
		t.Fatal("desktop replay lost the epoch boundary")
	}
}

func TestPhysicalMarkdownReplayAndTableRedraws(t *testing.T) {
	fixture := loadPhysicalReplay(t, "physical-pages-markdown")
	for name, indices := range map[string][]int{"complete": physicalIndices(0, 26, 1), "table20-21": {20, 21}, "table21-22": {21, 22}} {
		t.Run(name, func(t *testing.T) {
			m, _ := physicalReplay(t, fixture, indices, true, nil)
			if physicalNonblank(physicalHeldRows(t, m.states["p"])) == 0 {
				t.Fatal("earlier table layouts must remain held")
			}
			if name != "complete" {
				return
			}
			body := fixture.body(26)
			start := -1
			for i, row := range body {
				if strings.Contains(physicalText(row), "● Mountain Lakes") {
					start = i
					break
				}
			}
			if start < 0 {
				t.Fatal("markdown heading missing")
			}
			var want, got []string
			for _, row := range body[start:] {
				if physicalText(row) != "" {
					want = append(want, physicalText(row))
				}
			}
			for _, row := range m.states["p"].History {
				if slices.Contains(want, physicalText(row)) {
					got = append(got, physicalText(row))
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("final markdown answer layout/order differs: got=%d rows want=%d", len(got), len(want))
			}
		})
	}
}
