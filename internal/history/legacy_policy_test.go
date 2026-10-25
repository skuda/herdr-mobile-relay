package history

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

const legacyBaselineFooter = "\nfooter1\nfooter2\nfooter3\nfooter4\nfooter5\nfooter6"

type legacyBaselineFrame struct {
	body, want string
}

func legacyBaselineCases() map[string][]legacyBaselineFrame {
	return map[string][]legacyBaselineFrame{
		"same":              {{"A\nB\nC", "A\nB\nC"}, {"A\nB\nC", "A\nB\nC"}, {"A\nB\nC", "A\nB\nC"}},
		"growing":           {{"A\nB\nC", "A\nB\nC"}, {"A\nB\nC\nD", "A\nB\nC\nD"}, {"A\nB\nC\nD\nE", "A\nB\nC\nD\nE"}},
		"append":            {{"A\nB\nC", "A\nB\nC"}, {"B\nC\nD", "A\nB\nC\nD"}, {"C\nD\nE", "A\nB\nC\nD\nE"}},
		"sequence-rebase":   {{"H1\nH2\nA\nB\nC\nD\nE", "H1\nH2\nA\nB\nC\nD\nE"}, {"H1\nH2\nC\nD\nE\nF\nG", "H1\nH2\nA\nB\nC\nD\nE\nF\nG"}},
		"divergent":         {{"A\nB\nold1\nold2", "A\nB\nold1\nold2"}, {"A\nB\nnew1\nnew2", "A\nB\nnew1\nnew2"}, {"A\nB\nlast1\nlast2", "A\nB\nlast1\nlast2"}},
		"divergent-refusal": {{"A\nB\nold1\nold2\nold3\nold4", "A\nB\nold1\nold2\nold3\nold4"}, {"A\nB\nnew1\nnew2\nnew3", "A\nB\nold1\nold2\nold3\nold4"}, {"A\nB\nnew1\nnew2\nnew3", "A\nB\nnew1\nnew2\nnew3"}},
		"ansi-refresh":      {{"A\nB\n\x1b[42mstyle\x1b[0m\nold1\nold2", "A\nB\n\x1b[42mstyle\x1b[0m\nold1\nold2"}, {"A\nB\n\x1b[44mstyle\x1b[0m\nnew1\nnew2", "A\nB\n\x1b[44mstyle\x1b[0m\nnew1\nnew2"}},
	}
}

func TestFullscreenLegacyMatchesAcrossResizeAndRestart(t *testing.T) {
	for _, oldFormat := range []bool{false, true} {
		for name, frames := range legacyBaselineCases() {
			t.Run(fmt.Sprintf("old_format=%v/%s", oldFormat, name), func(t *testing.T) {
				dir := t.TempDir()
				m := NewManager(dir)
				ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, GeometryEpoch: 1, NoScrollback: true}
				if oldFormat {
					writeLegacyBaselineFile(t, m, frames[0].body)
				}
				for i, frame := range frames {
					ctx.GeometryEpoch = uint64(i + 1)
					assertLegacyBaselineAdmission(t, m, ctx, frame)
					if err := m.SaveAll(); err != nil {
						t.Fatal(err)
					}
					m = NewManager(dir)
					if got := m.Content("p", 0); got != frame.want+legacyBaselineFooter {
						t.Fatalf("restart content=%q, want literal v0.22.10 bytes %q", got, frame.want+legacyBaselineFooter)
					}
				}
			})
		}
	}
}

func writeLegacyBaselineFile(t *testing.T, m *Manager, body string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"history": strings.Split(body, "\n"), "footer": []string{}, "stale_refusals": 0, "last_hash": ""})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.stateFile("p"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertLegacyBaselineAdmission(t *testing.T, m *Manager, ctx SnapshotContext, frame legacyBaselineFrame) {
	t.Helper()
	got, _, admission := m.AdmitSnapshot("p", frame.body+legacyBaselineFooter, ctx, 0)
	if want := frame.want + legacyBaselineFooter; got != want {
		t.Errorf("legacy content=%q, want literal v0.22.10 bytes %q", got, want)
	}
	state := m.states["p"]
	if !admission.Admitted || state.SegmentStart != 0 || state.LiveStart != len(state.History) || state.SnapshotVersion != 1 || state.ReadSource != SourceLegacy {
		t.Errorf("legacy continuation segment=%d live=%d rows=%d version=%d source=%q admitted=%v", state.SegmentStart, state.LiveStart, len(state.History), state.SnapshotVersion, state.ReadSource, admission.Admitted)
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

func TestFullscreenLegacyMatchesAcrossLeaseRevisions(t *testing.T) {
	for _, oldFormat := range []bool{false, true} {
		for name, frames := range legacyBaselineCases() {
			t.Run(fmt.Sprintf("old_format=%v/%s", oldFormat, name), func(t *testing.T) {
				m := NewManager(t.TempDir())
				ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, GeometryEpoch: 1, NoScrollback: true}
				if oldFormat {
					writeLegacyBaselineFile(t, m, frames[0].body)
				}
				for i, revision := range []uint64{0, 7, 0} {
					ctx.LeaseRevision = revision
					frame := frames[min(i, len(frames)-1)]
					assertLegacyBaselineAdmission(t, m, ctx, frame)
					if m.states["p"].LeaseRevision != revision {
						t.Errorf("stored lease revision=%d, want %d", m.states["p"].LeaseRevision, revision)
					}
				}
			})
		}
	}
}

func TestLegacyKnownZeroInvalidMetadataKeepsFrozenPrefix(t *testing.T) {
	for name, metadata := range map[string]string{
		"held-type":     `"snapshot_version":1,"read_source":"legacy","live_start":3,"held":{}`,
		"held-position": `"snapshot_version":1,"read_source":"legacy","live_start":3,"held":[{"rows":["held"],"position":4,"segment":0,"first_seen_at":"2026-01-02T03:04:05Z"}]`,
		"offsets":       `"snapshot_version":1,"read_source":"legacy","segment_start":-1,"live_start":3`,
		"version":       `"snapshot_version":99,"read_source":"legacy","live_start":3`,
		"source":        `"snapshot_version":1,"read_source":"unknown","live_start":3`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			m := NewManager(root)
			data := `{"history":["A","B","retained"],"terminal_id":"terminal-1",` + metadata + `}`
			if err := os.WriteFile(m.stateFile("p"), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, NoScrollback: true}
			for range 2 {
				got, _, _ := m.AdmitSnapshot("p", "A\nB\nnew"+legacyBaselineFooter, ctx, 0)
				if got != "A\nB\nretained\nA\nB\nnew"+legacyBaselineFooter || m.states["p"].SegmentStart != 3 {
					t.Errorf("invalid metadata reopened the prefix: content=%q segment=%d", got, m.states["p"].SegmentStart)
				}
				if err := m.SaveAll(); err != nil {
					t.Fatal(err)
				}
				m = NewManager(root)
			}
		})
	}
}

func TestLegacyKnownZeroDoesNotSearchPhysicalPrefix(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	m.AdmitSnapshot("p", snapshotPage("A", "B", "physical retained"), physicalContext(1, 54), 0)
	ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, NoScrollback: true}
	m.AdmitSnapshot("p", "current X\ncurrent Y"+legacyBaselineFooter, ctx, 0)
	for _, revision := range []uint64{0, 7, 0} {
		ctx.GeometryEpoch, ctx.LeaseRevision = revision+1, revision
		got, _, _ := m.AdmitSnapshot("p", "prefix\nA\nB\nlegacy suffix"+legacyBaselineFooter, ctx, 0)
		if got != "A\nB\nphysical retained\ncurrent X\ncurrent Y\nprefix\nA\nB\nlegacy suffix"+legacyBaselineFooter || m.states["p"].SegmentStart != 3 {
			t.Errorf("known-zero legacy matched frozen physical rows: content=%q segment=%d", got, m.states["p"].SegmentStart)
		}
		if err := m.SaveAll(); err != nil {
			t.Fatal(err)
		}
		m = NewManager(root)
	}
}

func TestLegacyKnownZeroPredicateCouplings(t *testing.T) {
	t.Run("runtime-boundary", func(t *testing.T) {
		m := NewManager(t.TempDir())
		ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, GeometryEpoch: 1, NoScrollback: true}
		assertLegacyBaselineAdmission(t, m, ctx, legacyBaselineFrame{"A\nB", "A\nB"})
		ctx.GeometryEpoch = 2
		assertLegacyBaselineAdmission(t, m, ctx, legacyBaselineFrame{"A\nB", "A\nB"})
	})
	t.Run("flat-file-upgrade", func(t *testing.T) {
		m := NewManager(t.TempDir())
		writeLegacyBaselineFile(t, m, "A\nB")
		ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, NoScrollback: true}
		assertLegacyBaselineAdmission(t, m, ctx, legacyBaselineFrame{"A\nB", "A\nB"})
	})
}

func TestLegacyLifecycleLiteralBaselineAcrossScrollMetrics(t *testing.T) {
	for _, metric := range []string{"known-zero", "positive", "unknown"} {
		for _, oldFormat := range []bool{false, true} {
			for name, frames := range legacyBaselineCases() {
				t.Run(fmt.Sprintf("%s/old_format=%v/%s", metric, oldFormat, name), func(t *testing.T) {
					root := t.TempDir()
					m := NewManager(root)
					ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy, NoScrollback: metric == "known-zero"}
					if oldFormat {
						writeLegacyBaselineFile(t, m, frames[0].body)
					}
					for i, revision := range []uint64{0, 7, 0} {
						ctx.GeometryEpoch, ctx.LeaseRevision = uint64(i+1), revision
						assertLegacyBaselineAdmission(t, m, ctx, frames[min(i, len(frames)-1)])
						if err := m.SaveAll(); err != nil {
							t.Fatal(err)
						}
						m = NewManager(root)
					}
				})
			}
		}
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
