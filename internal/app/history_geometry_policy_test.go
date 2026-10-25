package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
)

type countingHistoryGeometry struct {
	*paneSizeFakes
	pane, process, resolver, open, device, size atomic.Int32
	failSize                                    atomic.Bool
}

func (g *countingHistoryGeometry) PaneGet(ctx context.Context, paneID string) (*herdr.Pane, error) {
	g.pane.Add(1)
	return g.paneSizeFakes.PaneGet(ctx, paneID)
}

func (g *countingHistoryGeometry) PaneProcessInfoFresh(ctx context.Context, paneID string) (*herdr.PaneProcessInfo, error) {
	g.process.Add(1)
	return g.paneSizeFakes.PaneProcessInfoFresh(ctx, paneID)
}

func (g *countingHistoryGeometry) TTYPath(ctx context.Context, pid int) (string, error) {
	g.resolver.Add(1)
	return g.paneSizeFakes.TTYPath(ctx, pid)
}

func (g *countingHistoryGeometry) Open(path string) (panesize.Terminal, error) {
	g.open.Add(1)
	return g, nil
}

func (g *countingHistoryGeometry) Device() (uint64, bool, error) {
	g.device.Add(1)
	return g.paneSizeFakes.Device()
}

func (g *countingHistoryGeometry) Size() (panesize.Winsize, error) {
	g.size.Add(1)
	if g.failSize.Load() {
		return panesize.Winsize{}, errors.New("test size unavailable")
	}
	return g.paneSizeFakes.Size()
}

func (g *countingHistoryGeometry) calls() [6]int32 {
	return [6]int32{g.pane.Load(), g.process.Load(), g.resolver.Load(), g.open.Load(), g.device.Load(), g.size.Load()}
}

func assertNoHistoryGeometryCalls(t *testing.T, g *countingHistoryGeometry, before [6]int32) {
	t.Helper()
	if got := g.calls(); got != before {
		t.Errorf("legacy geometry calls [pane process resolver open device size]=%v, want unchanged %v", got, before)
	}
}

func countingGeometryServer(t *testing.T, leased bool) (*Server, *countingHistoryGeometry, func(time.Duration)) {
	t.Helper()
	s, f, advance := captureServer(t, false)
	g := &countingHistoryGeometry{paneSizeFakes: f}
	s.paneSizeM = panesize.NewManagerWith(g, g, g, s.historyNow, nil)
	if leased {
		if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20); err != nil {
			t.Fatal(err)
		}
		advance(4 * time.Second)
	}
	return s, g, advance
}

func TestLegacyHistoryWritersDoNoReadGeometry(t *testing.T) {
	for _, metric := range []string{"known-zero", "positive", "unknown"} {
		for _, writer := range []string{"owner", "non-owner", "scheduled", "completion"} {
			t.Run(metric+"/"+writer, func(t *testing.T) {
				s, g, _ := countingGeometryServer(t, false)
				agent, _ := s.state.Agent("pane-1")
				if metric == "positive" {
					agent.ScrollMaxOffset = 10
				} else if metric == "unknown" {
					agent.ScrollMetricsKnown = false
				}
				s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
				raw := capturePage("A", "B", "C")
				installPaneReadFixture(t, s, func(context.Context) (string, error) { return raw, nil })
				before := g.calls()
				switch writer {
				case "owner", "non-owner":
					message := map[string]any{"pane_id": "pane-1", "format": "ansi", "lines": 1000}
					s.applyPaneReadLease(writer, message)
					response := s.readPaneResponse(context.Background(), message)
					if response["error"] != nil || response["content"] != raw {
						t.Fatalf("legacy display=%+v", response)
					}
				case "scheduled":
					s.historyTasks = newLifecycleTasks(context.Background())
					t.Cleanup(s.historyTasks.Stop)
					s.scheduleHistoryCapture(context.Background(), "pane-1")
					waitHistoryCapture(t, s)
				case "completion":
					if got := s.captureFinishedPane(context.Background(), "pane-1", "claude", "", ""); got != raw {
						t.Fatalf("legacy completion=%q", got)
					}
				}
				assertNoHistoryGeometryCalls(t, g, before)
				if got := s.historyM.Content("pane-1", 0); got != raw {
					t.Errorf("legacy writer did not retain baseline page: %q", got)
				}
			})
		}
	}
}

func TestLegacyHistoryLeasePolicyCycleRejectsWithoutReadGeometry(t *testing.T) {
	for _, cycle := range []string{"off-on", "off-on-off"} {
		t.Run(cycle, func(t *testing.T) {
			s, g, advance := countingGeometryServer(t, false)
			baseline := capturePage("A", "B", "C")
			executeCapture(t, s, baseline, false)
			request := captureRequest(s, false)
			before := g.calls()
			var afterWrites [6]int32
			result, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
				assertNoHistoryGeometryCalls(t, g, before)
				if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20); err != nil {
					return herdr.PaneRead{}, err
				}
				if cycle == "off-on-off" {
					if err := s.paneSizeM.Release(context.Background(), "owner", "pane-1", true); err != nil {
						return herdr.PaneRead{}, err
					}
				}
				advance(4 * time.Second)
				afterWrites = g.calls()
				return herdr.PaneRead{Content: []byte(capturePage("B", "C", "D"))}, nil
			})
			assertNoHistoryGeometryCalls(t, g, afterWrites)
			if err != nil || result.Capture.Admitted || s.historyM.Content("pane-1", 0) != baseline {
				t.Errorf("lease policy cycle admitted legacy read: capture=%+v error=%v", result.Capture, err)
			}
			if cycle == "off-on-off" {
				if on, _ := s.physicalHistorySource(captureRequest(s, false).Agent); on {
					t.Error("off-on-off fixture did not finish gate-off")
				}
			}
		})
	}
}

func TestLegacyHistoryRestoreCrossingRejectsWithoutReadGeometry(t *testing.T) {
	s, g, advance := countingGeometryServer(t, true)
	agent, _ := s.state.Agent("pane-1")
	agent.ScrollMaxOffset = 10
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	executeCapture(t, s, capturePage("A", "B", "C"), false)
	before := g.calls()
	var afterWrites [6]int32
	result, err := s.executeHistoryRead(context.Background(), captureRequest(s, false), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		assertNoHistoryGeometryCalls(t, g, before)
		if err := s.paneSizeM.Release(context.Background(), "owner", "pane-1", true); err != nil {
			return herdr.PaneRead{}, err
		}
		advance(4 * time.Second)
		afterWrites = g.calls()
		return herdr.PaneRead{Content: []byte(capturePage("B", "C", "D"))}, nil
	})
	assertNoHistoryGeometryCalls(t, g, afterWrites)
	if err != nil || result.Capture.Admitted {
		t.Errorf("legacy restore crossing admitted: capture=%+v error=%v", result.Capture, err)
	}
}

func TestPhysicalHistoryPendingRestoreStillSamplesGeometry(t *testing.T) {
	s, g, advance := countingGeometryServer(t, true)
	g.failSize.Store(true)
	if err := s.paneSizeM.Release(context.Background(), "owner", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	g.failSize.Store(false)
	advance(4 * time.Second)
	if physical, _ := s.physicalHistorySource(captureRequest(s, true).Agent); !physical {
		t.Fatal("pending restore disabled the pane-wide physical gate")
	}
	before := g.calls()
	result := executeCapture(t, s, capturePage("A", "B", "C"), true)
	if !result.Capture.Admitted || result.Capture.Snapshot.ReadSource != history.SourceRecent ||
		g.open.Load() <= before[3] || g.size.Load() < before[5]+2 {
		t.Errorf("pending-restore physical read skipped geometry: calls=%v before=%v capture=%+v", g.calls(), before, result.Capture)
	}
}

func TestLegacyHistoryMetricPolicyCycleRejectsWithoutGeometry(t *testing.T) {
	s, g, _ := countingGeometryServer(t, false)
	request := captureRequest(s, false)
	before := g.calls()
	result, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		agent, _ := s.state.Agent("pane-1")
		agent.ScrollMaxOffset = 10
		s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
		agent.ScrollMaxOffset = 0
		s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
		return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
	})
	assertNoHistoryGeometryCalls(t, g, before)
	if err != nil || result.Capture.Admitted {
		t.Errorf("legacy metric cycle admitted: capture=%+v error=%v", result.Capture, err)
	}
}

func TestPhysicalGeometrySelectionKeepsShapeAndIdentityEligibility(t *testing.T) {
	for _, shape := range []string{"text", "missing-terminal"} {
		t.Run(shape, func(t *testing.T) {
			s, g, _ := countingGeometryServer(t, true)
			request := captureRequest(s, true)
			if shape == "text" {
				request.Format = "text"
			} else {
				request.Agent.TerminalID = ""
			}
			before := g.calls()
			result, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
				return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
			})
			assertNoHistoryGeometryCalls(t, g, before)
			if err != nil || result.Capture.Admitted {
				t.Errorf("ineligible physical shape admitted: capture=%+v error=%v", result.Capture, err)
			}
		})
	}
}

func TestLegacyHistoryReadErrorStillDoesNoGeometry(t *testing.T) {
	s, g, _ := countingGeometryServer(t, false)
	before := g.calls()
	want := errors.New("content failed")
	_, err := s.executeHistoryRead(context.Background(), captureRequest(s, false), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		return herdr.PaneRead{}, want
	})
	assertNoHistoryGeometryCalls(t, g, before)
	if !errors.Is(err, want) {
		t.Errorf("legacy content error=%v, want %v", err, want)
	}
}

func TestLegacyHistoryPendingAndPreviewNeedNoGeometry(t *testing.T) {
	s, g, _ := countingGeometryServer(t, false)
	pendingDeparture(s, 10)
	before := g.calls()
	first := executeCapture(t, s, capturePage("A", "B", "C"), false)
	if !first.Capture.Admitted || s.historyPending["pane-1"] != nil {
		t.Error("legacy pending request did not acknowledge")
	}
	request := captureRequest(s, false)
	request.Generation--
	result, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		return herdr.PaneRead{Content: []byte(capturePage("B", "C", "D"))}, nil
	})
	assertNoHistoryGeometryCalls(t, g, before)
	if err != nil || result.Capture.Admitted || !result.Composed || result.Response["history_retry"] != true ||
		result.Response["content"] != capturePage("A", "B", "C", "D") || s.historyM.Content("pane-1", 0) != capturePage("A", "B", "C") {
		t.Errorf("geometry-independent preview=%+v error=%v", result, err)
	}
	if first.Capture.Snapshot.GeometryEpoch != 0 || first.Capture.Snapshot.Columns != 0 || first.Capture.Snapshot.Rows != 0 || first.Capture.Snapshot.ReadSource != history.SourceLegacy {
		t.Errorf("legacy capture gained geometry: %+v", first.Capture.Snapshot)
	}
}
