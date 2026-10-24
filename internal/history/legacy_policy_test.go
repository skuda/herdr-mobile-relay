package history

import (
	"fmt"
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
	for _, leaseRevision := range []uint64{2, 7, 2} {
		ctx.LeaseRevision = leaseRevision
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

func TestScrollbackLegacyInvalidSnapshotKeepsFrozenPrefix(t *testing.T) {
	for name, metadata := range map[string]string{
		"unknown version":      `"snapshot_version":99,"read_source":"legacy","live_start":3`,
		"invalid offsets":      `"snapshot_version":1,"read_source":"legacy","segment_start":-1,"live_start":3`,
		"unknown source":       `"snapshot_version":1,"read_source":"unknown","live_start":3`,
		"version zero source":  `"read_source":"legacy"`,
		"version zero segment": `"segment_start":1`,
		"version zero live":    `"live_start":3`,
		"version zero epoch":   `"geometry_epoch":1`,
		"version zero lease":   `"lease_revision":1`,
		"version zero columns": `"columns":80`,
		"version zero rows":    `"rows":24`,
	} {
		t.Run(name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			data := []byte(`{"history":["A","B","retained"],"terminal_id":"terminal-1",` + metadata + `}`)
			if err := os.WriteFile(m.stateFile("p"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy}
			page := snapshotPage("A", "B", "new")
			for range 2 {
				assertSnapshotContent(t, m.MergeSnapshot("p", page, ctx), "A", "B", "retained", "A", "B", "new")
				if state := m.states["p"]; state.SegmentStart != 3 {
					t.Fatalf("invalid metadata reopened the frozen prefix: %+v", state)
				}
			}
		})
	}
}

func TestScrollbackLegacyMatchesAcrossLeaseRevisions(t *testing.T) {
	for _, oldFormat := range []bool{false, true} {
		for _, resize := range []bool{false, true} {
			t.Run(fmt.Sprintf("old_format=%v/resize=%v", oldFormat, resize), func(t *testing.T) {
				m, base := NewManager(t.TempDir()), NewManager(t.TempDir())
				if oldFormat {
					data := []byte(`{"history":["A","B","C","D"],"footer":[],"stale_refusals":0,"last_hash":""}`)
					for _, manager := range []*Manager{m, base} {
						if err := os.WriteFile(manager.stateFile("p"), data, 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, GeometryEpoch: 1}
				for i, leaseRevision := range []uint64{0, 7, 0} {
					ctx.LeaseRevision = leaseRevision
					if resize {
						ctx.GeometryEpoch = uint64(i + 1)
					}
					page := snapshotPage([]string{"A", "B", "C", "D", "E", "F"}[:4+i]...)
					want := base.Merge("p", page)
					if want != page {
						t.Fatalf("base merge changed growing page: %q", want)
					}
					if got := m.MergeSnapshot("p", page, ctx); got != want {
						t.Fatalf("lease revision %d: content = %q, base Merge = %q", leaseRevision, got, want)
					}
					if got := m.states["p"].LeaseRevision; got != leaseRevision {
						t.Fatalf("stored lease revision = %d, want %d", got, leaseRevision)
					}
				}
			})
		}
	}
}

func TestFullscreenLegacyRetainsLeaseBoundaries(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, GeometryEpoch: 1, NoScrollback: true}
	page := snapshotPage("A", "B")
	for i, leaseRevision := range []uint64{0, 7, 0} {
		ctx.LeaseRevision = leaseRevision
		want := make([]string, 0, 2*(i+1))
		for range i + 1 {
			want = append(want, "A", "B")
		}
		assertSnapshotContent(t, m.MergeSnapshot("p", page, ctx), want...)
		if state := m.states["p"]; state.SegmentStart != 2*i || state.LeaseRevision != leaseRevision {
			t.Fatalf("lease boundary state = %+v", state)
		}
		assertSnapshotContent(t, m.MergeSnapshot("p", page, ctx), want...)
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
