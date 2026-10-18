package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
)

func captureServer(t *testing.T, leased bool) (*Server, *paneSizeFakes, func(time.Duration)) {
	t.Helper()
	s := testServerWithCacheDir(t.TempDir())
	f := newPaneSizeFakes(s)
	var mu sync.Mutex
	now := time.Unix(10000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	s.paneSizeM = panesize.NewManagerWith(f, f, f, clock, nil)
	s.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", TerminalID: "terminal-1", Agent: "claude", Status: "working", ScrollMetricsKnown: true}}, 0)
	s.syncHistoryPanes(s.state.Snapshot())
	if leased {
		if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20); err != nil {
			t.Fatal(err)
		}
		advance(4 * time.Second)
	}
	return s, f, advance
}

func captureRequest(s *Server, physical bool) coordinator.PaneReadRequest {
	agent, _ := s.state.Agent("pane-1")
	return coordinator.PaneReadRequest{PaneID: "pane-1", Agent: agent, Generation: s.state.Generation("pane-1"),
		ContentRevision: s.state.ContentRevision("pane-1"), Physical: physical, Lines: history.PhysicalCaptureLines, Format: "ansi"}
}

func capturePage(body ...string) string {
	return strings.Join(append(body, "f1", "f2", "f3", "f4", "f5", "f6"), "\n")
}

func executeCapture(t *testing.T, s *Server, raw string, physical bool) coordinator.PaneReadResult {
	t.Helper()
	result, err := s.executeHistoryRead(context.Background(), captureRequest(s, physical),
		func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
			return herdr.PaneRead{Content: []byte(raw)}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPaneWideLeasedNoScrollbackWritersUseRecentANSI(t *testing.T) {
	s, _, _ := captureServer(t, true)
	root := t.TempDir()
	record := filepath.Join(root, "calls")
	bin := filepath.Join(root, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+record+"\"\nprintf 'A\\nB\\nC\\nD\\nE\\nf1\\nf2\\nf3\\nf4\\nf5\\nf6'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.herdrC = herdr.NewClient(bin, filepath.Join(root, "missing.sock"))
	t.Cleanup(func() { _ = s.herdrC.Close() })
	s.dispatcher = coordinator.NewDispatcher(s.herdrC, s.state, nil, s.logger)
	for _, reader := range []string{"owner", "non-owner"} {
		message := map[string]any{"pane_id": "pane-1", "lines": 100, "format": "ansi"}
		s.applyPaneReadLease(reader, message)
		response := s.readPaneResponse(context.Background(), message)
		if response["error"] != nil {
			t.Fatalf("reader %s = %+v", reader, response)
		}
	}
	s.historyTasks = newLifecycleTasks(context.Background())
	t.Cleanup(s.historyTasks.Stop)
	s.scheduleHistoryCapture(context.Background(), "pane-1")
	deadline := time.Now().Add(time.Second)
	for {
		s.historyCaptureMu.Lock()
		inflight := s.historyInflight["pane-1"]
		s.historyCaptureMu.Unlock()
		if !inflight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled capture did not finish")
		}
		runtime.Gosched()
	}
	s.historyTasks.Stop()
	_ = s.captureFinishedPane(context.Background(), "pane-1", "claude", "", "")
	calls, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "--lines 1000 --source recent --format ansi") != 4 || strings.Contains(string(calls), "recent-unwrapped") {
		t.Fatalf("writer source requests = %s", calls)
	}
}

func TestEligibleViewportReadRetainsHistoryAndCurrentFooter(t *testing.T) {
	s, _, _ := captureServer(t, true)
	executeCapture(t, s, capturePage("A", "B", "C", "D", "E"), true)
	raw := strings.ReplaceAll(capturePage("C", "D", "E", "F", "G"), "f6", "fresh footer")
	result := executeCapture(t, s, raw, true)
	if !result.Capture.Admitted || result.Response["content"] != "A\nB\n"+raw {
		t.Fatalf("eligible response = %+v", result)
	}
}

func TestHistoryWritersSkipAcquireRestoreSettleAndCrossedEpoch(t *testing.T) {
	for _, phase := range []string{"acquire settle", "restore settle", "crossed epoch", "unknown geometry"} {
		t.Run(phase, func(t *testing.T) {
			s, f, advance := captureServer(t, true)
			executeCapture(t, s, capturePage("old A", "old B"), true)
			request := captureRequest(s, true)
			if phase == "acquire settle" {
				_, _, _ = s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 70, 20)
			} else if phase == "restore settle" {
				_ = s.paneSizeM.Release(context.Background(), "owner", "pane-1", true)
				request = captureRequest(s, false)
			} else if phase == "unknown geometry" {
				f.mu.Lock()
				f.size.Columns = 0
				f.mu.Unlock()
			}
			_, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
				if phase == "crossed epoch" {
					_, _, _ = s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 70, 20)
					advance(4 * time.Second)
				}
				return herdr.PaneRead{Content: []byte(capturePage("new A", "new B"))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := s.historyM.Content("pane-1", 0); strings.Contains(got, "new A") {
				t.Fatalf("unsafe frame archived: %q", got)
			}
		})
	}
}

func TestHistoryReadPolicyChangeMidReadSkipsArchive(t *testing.T) {
	s, _, _ := captureServer(t, true)
	result, err := s.executeHistoryRead(context.Background(), captureRequest(s, true), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		agent, _ := s.state.Agent("pane-1")
		agent.ScrollMaxOffset = 10
		s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
		return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
	})
	if err != nil || result.Capture.Admitted {
		t.Fatalf("policy change = %+v error=%v", result, err)
	}
}

func TestFinishedReadPublishesOldEpochAfterLaterResize(t *testing.T) {
	s, _, advance := captureServer(t, true)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan coordinator.PaneReadResult, 1)
	s.historyReadFinishedObserver = func(historyReadToken) { close(entered); <-release }
	go func() { done <- executeCapture(t, s, capturePage("old A", "old B"), true) }()
	<-entered
	_, _, _ = s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 70, 20)
	close(release)
	if result := <-done; !result.Capture.Admitted {
		t.Fatal("finished old epoch was rejected")
	}
	s.historyReadFinishedObserver = nil
	advance(4 * time.Second)
	executeCapture(t, s, capturePage("new A", "new B"), true)
	if got := s.historyM.Content("pane-1", 0); got != capturePage("old A", "old B", "new A", "new B") {
		t.Fatalf("epoch composition = %q", got)
	}
}

func TestNewerArchivedReadRejectsOlderFinishedCompletion(t *testing.T) {
	s, _, _ := captureServer(t, true)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan coordinator.PaneReadResult, 1)
	s.historyReadFinishedObserver = func(token historyReadToken) {
		if token.sequence == 1 {
			close(entered)
			<-release
		}
	}
	go func() { done <- executeCapture(t, s, capturePage("old A", "old B"), true) }()
	<-entered
	newer := executeCapture(t, s, capturePage("new A", "new B"), true)
	close(release)
	older := <-done
	if !newer.Capture.Admitted || older.Capture.Admitted || older.Capture.Sequence >= newer.Capture.Sequence {
		t.Fatalf("older=%+v newer=%+v", older.Capture, newer.Capture)
	}
	if got := s.historyM.Content("pane-1", 0); got != capturePage("new A", "new B") {
		t.Fatalf("stale publication changed history: %q", got)
	}
}

func TestBackgroundReadRejectsTerminalReplacementAndGenerationChange(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		s, _, _ := captureServer(t, true)
		result, err := s.executeHistoryRead(context.Background(), captureRequest(s, true), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
			agent, _ := s.state.Agent("pane-1")
			if replacement {
				agent.TerminalID = "replacement"
			} else {
				agent.SessionID = "discovered"
			}
			s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
			s.syncHistoryPanes(s.state.Snapshot())
			return herdr.PaneRead{Content: []byte(capturePage("obsolete A", "obsolete B"))}, nil
		})
		if err != nil || result.Capture.Admitted || strings.Contains(s.historyM.Content("pane-1", 0), "obsolete") {
			t.Fatalf("replacement=%v result=%+v err=%v", replacement, result, err)
		}
	}
}

func TestLegacyReadSizingUnchanged(t *testing.T) {
	s, _, _ := captureServer(t, false)
	root := t.TempDir()
	record, bin := filepath.Join(root, "calls"), filepath.Join(root, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+record+"\"\nprintf 'A\\nB'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.herdrC = herdr.NewClient(bin, filepath.Join(root, "missing.sock"))
	t.Cleanup(func() { _ = s.herdrC.Close() })
	s.dispatcher = coordinator.NewDispatcher(s.herdrC, s.state, nil, s.logger)
	response := s.readPaneResponse(context.Background(), map[string]any{"pane_id": "pane-1", "lines": 100, "format": "ansi"})
	if response["error"] != nil {
		t.Fatal(response)
	}
	_, _ = s.captureTerminalHistory(context.Background(), "pane-1")
	calls, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "--lines 100 --source recent-unwrapped --format ansi") || !strings.Contains(string(calls), "--lines 10000 --source recent-unwrapped --format ansi") {
		t.Fatalf("legacy calls = %s", calls)
	}
}

func TestPhysicalCaptureBudgetIgnoresReaderLines(t *testing.T) {
	s, _, _ := captureServer(t, true)
	body := make([]string, 114)
	for i := range body {
		body[i] = fmt.Sprintf("marker-%03d", i)
	}
	raw := capturePage(body...)
	for range 4 {
		result := executeCapture(t, s, raw, true)
		if result.Response["content"] != raw || !result.Capture.Admitted {
			t.Fatalf("stable capture = %+v", result.Capture)
		}
	}
	if got := s.historyM.Content("pane-1", 0); got != raw {
		t.Fatal("unchanged physical page grew archive")
	}
}

func TestLeaseActivationAndRestoreAreSourceBoundaries(t *testing.T) {
	s, _, advance := captureServer(t, false)
	executeCapture(t, s, capturePage("legacy A", "legacy B"), false)
	_, _, _ = s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20)
	advance(4 * time.Second)
	executeCapture(t, s, capturePage("physical A", "physical B"), true)
	_ = s.paneSizeM.Release(context.Background(), "owner", "pane-1", false)
	if on, _ := s.physicalHistorySource(captureRequest(s, true).Agent); !on {
		t.Fatal("navigation grace disabled physical source")
	}
	advance(panesize.ReleaseGrace)
	s.paneSizeM.SweepExpired(context.Background())
	settling := executeCapture(t, s, capturePage("settling A", "settling B"), false)
	if settling.Capture.Admitted {
		t.Fatal("restore settling archived a page")
	}
	advance(4 * time.Second)
	result := executeCapture(t, s, capturePage("restored A", "restored B"), false)
	if !result.Capture.Admitted || result.Response["content"] != capturePage("legacy A", "legacy B", "physical A", "physical B", "restored A", "restored B") {
		t.Fatalf("restore composition = %+v", result)
	}
}

func TestHistoryLegacyAdmissionWithUnknownGeometry(t *testing.T) {
	s, f, _ := captureServer(t, false)
	f.mu.Lock()
	f.size.Columns = 0
	f.mu.Unlock()
	result := executeCapture(t, s, capturePage("legacy A", "legacy B"), false)
	if !result.Capture.Admitted {
		t.Fatal("legacy route imposed known-geometry eligibility")
	}
}

func TestPaneWatchPublishesSettledHistoryWithUnchangedProbe(t *testing.T) {
	s, _, _ := captureServer(t, true)
	executeCapture(t, s, capturePage("old A", "old B"), true)
	acknowledged := &paneWatchFrame{resizeSettling: true}
	if !paneWatchNeedsFrameRead("same", "same", acknowledged, "claude") {
		t.Fatal("unchanged probe skipped settled full read")
	}
	result := executeCapture(t, s, capturePage("new A", "new B"), true)
	if !result.Capture.Admitted || !strings.Contains(result.Response["content"].(string), "old A") {
		t.Fatal("settled read did not attach history")
	}
}

func TestOtherRelayPTYResizeDoesNotEnablePhysicalSource(t *testing.T) {
	s, f, advance := captureServer(t, false)
	executeCapture(t, s, capturePage("old A", "old B"), false)
	f.mu.Lock()
	f.size.Columns = 80
	f.mu.Unlock()
	if physical, _ := s.physicalHistorySource(captureRequest(s, false).Agent); physical {
		t.Fatal("external resize enabled gate")
	}
	if result := executeCapture(t, s, capturePage("settling A", "settling B"), false); result.Capture.Admitted {
		t.Fatal("external resize bypassed settling")
	}
	advance(4 * time.Second)
	result := executeCapture(t, s, capturePage("new A", "new B"), false)
	if !result.Capture.Admitted || result.Response["content"] != capturePage("old A", "old B", "new A", "new B") {
		t.Fatalf("external resize composition = %+v", result)
	}
}

func TestDifferentKeySamplingInversionTakesConservativeFallback(t *testing.T) {
	s, _, _ := captureServer(t, true)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan coordinator.PaneReadResult, 1)
	request := captureRequest(s, true)
	go func() {
		result, _ := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
			close(entered)
			<-release
			return herdr.PaneRead{Content: []byte(capturePage("H1", "H2", "C", "D", "E", "F", "G"))}, nil
		})
		done <- result
	}()
	<-entered
	secondSampled, secondRelease, secondDone := make(chan struct{}), make(chan struct{}), make(chan coordinator.PaneReadResult, 1)
	go func() {
		result, _ := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
			close(secondSampled)
			<-secondRelease
			return herdr.PaneRead{Content: []byte(capturePage("H1", "H2", "A", "B", "C", "D", "E"))}, nil
		})
		secondDone <- result
	}()
	<-secondSampled
	close(release)
	first := <-done
	close(secondRelease)
	second := <-secondDone
	if !first.Capture.Admitted || !second.Capture.Admitted || first.Capture.Sequence >= second.Capture.Sequence {
		t.Fatal("inversion fixture did not admit increasing start order")
	}
	want := capturePage("H1", "H2", "C", "D", "E", "F", "G", "H1", "H2", "A", "B", "C", "D", "E")
	if got := s.historyM.Content("pane-1", 0); got != want {
		t.Fatalf("reverse sampling deleted prior page: %q", got)
	}
}

func TestUnleasedNoScrollbackUsesReadPaneAndLegacyFixtureBytes(t *testing.T) {
	s, _, _ := captureServer(t, false)
	if physical, _ := s.physicalHistorySource(captureRequest(s, false).Agent); physical {
		t.Fatal("unleased pane chose physical history")
	}
	executeCapture(t, s, capturePage("H1", "H2", "A", "B", "C", "D", "E"), false)
	result := executeCapture(t, s, capturePage("H1", "H2", "C", "D", "E", "F", "G"), false)
	if result.Response["content"] != capturePage("H1", "H2", "A", "B", "C", "D", "E", "F", "G") {
		t.Fatalf("legacy composition = %q", result.Response["content"])
	}
}

func TestHistorySameTerminalDiscoveryKeepsArchive(t *testing.T) {
	s, _, _ := captureServer(t, true)
	executeCapture(t, s, capturePage("A", "B", "C", "D", "E"), true)
	agent, _ := s.state.Agent("pane-1")
	agent.SessionID = "discovered"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.syncHistoryPanes(s.state.Snapshot())
	result := executeCapture(t, s, capturePage("C", "D", "E", "F", "G"), true)
	if !result.Capture.Admitted || result.Response["content"] != capturePage("A", "B", "C", "D", "E", "F", "G") {
		t.Fatalf("discovery result = %+v", result)
	}
}

func TestInlineAndUnsupportedLeaseReadsRemainRaw(t *testing.T) {
	for _, mode := range []string{"inline", "unknown", "unsupported"} {
		s, _, _ := captureServer(t, true)
		if mode == "unsupported" {
			s.paneSizeM = panesize.NewManager(nil, nil)
		} else {
			agent, _ := s.state.Agent("pane-1")
			if mode == "inline" {
				agent.ScrollMaxOffset = 10
			} else {
				agent.ScrollMetricsKnown = false
			}
			s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
		}
		request := captureRequest(s, false)
		request.Resized = true
		raw := capturePage("A", "B")
		result, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
			return herdr.PaneRead{Content: []byte(raw)}, nil
		})
		if err != nil || result.Capture.Admitted || result.Response["content"] != raw {
			t.Fatalf("mode=%s result=%+v error=%v", mode, result, err)
		}
	}
}

func TestTextReadDoesNotEnterPhysicalArchive(t *testing.T) {
	s, _, _ := captureServer(t, true)
	request := captureRequest(s, false)
	request.Format = "text"
	result, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
	})
	if err != nil || result.Capture.Admitted {
		t.Fatalf("text result = %+v error=%v", result.Capture, err)
	}
	_, err = s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		return herdr.PaneRead{}, errors.New("read failure")
	})
	if err == nil {
		t.Fatal("Herdr error was hidden")
	}
}
