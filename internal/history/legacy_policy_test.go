package history

import (
	"os"
	"testing"
)

func TestLegacyBoundaryPolicyPreservesFrozenPhysicalPrefix(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.MergeSnapshot("p", snapshotPage("physical A", "physical B"), physicalContext(1, 40))
	ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, GeometryEpoch: 2, LeaseRevision: 2}
	page := snapshotPage("A", "B", "C")
	want := snapshotPage("physical A", "physical B", "A", "B", "C")
	for range 3 {
		if got := m.MergeSnapshot("p", page, ctx); got != want {
			t.Fatalf("legacy matching changed frozen prefix: %q", got)
		}
		if err := m.SaveAll(); err != nil {
			t.Fatal(err)
		}
		ctx.GeometryEpoch++
		m = NewManager(dir)
	}
}

func TestFullscreenLegacyRetainsResizeAndRestartBoundaries(t *testing.T) {
	for _, oldFormat := range []bool{false, true} {
		dir := t.TempDir()
		m := NewManager(dir)
		ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, GeometryEpoch: 1, NoScrollback: true}
		page := snapshotPage("A", "B")
		if oldFormat {
			if err := os.WriteFile(m.stateFile("p"), []byte(`{"history":["A","B"],"footer":[],"stale_refusals":0,"last_hash":""}`), 0o600); err != nil {
				t.Fatal(err)
			}
			assertSnapshotContent(t, m.MergeSnapshot("p", page, ctx), "A", "B", "A", "B")
			continue
		}
		m.MergeSnapshot("p", page, ctx)
		ctx.GeometryEpoch++
		assertSnapshotContent(t, m.MergeSnapshot("p", page, ctx), "A", "B", "A", "B")
		if err := m.SaveAll(); err != nil {
			t.Fatal(err)
		}
		m = NewManager(dir)
		assertSnapshotContent(t, m.MergeSnapshot("p", page, ctx), "A", "B", "A", "B", "A", "B")
	}
}

func TestScrollbackLegacyMatchesAcrossResizeAndRestarts(t *testing.T) {
	for _, oldFormat := range []bool{false, true} {
		dir := t.TempDir()
		m := NewManager(dir)
		ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, GeometryEpoch: 1}
		page := snapshotPage("A", "B", "C")
		if oldFormat {
			if err := os.WriteFile(m.stateFile("p"), []byte(`{"history":["A","B","C"],"footer":[],"stale_refusals":0,"last_hash":""}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for range 3 {
			if got := m.MergeSnapshot("p", page, ctx); got != page {
				t.Fatalf("scrollback boundary duplicated page: oldFormat=%v content=%q", oldFormat, got)
			}
			ctx.GeometryEpoch++
			if got := m.MergeSnapshot("p", page, ctx); got != page {
				t.Fatalf("scrollback resize duplicated page: %q", got)
			}
			if err := m.SaveAll(); err != nil {
				t.Fatal(err)
			}
			m = NewManager(dir)
		}
	}
}
