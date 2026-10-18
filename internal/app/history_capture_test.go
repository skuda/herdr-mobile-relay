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
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
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
	s.historyNow = clock
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

func pendingDeparture(s *Server, revision int64) {
	s.enqueueHistoryTransition(coordinator.StatusTransition{PaneID: "pane-1", TerminalID: "terminal-1", Generation: s.state.Generation("pane-1"), OldStatus: "working", NewStatus: "idle", Revision: revision})
}

func TestTrailingCaptureSeesShortWorkingCycleBetweenTicks(t *testing.T) {
	s, _, _ := captureServer(t, false)
	s.state.SetOnStatusTransition(s.enqueueHistoryTransition)
	s.state.CommitEvent("pane-1", "idle", 1)
	s.state.CommitEvent("pane-1", "working", 2)
	s.state.CommitEvent("pane-1", "idle", 3)
	if pending := s.historyPending["pane-1"]; pending == nil {
		t.Fatal("between-tick cycle lost trailing request")
	}
	result := executeCapture(t, s, capturePage("final A", "final B"), false)
	if !result.Capture.Admitted || s.historyPending["pane-1"] != nil {
		t.Fatal("admitted trailing page did not clear request")
	}
}

func TestTrailingCaptureCoalescesAndCannotClearNewerRequest(t *testing.T) {
	s, _, _ := captureServer(t, false)
	pendingDeparture(s, 10)
	first := *s.historyPending["pane-1"]
	result, err := s.executeHistoryRead(context.Background(), captureRequest(s, false), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		pendingDeparture(s, 11)
		return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
	})
	if err != nil || !result.Capture.Admitted {
		t.Fatalf("capture=%+v error=%v", result.Capture, err)
	}
	pending := s.historyPending["pane-1"]
	if pending == nil || pending.sequence <= first.sequence || pending.queued != first.queued || pending.attempts != 1 {
		t.Fatalf("newer coalesced request=%+v first=%+v", pending, first)
	}
	pendingDeparture(s, 10)
	if s.historyPending["pane-1"].sequence != pending.sequence {
		t.Fatal("old accepted revision replaced current request")
	}
	executeCapture(t, s, capturePage("A", "B"), false)
	if s.historyPending["pane-1"] != nil {
		t.Fatal("unchanged admitted capture did not acknowledge")
	}
}

func TestTrailingCaptureRekeysSameTerminalGenerationAndCancelsReplacement(t *testing.T) {
	s, _, _ := captureServer(t, false)
	pendingDeparture(s, 10)
	first := *s.historyPending["pane-1"]
	agent, _ := s.state.Agent("pane-1")
	agent.SessionID = "discovered"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.syncHistoryPanes(s.state.Snapshot())
	pending := s.historyPending["pane-1"]
	if pending == nil || pending.identity.generation == first.identity.generation || pending.queued != first.queued {
		t.Fatal("same terminal discovery did not rekey pending request")
	}
	agent, _ = s.state.Agent("pane-1")
	agent.TerminalID = "replacement"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.syncHistoryPanes(s.state.Snapshot())
	if s.historyPending["pane-1"] != nil {
		t.Fatal("replacement retained pending request")
	}
	pendingDeparture(s, 11)
	if s.historyPending["pane-1"] != nil {
		t.Fatal("delayed old-identity event created replacement request")
	}
}

func TestTrailingCaptureSurvivesInflightIntervalSettleAndReadError(t *testing.T) {
	s, _, advance := captureServer(t, true)
	pendingDeparture(s, 10)
	root := t.TempDir()
	record, bin := filepath.Join(root, "calls"), filepath.Join(root, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'call\\n' >> \""+record+"\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.herdrC = herdr.NewClient(bin, filepath.Join(root, "missing.sock"))
	t.Cleanup(func() { _ = s.herdrC.Close() })
	s.historyTasks = newLifecycleTasks(context.Background())
	t.Cleanup(s.historyTasks.Stop)
	s.historyInflight["pane-1"] = true
	s.captureHistoryTick(s.historyNow())
	delete(s.historyInflight, "pane-1")
	s.historyLast["pane-1"] = s.historyNow()
	s.captureHistoryTick(s.historyNow())
	advance(4 * time.Second)
	_, _, _ = s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 70, 20)
	s.captureHistoryTick(s.historyNow())
	if s.historyPending["pane-1"].attempts != 0 {
		t.Fatal("deferral consumed a read attempt")
	}
	advance(4 * time.Second)
	s.captureHistoryTick(s.historyNow())
	waitHistoryCapture(t, s)
	if s.historyPending["pane-1"].attempts != 1 {
		t.Fatal("failed read did not consume one attempt")
	}
	result := executeCapture(t, s, capturePage("final A", "final B"), true)
	if !result.Capture.Admitted || s.historyPending["pane-1"] != nil {
		t.Fatal("retry did not admit trailing page")
	}
}

func waitHistoryCapture(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		s.historyCaptureMu.Lock()
		inflight := s.historyInflight["pane-1"]
		s.historyCaptureMu.Unlock()
		if !inflight {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("history capture did not finish")
		}
		runtime.Gosched()
	}
}

func TestTrailingCaptureUsesPaneWideSourceAndSurvivesRestoreBoundary(t *testing.T) {
	s, _, advance := captureServer(t, true)
	pendingDeparture(s, 10)
	_ = s.paneSizeM.Release(context.Background(), "owner", "pane-1", true)
	s.captureHistoryTick(s.historyNow())
	if s.historyPending["pane-1"] == nil || s.historyPending["pane-1"].attempts != 0 {
		t.Fatal("restore deferral lost request")
	}
	advance(4 * time.Second)
	result := executeCapture(t, s, capturePage("restored A", "restored B"), false)
	if !result.Capture.Admitted || s.historyPending["pane-1"] != nil || result.Capture.Snapshot.ReadSource != history.SourceLegacy {
		t.Fatal("restored request did not use legacy source")
	}
}

type slowHistoryProvider struct {
	*paneSizeFakes
	entered chan struct{}
	once    sync.Once
}

func (p *slowHistoryProvider) PaneGet(ctx context.Context, paneID string) (*herdr.Pane, error) {
	if paneID == "slow" {
		p.once.Do(func() { close(p.entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.paneSizeFakes.PaneGet(ctx, paneID)
}

func TestSlowHistoryGeometryDoesNotBlockOtherPaneScheduling(t *testing.T) {
	s, f, _ := captureServer(t, false)
	agent, _ := s.state.Agent("pane-1")
	slow := *agent
	slow.PaneID = "slow"
	s.state.CommitInventory([]*coordinator.AgentState{agent, &slow}, s.state.RevisionCounter())
	s.syncHistoryPanes(s.state.Snapshot())
	provider := &slowHistoryProvider{paneSizeFakes: f, entered: make(chan struct{})}
	s.paneSizeM = panesize.NewManagerWith(provider, f, f, s.historyNow, nil)
	root := t.TempDir()
	bin := filepath.Join(root, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'fast A\\nfast B'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.herdrC = herdr.NewClient(bin, filepath.Join(root, "missing.sock"))
	t.Cleanup(func() { _ = s.herdrC.Close() })
	s.historyTasks = newLifecycleTasks(context.Background())
	t.Cleanup(s.historyTasks.Stop)
	done := make(chan struct{})
	go func() {
		s.scheduleHistoryCapture(context.Background(), "slow")
		s.scheduleHistoryCapture(context.Background(), "pane-1")
		close(done)
	}()
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("slow preflight did not enter")
	}
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("slow preflight blocked other scheduling")
	}
	waitHistoryCapture(t, s)
	if got := s.historyM.Content("pane-1", 0); got != "fast A\nfast B" {
		t.Fatalf("fast pane capture=%q", got)
	}
}

func TestReplacementDepartureBeforeReconciliationIsRetained(t *testing.T) {
	s, _, _ := captureServer(t, false)
	s.state.SetOnStatusTransition(s.enqueueHistoryTransition)
	agent, _ := s.state.Agent("pane-1")
	agent.TerminalID = "replacement"
	agent.Status = "working"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "idle", 1)
	pending := s.historyPending["pane-1"]
	if pending == nil || pending.identity.terminalID != "replacement" {
		t.Fatalf("new terminal departure lost: %+v", pending)
	}
	s.syncHistoryPanes(s.state.Snapshot())
	if s.historyPending["pane-1"] == nil {
		t.Fatal("reconciliation removed newer terminal request")
	}
}

func TestReversedStatusDeliveryRetainsEarlierDeparture(t *testing.T) {
	s, _, _ := captureServer(t, false)
	s.enqueueHistoryTransition(coordinator.StatusTransition{PaneID: "pane-1", TerminalID: "terminal-1", OldStatus: "idle", NewStatus: "working", Revision: 11})
	pendingDeparture(s, 10)
	if s.historyPending["pane-1"] == nil {
		t.Fatal("later non-departure delivery invalidated pending departure")
	}
}

func TestTrailingCaptureBoundsAttemptsAndRequestLifetime(t *testing.T) {
	for _, expire := range []string{"attempts", "lifetime"} {
		s, _, _ := captureServer(t, false)
		now := time.Unix(10000, 0)
		s.historyNow = func() time.Time { return now }
		pendingDeparture(s, 10)
		if expire == "attempts" {
			for range 5 {
				_, _ = s.executeHistoryRead(context.Background(), captureRequest(s, false), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
					return herdr.PaneRead{}, errors.New("read failed")
				})
			}
		} else {
			now = now.Add(60 * time.Second)
		}
		s.captureHistoryTick(now)
		if s.historyPending["pane-1"] != nil {
			t.Fatalf("%s did not expire request", expire)
		}
	}
}

func TestNativeConversationDoesNotSuppressTrailingTerminalCapture(t *testing.T) {
	s, _, _ := captureServer(t, false)
	home := t.TempDir()
	const sessionID = "123e4567-e89b-12d3-a456-426614174322"
	writeClaudeTranscriptAnswering(t, filepath.Join(home, ".claude", "projects", "-work-old", sessionID+".jsonl"), "work", "native answer")
	s.conversationM = conversation.NewReader(home)
	pendingDeparture(s, 10)
	if got := s.captureFinishedPane(context.Background(), "pane-1", "claude", "/work/old", sessionID); got != "native answer" {
		t.Fatalf("native completion=%q", got)
	}
	if s.historyPending["pane-1"] == nil {
		t.Fatal("native response cleared terminal request")
	}
	executeCapture(t, s, capturePage("terminal A", "terminal B"), false)
	if s.historyPending["pane-1"] != nil || s.historyM.Content("pane-1", 0) != capturePage("terminal A", "terminal B") {
		t.Fatal("terminal trailing capture did not complete")
	}
}

func TestSharedReadPendingAcknowledgement(t *testing.T) {
	for _, mode := range []string{"present", "created after sample", "newer request", "rejected", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := captureServer(t, false)
			if mode != "created after sample" {
				pendingDeparture(s, 10)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			s.dispatcher = coordinator.NewDispatcher(nil, s.state, nil, s.logger)
			var reads atomic.Int32
			options := coordinator.PaneReadOptions{Execute: func(ctx context.Context, request coordinator.PaneReadRequest, _ func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error)) (coordinator.PaneReadResult, error) {
				reads.Add(1)
				return s.executeHistoryRead(ctx, request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
					close(entered)
					<-release
					if mode == "cancelled" {
						return herdr.PaneRead{}, context.Canceled
					}
					raw := capturePage("A", "B")
					if mode == "rejected" {
						raw = ""
					}
					return herdr.PaneRead{Content: []byte(raw)}, nil
				})
			}}
			results := make(chan coordinator.PaneReadCapture, 3)
			read := func() {
				_, capture := s.dispatcher.HandleReadPaneWithOptions(context.Background(), map[string]any{"pane_id": "pane-1", "format": "ansi", "lines": 100}, options)
				results <- capture
			}
			go read()
			<-entered
			if mode == "created after sample" {
				pendingDeparture(s, 10)
			}
			if mode == "newer request" {
				pendingDeparture(s, 11)
			}
			go read()
			go read()
			time.Sleep(20 * time.Millisecond)
			close(release)
			var first coordinator.PaneReadCapture
			for index := range 3 {
				capture := <-results
				if index == 0 {
					first = capture
				} else if capture != first {
					t.Fatal("followers changed underlying capture identity")
				}
			}
			if reads.Load() != 1 {
				t.Fatalf("underlying reads=%d", reads.Load())
			}
			pending := s.historyPending["pane-1"]
			if (pending == nil) != (mode == "present") {
				t.Fatalf("mode=%s pending=%+v", mode, pending)
			}
			if pending != nil && pending.attempts > 1 {
				t.Fatal("followers consumed attempts")
			}
			wantAcknowledgements := uint64(0)
			if mode == "present" {
				wantAcknowledgements = 1
			}
			if s.historyAcknowledgements != wantAcknowledgements {
				t.Fatalf("acknowledgements=%d want=%d", s.historyAcknowledgements, wantAcknowledgements)
			}
			if mode == "newer request" && pending.sequence == first.PendingSequence {
				t.Fatal("newer request retained sampled sequence")
			}
			if mode == "created after sample" && first.PendingSequence != 0 {
				t.Fatal("follower attached a request created after sampling")
			}
		})
	}
}

func TestReadBeforeInventoryPreservesCachedHistory(t *testing.T) {
	s := testServerWithCacheDir(t.TempDir())
	s.historyM.Merge("cached", capturePage("cached A", "cached B"))
	if err := s.historyM.SaveAll(); err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, nil, s.logger)
	_ = s.readPaneResponse(context.Background(), map[string]any{})
	matches, err := filepath.Glob(filepath.Join(s.cfg.CacheDir, "claude-history", "*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("pre-inventory read removed persisted history: files=%v error=%v", matches, err)
	}
}

func TestStaleHistoryInventoryCannotDiscardReplacementPage(t *testing.T) {
	s, _, _ := captureServer(t, false)
	older := s.state.InventorySnapshot()
	agent, _ := s.state.Agent("pane-1")
	agent.TerminalID = "terminal-new"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.syncHistoryPanes(s.state.Snapshot())
	result := executeCapture(t, s, capturePage("Q A", "Q B"), false)
	if !result.Capture.Admitted {
		t.Fatal("replacement Q was not admitted")
	}
	s.syncHistorySnapshot(older)
	if got := s.historyM.Content("pane-1", 0); got != capturePage("Q A", "Q B") {
		t.Fatalf("stale inventory removed Q: %q", got)
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

func TestSingleFlightFollowerCannotRelabelOlderSnapshot(t *testing.T) {
	s, _, _ := captureServer(t, true)
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, nil, s.logger)
	pageP, pageQ := capturePage("P A", "P B"), capturePage("Q A", "Q B")
	ownerFinished, releaseOwner := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOwner) }) }
	t.Cleanup(release)
	var ownerCalls atomic.Int32
	on, revision := s.physicalHistorySource(captureRequest(s, true).Agent)
	if !on {
		t.Fatal("fixture does not have physical source")
	}
	options := coordinator.PaneReadOptions{Physical: true, SourceRevision: revision,
		Execute: func(ctx context.Context, request coordinator.PaneReadRequest, _ func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error)) (coordinator.PaneReadResult, error) {
			if request.Lines != 1000 {
				t.Errorf("owner capture count=%d", request.Lines)
			}
			if ownerCalls.Add(1) != 1 {
				return coordinator.PaneReadResult{}, errors.New("follower started an independent read")
			}
			result, err := s.executeHistoryRead(ctx, request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
				return herdr.PaneRead{Content: []byte(pageP)}, nil
			})
			close(ownerFinished)
			<-releaseOwner
			return result, err
		}}
	type callerResult struct {
		response map[string]any
		capture  coordinator.PaneReadCapture
	}
	ownerDone, followerDone := make(chan callerResult, 1), make(chan callerResult, 1)
	message := func() map[string]any { return map[string]any{"pane_id": "pane-1", "lines": 1000, "format": "ansi"} }
	go func() {
		response, capture := s.dispatcher.HandleReadPaneWithOptions(context.Background(), message(), options)
		ownerDone <- callerResult{response, capture}
	}()
	select {
	case <-ownerFinished:
	case <-time.After(time.Second):
		t.Fatal("owner did not pause before flight removal")
	}
	newer := executeCapture(t, s, pageQ, true)
	if !newer.Capture.Admitted || newer.Capture.Sequence != 2 {
		t.Fatalf("background C capture=%+v", newer.Capture)
	}
	followerStarted := make(chan struct{})
	go func() {
		close(followerStarted)
		response, capture := s.dispatcher.HandleReadPaneWithOptions(context.Background(), message(), options)
		followerDone <- callerResult{response, capture}
	}()
	<-followerStarted
	select {
	case <-followerDone:
		t.Fatal("follower returned before owner release")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	owner, follower := <-ownerDone, <-followerDone
	if ownerCalls.Load() != 1 || owner.capture.Sequence != 1 || follower.capture != owner.capture || follower.response["content"] != pageP {
		t.Fatalf("owner=%+v follower=%+v underlying calls=%d", owner.capture, follower.capture, ownerCalls.Load())
	}
	s.historyCaptureMu.Lock()
	sequence, archived := s.historySequence, s.historyArchived["pane-1"]
	s.historyCaptureMu.Unlock()
	if sequence != 2 || archived != 2 || follower.capture.PendingSequence != 0 {
		t.Fatalf("follower changed admission bookkeeping: sequence=%d archived=%d pending=%d", sequence, archived, follower.capture.PendingSequence)
	}
	if got := s.historyM.Content("pane-1", 0); got != "P A\nP B\n"+pageQ {
		t.Fatalf("follower replaced Q with P: %q", got)
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
	root := t.TempDir()
	page, record, bin := filepath.Join(root, "page"), filepath.Join(root, "calls"), filepath.Join(root, "herdr")
	if err := os.WriteFile(page, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+record+"\"\ncat \""+page+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.herdrC = herdr.NewClient(bin, filepath.Join(root, "missing.sock"))
	t.Cleanup(func() { _ = s.herdrC.Close() })
	s.dispatcher = coordinator.NewDispatcher(s.herdrC, s.state, nil, s.logger)
	for range 4 {
		response := s.readPaneResponse(context.Background(), map[string]any{"pane_id": "pane-1", "lines": 100, "format": "ansi"})
		if response["error"] != nil || strings.Count(response["content"].(string), "\n")+1 != 100 || response["truncated"] != true {
			t.Fatalf("100-row display response = %+v", response)
		}
		if got := s.historyM.Content("pane-1", 0); got != raw {
			t.Fatal("display row clipping changed canonical stored page")
		}
		background, err := s.captureTerminalHistory(context.Background(), "pane-1")
		if err != nil || background != raw {
			t.Fatalf("background canonical page=%q error=%v", background, err)
		}
		if got := s.historyM.Content("pane-1", 0); got != raw {
			t.Fatal("alternating captures grew archive")
		}
	}
	calls, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "--lines 1000 --source recent --format ansi") != 8 {
		t.Fatalf("noncanonical capture requests: %s", calls)
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

func TestFailedLegacyGeometrySampleRetainsRestoreSettleFence(t *testing.T) {
	s, f, advance := captureServer(t, true)
	executeCapture(t, s, capturePage("old A", "old B"), true)
	_ = s.paneSizeM.Release(context.Background(), "owner", "pane-1", true)
	f.mu.Lock()
	f.size.Columns = 0
	f.mu.Unlock()
	if result := executeCapture(t, s, capturePage("settling A", "settling B"), false); result.Capture.Admitted {
		t.Fatal("failed sample bypassed retained restore settling")
	}
	advance(4 * time.Second)
	if result := executeCapture(t, s, capturePage("legacy A", "legacy B"), false); !result.Capture.Admitted {
		t.Fatal("outside-settle legacy read required known geometry")
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
