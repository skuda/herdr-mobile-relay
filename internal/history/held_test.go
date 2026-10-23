package history

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// RED: A held-only mutation is durable metadata; its preview and queued save
// own independent nested rows; held churn cannot evict transcript.
func heldWireState(t *testing.T, history []string, live int, held any) *PaneState {
	t.Helper()
	wire := map[string]any{
		"history": history, "footer": []string{"footer1", "footer2", "footer3", "footer4", "footer5", "footer6"},
		"snapshot_version": 1, "terminal_id": "terminal-1", "read_source": SourceRecent,
		"geometry_epoch": 1, "lease_revision": 1, "columns": 54, "rows": 82,
		"segment_start": 0, "live_start": live,
	}
	if held != nil {
		wire["held"] = held
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	state := &PaneState{}
	if err := json.Unmarshal(data, state); err != nil {
		t.Fatal(err)
	}
	return state
}

func heldRecordWire(rows []string, position, segment int) map[string]any {
	return map[string]any{"rows": rows, "position": position, "segment": segment, "first_seen_at": "2026-01-02T03:04:05.000000006Z"}
}

func TestHeldOptionalValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		held    any
		invalid bool
	}{
		{"absent", nil, false}, {"empty", []any{}, false},
		{"valid", []any{heldRecordWire([]string{"raw\x1b[31m row\x1b[0m", "", "last\r"}, 2, 1)}, false},
		{"negative-position", []any{heldRecordWire([]string{"held"}, -1, 0)}, true},
		{"negative-segment", []any{heldRecordWire([]string{"held"}, 1, -1)}, true},
		{"segment-after-position", []any{heldRecordWire([]string{"held"}, 1, 2)}, true},
		{"position-after-live", []any{heldRecordWire([]string{"held"}, 3, 0)}, true},
		{"empty-rows", []any{heldRecordWire([]string{}, 1, 0)}, true},
		{"blank-only-surviving-suffix", []any{heldRecordWire([]string{"", " "}, 1, 0)}, false},
		{"missing-timestamp", []any{map[string]any{"rows": []string{"held"}, "position": 1, "segment": 0}}, true},
		{"zero-timestamp", []any{map[string]any{"rows": []string{"held"}, "position": 1, "segment": 0, "first_seen_at": "0001-01-01T00:00:00Z"}}, true},
		{"overflow", []any{heldRecordWire(slices.Repeat([]string{"held"}, 2001), 1, 0)}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := heldWireState(t, []string{"archive1", "archive2", "A", "B"}, 2, test.held)
			validateSnapshotState(state)
			if state.snapshotInvalid != test.invalid {
				t.Fatalf("invalid=%v, want %v", state.snapshotInvalid, test.invalid)
			}
			if test.invalid {
				if state.SnapshotVersion != 0 || len(physicalHeld(t, state)) != 0 {
					t.Fatal("invalid held metadata was not dropped and downgraded")
				}
			} else if test.name == "valid" && len(physicalHeld(t, state)) != 1 {
				t.Fatal("valid optional held rows were lost")
			}
			if !slices.Equal(state.History, []string{"archive1", "archive2", "A", "B"}) {
				t.Fatal("held validation changed core rows")
			}
		})
	}
}

func TestHeldDecodeErrorsPreserveCoreAndFence(t *testing.T) {
	for _, held := range []string{`{}`, `[{"rows":[7],"position":0,"segment":0,"first_seen_at":"2026-01-02T03:04:05Z"}]`, `[{"rows":["held"],"position":0,"segment":0,"first_seen_at":"not-time"}]`, `null`, `[{"rows":[null],"position":0,"segment":0,"first_seen_at":"2026-01-02T03:04:05Z"}]`} {
		m := NewManager(t.TempDir())
		data := `{"history":["A","B"],"footer":["old-footer"],"snapshot_version":1,"terminal_id":"terminal-1","read_source":"recent","segment_start":0,"live_start":0,"geometry_epoch":1,"lease_revision":1,"held":` + held + `}`
		if err := os.WriteFile(m.stateFile("p"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		state := m.loadState("p")
		if !slices.Equal(state.History, []string{"A", "B"}) || !slices.Equal(state.Footer, []string{"old-footer"}) || !state.snapshotInvalid || len(physicalHeld(t, state)) != 0 {
			t.Fatalf("optional decode lost core or omitted fence: history=%v invalid=%v", state.History, state.snapshotInvalid)
		}
		ctx := physicalContext(1, 54)
		ctx.ReadSource = SourceLegacy
		got, _, _ := m.AdmitSnapshot("p", snapshotPage("new1", "new2"), ctx, 0)
		if got != snapshotPage("A", "B", "new1", "new2") {
			t.Fatal("invalid held metadata did not fence on legacy admission")
		}
	}
}

func TestOptionalSnapshotMetadataTypesPreserveCoreRows(t *testing.T) {
	for _, metadata := range []string{`"geometry_epoch":-1`, `"live_start":"3"`, `"terminal_id":7`, `"read_source":{}`, `"snapshot_version":[]`} {
		m := NewManager(t.TempDir())
		data := `{"history":["A","B"],"footer":["footer"],"stale_refusals":0,"last_hash":"h",` + metadata + `}`
		if err := os.WriteFile(m.stateFile("p"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		state := m.loadState("p")
		if !slices.Equal(state.History, []string{"A", "B"}) || !slices.Equal(state.Footer, []string{"footer"}) || !state.snapshotInvalid {
			t.Fatalf("optional metadata reset valid core: history=%q footer=%q invalid=%v", state.History, state.Footer, state.snapshotInvalid)
		}
		ctx := physicalContext(1, 54)
		ctx.ReadSource = SourceLegacy
		got, _, _ := m.AdmitSnapshot("p", snapshotPage("new1", "new2"), ctx, 0)
		if got != snapshotPage("A", "B", "new1", "new2") {
			t.Fatal("bad optional metadata did not retain a frozen prefix")
		}
	}
}

func TestHeldMalformedWholeFileResetsAndUnknownVersionFences(t *testing.T) {
	for _, test := range []struct {
		name, data string
		wantRows   int
	}{
		{"syntax", `{"history":["A","B"],"held":[`, 0},
		{"core-type", `{"history":7,"held":[]}`, 0},
		{"unknown-version", `{"history":["A","B"],"footer":["f"],"snapshot_version":99,"read_source":"recent","held":[{"rows":["held"],"position":0,"segment":0,"first_seen_at":"2026-01-02T03:04:05Z"}]}`, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			if err := os.WriteFile(m.stateFile("p"), []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			state := m.loadState("p")
			if len(state.History) != test.wantRows || len(physicalHeld(t, state)) != 0 {
				t.Fatal("whole-file or unknown-version downgrade changed")
			}
			if test.name == "unknown-version" && !state.snapshotInvalid {
				t.Fatal("unknown version lacks conservative fence")
			}
		})
	}
}

func TestHeldTrimRebasesExpiresAndClamps(t *testing.T) {
	history := make([]string, MaxLines+3)
	state := heldWireState(t, history, 6, []any{heldRecordWire([]string{"expire"}, 2, 0), heldRecordWire([]string{"at"}, 3, 1), heldRecordWire([]string{"after"}, 5, 4), heldRecordWire([]string{"equal"}, 5, 4)})
	trimSnapshotState(state)
	records := physicalHeld(t, state)
	if len(records) != 3 {
		t.Fatalf("trim retained %d records, want three surviving pins", len(records))
	}
	if records[0].Position != 0 || records[0].Segment != 0 || records[1].Position != 2 || records[1].Segment != 1 || records[2].Rows[0] != "equal" {
		t.Fatalf("trim/rebase/order=%+v", records)
	}
}

func TestHeldOnlyCapacityMutationIsDurableMetadata(t *testing.T) {
	m := NewManager(t.TempDir())
	now := time.Unix(10000, 0)
	m.now = func() time.Time { return now }
	ctx := physicalContext(1, 54)
	raw := snapshotPage("A", "B")
	m.AdmitSnapshot("p", raw, ctx, 0)
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	state := heldWireState(t, []string{"A", "B"}, 0, []any{heldRecordWire(slices.Repeat([]string{"held"}, 2004), 0, 0)})
	state.LastHash = m.states["p"].LastHash
	m.states["p"] = state
	version := state.mutationVersion
	got, _, admission := m.AdmitSnapshot("p", raw, ctx, 0)
	if got != raw || admission.ContentChanged || !admission.MetadataChanged || !state.dirty || state.mutationVersion != version+1 {
		t.Fatalf("held-only mutation admission=%+v dirty=%v version=%d", admission, state.dirty, state.mutationVersion)
	}
	if len(physicalHeldRows(t, state)) != 2000 || admission.HeldEvictedRows != 4 || admission.HeldExpiredRows != 0 {
		t.Fatal("held ring bound or eviction diagnostics differ")
	}
	due := state.saveDue
	now = now.Add(time.Second)
	m.AdmitSnapshot("p", raw, ctx, 0)
	if state.saveDue != due {
		t.Fatal("unchanged held admission postponed dirty deadline")
	}
	if err := m.FlushDue(due); err != nil {
		t.Fatal(err)
	}
	loaded := NewManager(strings.TrimSuffix(m.dir, "/claude-history")).loadState("p")
	if len(physicalHeldRows(t, loaded)) != 2000 || !slices.Equal(loaded.History, []string{"A", "B"}) {
		t.Fatal("held-only change was not durable")
	}
	version = state.mutationVersion
	m.AdmitSnapshot("p", raw, ctx, 0)
	if state.dirty || state.mutationVersion != version {
		t.Fatal("identical admission changed held metadata")
	}
}

func TestHeldArchiveExpiryAdmissionReportsPhysicalRows(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	history := slices.Repeat([]string{"frozen"}, MaxLines-1)
	history = append(history, "A", "B", "C", "D")
	state := heldWireState(t, history, MaxLines-1, []any{heldRecordWire([]string{"expired", ""}, 2, 0), heldRecordWire([]string{"survives"}, 3, 1)})
	m.states["p"] = state
	_, _, admission := m.AdmitSnapshot("p", snapshotPage("A", "B", "C", "D"), ctx, 0)
	if admission.HeldExpiredRows != 2 || admission.HeldEvictedRows != 0 || admission.HeldTotalRows != 1 || state.Held[0].Position != 0 || state.Held[0].Segment != 0 {
		t.Fatalf("archive expiry diagnostic=%+v held=%+v", admission, state.Held)
	}
}

func TestHeldPreviewDoesNotMutatePins(t *testing.T) {
	m := NewManager(t.TempDir())
	history := slices.Repeat([]string{"old"}, MaxLines+3)
	state := heldWireState(t, history, len(history), []any{heldRecordWire([]string{"held", "", "tail"}, 4, 3)})
	state.ReadSource = SourceLegacy
	m.states["p"] = state
	ctx := physicalContext(1, 54)
	ctx.ReadSource = SourceLegacy
	before := physicalHeldRows(t, state)
	_, ok := m.PreviewSnapshot("p", snapshotPage("new1", "new2"), ctx)
	if !ok || !slices.Equal(physicalHeldRows(t, state), before) || len(physicalHeld(t, state)) != 1 || physicalHeld(t, state)[0].Position != 4 {
		t.Fatal("preview changed original held rows or pins")
	}
}

func TestHeldQueuedSaveOwnsNestedRows(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, pane := range []string{"a", "b"} {
		state := heldWireState(t, []string{"A", "B"}, 0, []any{heldRecordWire([]string{"original", "", "tail"}, 0, 0)})
		state.dirty = true
		state.mutationVersion = 1
		m.states[pane] = state
	}
	first := true
	m.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if first {
			first = false
			other := "a"
			if strings.HasSuffix(path, "a.json.tmp") {
				other = "b"
			}
			m.mu.Lock()
			state := m.states[other]
			state.Held[0].Rows[0] = "newer"
			state.dirty = true
			state.mutationVersion++
			m.mu.Unlock()
		}
		return os.WriteFile(path, data, mode)
	}
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	for _, pane := range []string{"a", "b"} {
		loaded := persistedState(t, m, pane)
		if rows := physicalHeldRows(t, &loaded); len(rows) != 3 || rows[0] != "original" {
			t.Fatalf("queued save lost held snapshot for %s: %v", pane, rows)
		}
	}
}

func TestHeldFencesPreserveAndTerminalResetDrops(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	state := heldWireState(t, []string{"A", "B"}, 0, []any{heldRecordWire([]string{"held"}, 0, 0)})
	m.states["p"] = state
	for _, change := range []string{"geometry", "source", "restart"} {
		switch change {
		case "geometry":
			ctx.GeometryEpoch++
		case "source":
			ctx.ReadSource = SourceLegacy
		case "restart":
			state.loaded = true
			ctx.NoScrollback = true
		}
		m.AdmitSnapshot("p", snapshotPage("new1", "new2"), ctx, 0)
		if len(physicalHeldRows(t, state)) != 1 {
			t.Fatalf("%s fence dropped old held rows", change)
		}
	}
	ctx.TerminalID = "replacement"
	m.AdmitSnapshot("p", snapshotPage("replacement1", "replacement2"), ctx, 0)
	if len(physicalHeldRows(t, m.states["p"])) != 0 {
		t.Fatal("terminal replacement kept old held rows")
	}
	m.Discard("p")
	if len(physicalHeldRows(t, m.loadState("p"))) != 0 {
		t.Fatal("discard retained held rows")
	}
}

func TestHeldFixtureLedgerHasInteriorBlanks(t *testing.T) {
	state := heldWireState(t, []string{"A", "B"}, 0, []any{heldRecordWire([]string{"one", "", "two"}, 0, 0)})
	if got := fmt.Sprint(physicalHeldRows(t, state)); got != "[one  two]" {
		t.Fatalf("held raw layout=%s", got)
	}
}

func TestHeldRingChurnCannotTrimTranscript(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	raw := snapshotPage("A", "B")
	m.AdmitSnapshot("p", raw, ctx, 0)
	state := m.states["p"]
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	total, evicted := 0, 0
	for frame := range 3001 {
		rows := []string{fmt.Sprintf("slot%d-a", frame), "", fmt.Sprintf("slot%d-b", frame), fmt.Sprintf("slot%d-c", frame)}
		appendHeldRows(state, rows, 0, 0, stamp.Add(time.Duration(frame)*time.Second))
		total += len(rows)
		got, _, admission := m.AdmitSnapshot("p", raw, ctx, 0)
		evicted += admission.HeldEvictedRows
		if got != raw || !slices.Equal(state.History, []string{"A", "B"}) || state.LiveStart != 0 || admission.HeldTotalRows != min(total, MaxHeldRows) {
			t.Fatalf("held-only frame %d changed transcript or capacity", frame)
		}
		if len(state.Held) > MaxHeldRows {
			t.Fatal("record count exceeds held row bound")
		}
	}
	if total <= MaxLines || evicted != total-MaxHeldRows || state.Held[0].Rows[0] != "slot2501-a" {
		t.Fatalf("churn total=%d evicted=%d first=%q", total, evicted, state.Held[0].Rows[0])
	}
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	loaded := NewManager(strings.TrimSuffix(m.dir, "/claude-history")).loadState("p")
	if !equalHeldRecords(loaded.Held, state.Held) || len(loaded.History) != 2 {
		t.Fatal("bounded held churn did not reload")
	}
}

func TestHeldRingPartialEvictionKeepsTimestampAndEqualPinOrder(t *testing.T) {
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	state := &PaneState{Held: []HeldRecord{
		{Rows: []string{"old-a", "old-b", "old-c", "old-d", "old-e"}, FirstSeenAt: stamp},
		{Rows: slices.Repeat([]string{"second"}, 1998), FirstSeenAt: stamp.Add(time.Second)},
	}}
	if removed := trimHeldCapacity(state); removed != 3 {
		t.Fatalf("removed=%d want=3", removed)
	}
	if !slices.Equal(state.Held[0].Rows, []string{"old-d", "old-e"}) || !state.Held[0].FirstSeenAt.Equal(stamp) || state.Held[1].Rows[0] != "second" {
		t.Fatal("partial eviction changed surviving suffix, timestamp or append order")
	}
	if removed := trimHeldCapacity(state); removed != 0 {
		t.Fatal("capacity removal counted twice")
	}
	state.Held = append(state.Held, HeldRecord{Rows: []string{"new"}, FirstSeenAt: stamp.Add(2 * time.Second)})
	if trimHeldCapacity(state) != 1 || state.Held[0].Rows[0] != "old-e" {
		t.Fatal("next admission did not continue oldest-first eviction")
	}
}

func TestHeldReverseClampAndTrimCounts(t *testing.T) {
	stamp := time.Now().UTC()
	state := &PaneState{History: make([]string, MaxLines+3), LiveStart: 6, Held: []HeldRecord{
		{Rows: []string{"expire", ""}, Position: 2, FirstSeenAt: stamp},
		{Rows: []string{"at"}, Position: 3, Segment: 1, FirstSeenAt: stamp},
		{Rows: []string{"after"}, Position: 5, Segment: 4, FirstSeenAt: stamp},
	}}
	if expired := trimSnapshotState(state); expired != 2 {
		t.Fatalf("expired=%d, want two physical rows", expired)
	}
	state.LiveStart = 1
	clampHeldPins(state)
	if state.Held[1].Position != 1 || state.Held[1].Segment != 1 || state.Held[0].Position != 0 {
		t.Fatal("reverse retraction did not clamp position then segment")
	}
	if trimSnapshotState(state) != 0 {
		t.Fatal("archive expiry counted twice")
	}
}

func TestHeldAppendRejectsBlankRunsAndOwnsRawRows(t *testing.T) {
	state := &PaneState{LiveStart: 3}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("test", 3600))
	if appendHeldRows(state, []string{"", " ", "\x1b[31m \x1b[0m\r"}, 2, 1, stamp) != 0 || len(state.Held) != 0 {
		t.Fatal("blank-only run created held record")
	}
	rows := []string{"\x1b[31mraw\x1b[0m\r", "", "tail"}
	if appendHeldRows(state, rows, 2, 1, stamp) != 3 {
		t.Fatal("held append did not count physical rows")
	}
	rows[0] = "changed"
	if state.Held[0].Rows[0] != "\x1b[31mraw\x1b[0m\r" || state.Held[0].FirstSeenAt.Location() != time.UTC || !state.Held[0].FirstSeenAt.Equal(stamp) {
		t.Fatal("held append changed raw storage ownership or timestamp")
	}
}

func TestHeldPreviewAndCloneOwnNestedRows(t *testing.T) {
	state := heldWireState(t, []string{"A", "B"}, 0, []any{heldRecordWire([]string{"original", "", "tail"}, 0, 0)})
	clone := cloneHeldRecords(state.Held)
	clone[0].Rows[0] = "changed"
	clone[0].Position = 1
	if state.Held[0].Rows[0] != "original" || state.Held[0].Position != 0 {
		t.Fatal("held clone aliases original nested rows")
	}
}

func TestHeldInFlightNewerMutationRemainsDirtyAndFinalSaveReloads(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 54)
	raw := snapshotPage("A", "B")
	m.AdmitSnapshot("p", raw, ctx, 0)
	state := m.states["p"]
	state.Held = []HeldRecord{{Rows: []string{"old", "", "tail"}, FirstSeenAt: time.Now().UTC()}}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	m.beforeRename = func() { close(entered); <-release }
	go func() { done <- m.SaveAll() }()
	<-entered
	m.mu.Lock()
	state.Held[0].Rows[0] = "new"
	state.mutationVersion++
	state.dirty = true
	m.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !state.dirty {
		t.Fatal("older held save cleared newer dirty state")
	}
	first := persistedState(t, m, "p")
	if first.Held[0].Rows[0] != "old" {
		t.Fatal("save snapshot aliased newer raw row")
	}
	m.beforeRename = nil
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	loaded := NewManager(strings.TrimSuffix(m.dir, "/claude-history")).loadState("p")
	if loaded.Held[0].Rows[0] != "new" || state.dirty {
		t.Fatal("final save did not retain newer held mutation")
	}
}

func TestHeldDiscardAndReplacementFenceQueuedRename(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		m := NewManager(t.TempDir())
		ctx := physicalContext(1, 54)
		m.AdmitSnapshot("p", snapshotPage("A", "B"), ctx, 0)
		m.states["p"].Held = []HeldRecord{{Rows: []string{"held"}, FirstSeenAt: time.Now().UTC()}}
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		m.beforeRename = func() { close(entered); <-release }
		go func() { done <- m.SaveAll() }()
		<-entered
		if replacement {
			ctx.TerminalID = "new-terminal"
			m.AdmitSnapshot("p", snapshotPage("new1", "new2"), ctx, 0)
		} else {
			m.Discard("p")
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(m.stateFile("p")); !os.IsNotExist(err) {
			t.Fatal("obsolete held save recreated state file")
		}
		if _, err := os.Stat(m.stateFile("p") + ".tmp"); !os.IsNotExist(err) {
			t.Fatal("obsolete held save left temporary file")
		}
	}
}
