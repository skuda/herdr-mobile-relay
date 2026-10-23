package history

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
)

type physicalCase struct {
	Name     string   `json:"name"`
	Old      []string `json:"old"`
	Next     []string `json:"next"`
	Archive  []string `json:"archive"`
	Held     []int    `json:"held"`
	Matched  []int    `json:"matched"`
	Retired  []int    `json:"retired"`
	Replaced []int    `json:"replaced"`
	Seed     []string `json:"seed"`
	Epoch    uint64   `json:"epoch"`
	Class    string   `json:"cls"`
}

type physicalHeldView struct {
	Rows        []string  `json:"rows"`
	Position    int       `json:"position"`
	Segment     int       `json:"segment"`
	FirstSeenAt time.Time `json:"first_seen_at"`
}

func physicalCases(t *testing.T) []physicalCase {
	t.Helper()
	var fixture struct {
		Schema int            `json:"schema"`
		Cases  []physicalCase `json:"cases"`
	}
	data, err := os.ReadFile("testdata/physical-adversarial.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Schema != 1 || len(fixture.Cases) < 30 {
		t.Fatal("missing synthetic accounting cases")
	}
	return fixture.Cases
}

func physicalText(row string) string {
	return strings.TrimRight(strings.ReplaceAll(NormalizeLine(row), " ", " "), " \t\n")
}

func physicalNonblank(rows []string) int {
	count := 0
	for _, row := range rows {
		if physicalText(row) != "" {
			count++
		}
	}
	return count
}

func physicalHeld(t *testing.T, state *PaneState) []physicalHeldView {
	t.Helper()
	var records []physicalHeldView
	for _, record := range state.Held {
		records = append(records, physicalHeldView{Rows: slices.Clone(record.Rows), Position: record.Position, Segment: record.Segment, FirstSeenAt: record.FirstSeenAt})
	}
	for _, record := range records {
		if len(record.Rows) == 0 || record.FirstSeenAt.IsZero() || record.Segment < 0 || record.Segment > record.Position || record.Position > state.LiveStart {
			t.Fatalf("invalid held record: rows=%d segment=%d position=%d live=%d timestamp=%v", len(record.Rows), record.Segment, record.Position, state.LiveStart, record.FirstSeenAt)
		}
	}
	return records
}

func physicalHeldRows(t *testing.T, state *PaneState) []string {
	t.Helper()
	var rows []string
	for _, record := range physicalHeld(t, state) {
		rows = append(rows, record.Rows...)
	}
	return rows
}

// S9c/S9d add admission diagnostics; keep their field spelling isolated here
// while the matcher contract tests build before those slices.
func physicalDiagnostic(t *testing.T, admission SnapshotAdmission, names ...string) int {
	t.Helper()
	value := reflect.ValueOf(admission)
	for _, name := range names {
		field := value.FieldByName(name)
		if field.IsValid() && field.CanInt() {
			return int(field.Int())
		}
		if field.IsValid() && field.CanUint() {
			return int(field.Uint())
		}
	}
	t.Fatalf("A2 diagnostic unavailable: %s", strings.Join(names, "/"))
	return 0
}

func physicalAssertReason(t *testing.T, admission SnapshotAdmission, want string) {
	t.Helper()
	value := reflect.ValueOf(admission)
	for _, name := range []string{"Reason", "FreezeReason"} {
		field := value.FieldByName(name)
		if field.IsValid() && field.Kind() == reflect.String {
			if field.String() != want {
				t.Errorf("freeze reason=%q, want %q", field.String(), want)
			}
			return
		}
	}
	t.Fatal("A2 diagnostic unavailable: Reason/FreezeReason")
}

func physicalRowsAt(rows []string, indices []int) []string {
	var selected []string
	for _, i := range indices {
		selected = append(selected, rows[i])
	}
	return selected
}

func physicalAccountingError(rows []string, groups ...[]int) error {
	seen := make([]int, len(rows))
	for _, group := range groups {
		for _, i := range group {
			if i < 0 || i >= len(rows) {
				return fmt.Errorf("coordinate %d outside old body", i)
			}
			seen[i]++
		}
	}
	for i, row := range rows {
		if physicalText(row) != "" && seen[i] != 1 {
			return fmt.Errorf("old nonblank coordinate %d has %d outcomes", i, seen[i])
		}
		if seen[i] > 1 {
			return fmt.Errorf("old coordinate %d has duplicate outcomes", i)
		}
	}
	return nil
}

func TestPhysicalStateAccountingHelpers(t *testing.T) {
	for _, fixture := range physicalCases(t) {
		if err := physicalAccountingError(fixture.Old, fixture.Matched, fixture.Retired, fixture.Replaced, fixture.Held); err != nil {
			t.Errorf("%s: %v", fixture.Name, err)
		}
	}
	for name, groups := range map[string][][]int{
		"missing": {{0}}, "duplicate": {{0, 1}, {1}}, "invalid": {{0, 1, 2}},
	} {
		if err := physicalAccountingError([]string{"A", "B"}, groups...); err == nil {
			t.Errorf("%s ledger was accepted", name)
		}
	}
	if physicalNonblank([]string{" ", "{", "\x1b[31mA\x1b[0m\r", ""}) != 2 {
		t.Fatal("nonblank accounting dropped punctuation or counted NBSP padding")
	}
}

func runPhysicalCase(t *testing.T, fixture physicalCase) {
	t.Helper()
	m := NewManager(t.TempDir())
	ctx := physicalContext(fixture.Epoch, 54)
	ctx.Rows = len(fixture.Old) + FooterLines
	m.AdmitSnapshot("p", snapshotPage(fixture.Old...), ctx, 0)
	state := m.states["p"]
	if len(fixture.Seed) > 0 {
		state.History = append(slices.Clone(fixture.Seed), state.History...)
		state.LiveStart = len(fixture.Seed)
		if fixture.Epoch == 2 {
			state.SegmentStart = len(fixture.Seed)
		}
	}
	got, _, admission := m.AdmitSnapshot("p", snapshotPage(fixture.Next...), ctx, 0)
	if !admission.Admitted {
		t.Fatal("transition was not admitted")
	}
	archive := fixture.Archive
	if len(fixture.Seed) > 0 && fixture.Class != "reverse" && fixture.Epoch != 2 {
		archive = append(slices.Clone(fixture.Seed), archive...)
	}
	if !slices.Equal(state.History[:state.LiveStart], archive) {
		t.Fatalf("archive = %q (live=%d), want %q; no unsupported page copy or B C A retirement", state.History[:state.LiveStart], state.LiveStart, archive)
	}
	if !slices.Equal(state.History[state.LiveStart:], fixture.Next) {
		t.Fatal("stored live rows differ from raw body")
	}
	want := strings.Join(append(slices.Clone(archive), strings.Split(snapshotPage(fixture.Next...), "\n")...), "\n")
	if got != want {
		t.Fatal("response is not archive plus exact raw page")
	}
	wantHeld := physicalRowsAt(fixture.Old, fixture.Held)
	if fixture.Class == "freeze" {
		wantHeld = nil
	}
	if held := physicalHeldRows(t, state); !slices.Equal(held, wantHeld) {
		t.Fatalf("held raw rows = %q, want %q", held, wantHeld)
	}
	for _, check := range []struct {
		field string
		want  int
	}{
		{"MatchedNonblank", physicalNonblank(physicalRowsAt(fixture.Old, fixture.Matched))},
		{"RetiredNonblank", physicalNonblank(physicalRowsAt(fixture.Old, fixture.Retired))},
		{"ReplacedNonblank", physicalNonblank(physicalRowsAt(fixture.Old, fixture.Replaced))},
		{"HeldNonblank", physicalNonblank(physicalRowsAt(fixture.Old, fixture.Held))},
		{"OldNonblank", physicalNonblank(fixture.Old)},
	} {
		if fixture.Class == "freeze" && check.field != "HeldNonblank" && check.field != "OldNonblank" {
			continue
		}
		if got := physicalDiagnostic(t, admission, check.field); got != check.want {
			t.Errorf("%s=%d, want %d old nonblank coordinates", check.field, got, check.want)
		}
	}
	if fixture.Class == "freeze" {
		physicalAssertReason(t, admission, "held_majority")
	}
	if fixture.Class != "reverse" {
		beforeLive := state.LiveStart
		beforeHeld := slices.Clone(physicalHeldRows(t, state))
		m.AdmitSnapshot("p", snapshotPage(fixture.Next...), ctx, 0)
		if state.LiveStart != beforeLive || !slices.Equal(physicalHeldRows(t, state), beforeHeld) {
			t.Fatal("identical admission grew archive or held storage")
		}
	}
	if fixture.Class == "reverse" {
		if rows := physicalDiagnostic(t, admission, "RetractedRows"); rows != len(fixture.Seed) {
			t.Fatalf("retracted=%d, want %d", rows, len(fixture.Seed))
		}
		got, _, _ = m.AdmitSnapshot("p", snapshotPage(fixture.Old...), ctx, 0)
		if state.LiveStart != len(fixture.Seed) || !slices.Equal(state.History[:state.LiveStart], fixture.Seed) {
			t.Fatal("forward return did not retire A,B once")
		}
		if got != snapshotPage(append(slices.Clone(fixture.Seed), fixture.Old...)...) {
			t.Fatal("forward return changed raw composition")
		}
	}
}

func TestPhysicalAdversarialTransitions(t *testing.T) {
	for _, fixture := range physicalCases(t) {
		t.Run(fixture.Name, func(t *testing.T) { runPhysicalCase(t, fixture) })
	}
}

func TestPhysicalFastScrollWithHeldChromeDoesNotFreeze(t *testing.T) {
	for _, fixture := range physicalCases(t) {
		if fixture.Name == "fast-scroll-with-held-chrome" {
			runPhysicalCase(t, fixture)
			return
		}
	}
	t.Fatal("missing fast-scroll fixture")
}

func TestPhysicalMinimumHeightChromeContractionStaysHeld(t *testing.T) {
	for _, fixture := range physicalCases(t) {
		if fixture.Name == "minimum-height-chrome-contraction" {
			runPhysicalCase(t, fixture)
			return
		}
	}
	t.Fatal("missing minimum-height fixture")
}

func TestPhysicalShortBodyClearStaysHeld(t *testing.T) {
	// Three logo rows plus three transcript rows: N=6, below the N>=8 guard.
	for _, fixture := range physicalCases(t) {
		if fixture.Name == "clear-logo3-transcript3" {
			runPhysicalCase(t, fixture)
			return
		}
	}
	t.Fatal("missing six-nonblank-row clear fixture")
}

func TestPhysicalFlickerDoesNotCopyOrHoldSupportedSlot(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, body := range [][]string{{"h1", "h2", "x1", "x2", "t1", "t2"}, {"h1", "h2", "y1", "y2", "t1", "t2"}, {"h1", "h2", "x1", "x2", "t1", "t2"}} {
		got, _, _ := m.AdmitSnapshot("p", snapshotPage(body...), physicalContext(1, 54), 0)
		if m.states["p"].LiveStart != 0 || got != snapshotPage(body...) || len(physicalHeldRows(t, m.states["p"])) != 0 {
			t.Fatal("supported A->B->A slot created archive/held rows")
		}
	}
}

func TestPhysicalNBSPComparisonPreservesRawAndLegacyBytes(t *testing.T) {
	for _, pair := range [][2][]string{
		{{"A", "B", "❯ ", " "}, {"A", "B", "❯ ", " "}},
		{{"A", "B", "❯ draft", " "}, {"A", "B", "❯ draft", ""}},
	} {
		m := NewManager(t.TempDir())
		m.AdmitSnapshot("p", snapshotPage(pair[0]...), physicalContext(1, 54), 0)
		got, _, _ := m.AdmitSnapshot("p", snapshotPage(pair[1]...), physicalContext(1, 54), 0)
		if m.states["p"].LiveStart != 0 || got != snapshotPage(pair[1]...) || len(physicalHeldRows(t, m.states["p"])) != 0 {
			t.Fatal("NBSP twin grew archive or changed raw bytes")
		}
		if !slices.Equal(comparisonBody(pair[0]), comparisonBody(pair[1])) {
			t.Fatal("physical NBSP mapping must precede trailing trim")
		}
		if NormalizeLine("❯ ") != "❯ " || NormalizeLine(" ") != " " {
			t.Fatal("legacy normalization changed")
		}
		legacy, segmented := NewManager(t.TempDir()), NewManager(t.TempDir())
		for _, body := range pair {
			raw := snapshotPage(body...)
			want := legacy.Merge("p", raw)
			got := segmented.MergeSnapshot("p", raw, SnapshotContext{ReadSource: SourceLegacy})
			if got != want {
				t.Fatal("legacy route bytes changed")
			}
		}
	}
}

type physicalPair struct{ Old, Next int }

// Enumerate distinct optimal occurrence mappings, not edit paths. This small
// oracle deliberately uses exponential work and never runs on captured pages.
func physicalConsensusOracle(old, next []string) (agreed, feasible []physicalPair) {
	best := -1
	mappings := make(map[string][]physicalPair)
	var visit func(int, int, []physicalPair)
	visit = func(a, b int, pairs []physicalPair) {
		if len(pairs) > best {
			best = len(pairs)
			clear(mappings)
		}
		if len(pairs) == best {
			mappings[fmt.Sprint(pairs)] = slices.Clone(pairs)
		}
		for i := a; i < len(old); i++ {
			text := physicalText(old[i])
			if !strings.ContainsFunc(text, func(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }) {
				continue
			}
			for j := b; j < len(next); j++ {
				if text == physicalText(next[j]) {
					visit(i+1, j+1, append(slices.Clone(pairs), physicalPair{i, j}))
				}
			}
		}
	}
	visit(0, 0, nil)
	counts := make(map[physicalPair]int)
	for _, mapping := range mappings {
		for _, pair := range mapping {
			counts[pair]++
		}
	}
	for pair, count := range counts {
		feasible = append(feasible, pair)
		if count == len(mappings) {
			agreed = append(agreed, pair)
		}
	}
	less := func(a, b physicalPair) int {
		if a.Old != b.Old {
			return a.Old - b.Old
		}
		return a.Next - b.Next
	}
	slices.SortFunc(agreed, less)
	slices.SortFunc(feasible, less)
	return
}

func TestPhysicalConsensusOracle(t *testing.T) {
	for _, test := range []struct {
		old, next []string
		agreed    []physicalPair
		feasible  int
	}{
		{[]string{"A", "B", "A"}, []string{"A", "B"}, []physicalPair{{0, 0}, {1, 1}}, 2},
		{[]string{"A", "A"}, []string{"A"}, nil, 2},
		{[]string{"A", "", "{", "B"}, []string{"A", "{", "B"}, []physicalPair{{0, 0}, {3, 2}}, 2},
		{[]string{"A", "B"}, []string{"B", "A"}, nil, 2},
	} {
		agreed, feasible := physicalConsensusOracle(test.old, test.next)
		if !slices.Equal(agreed, test.agreed) || len(feasible) != test.feasible {
			t.Fatalf("oracle agreed=%v feasible=%v", agreed, feasible)
		}
	}
}
