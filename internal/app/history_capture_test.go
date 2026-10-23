package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	"github.com/0cv/herdr-mobile-relay/internal/question"
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
	s.syncHistoryPanes()
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

type historyCaptureOutcome struct {
	result coordinator.PaneReadResult
	err    error
}

func startHistoryCapture(s *Server, raw string, physical bool) <-chan historyCaptureOutcome {
	done := make(chan historyCaptureOutcome, 1)
	go func() {
		result, err := s.executeHistoryRead(context.Background(), captureRequest(s, physical),
			func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
				return herdr.PaneRead{Content: []byte(raw)}, nil
			})
		done <- historyCaptureOutcome{result, err}
	}()
	return done
}

func awaitHistorySignal[T any](t *testing.T, signal <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-signal:
		return value
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	}
}

func awaitHistoryCaptureStart(t *testing.T, entered <-chan struct{}, done <-chan historyCaptureOutcome) {
	t.Helper()
	select {
	case <-entered:
	case outcome := <-done:
		t.Fatalf("capture returned before sampling barrier: error=%v capture=%+v", outcome.err, outcome.result.Capture)
	case <-time.After(3 * time.Second):
		t.Fatal("capture did not reach sampling barrier")
	}
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

func installPaneReadFixture(t *testing.T, s *Server, read func(context.Context) (string, error)) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
				for {
					var request struct {
						ID     string `json:"id"`
						Method string `json:"method"`
					}
					if decoder.Decode(&request) != nil {
						return
					}
					if request.Method != "pane.read" {
						t.Errorf("unexpected method %q", request.Method)
						return
					}
					content, err := read(ctx)
					if err != nil {
						return
					}
					if encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"type": "pane_read", "read": map[string]any{"text": content, "revision": 0, "truncated": false}}}) != nil {
						return
					}
				}
			}()
		}
	}()
	s.herdrC = herdr.NewClient("unused", socket)
	s.dispatcher = coordinator.NewDispatcher(s.herdrC, s.state, nil, s.logger)
	t.Cleanup(func() { cancel(); _ = s.herdrC.Close(); _ = listener.Close(); <-acceptDone; workers.Wait() })
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
	s.state.SetOnStatusTransition(s.enqueueHistoryTransition)
	s.state.CommitEvent("pane-1", "idle", 1)
	first := *s.historyPending["pane-1"]
	agent, _ := s.state.Agent("pane-1")
	agent.SessionID = "discovered"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.syncHistoryPanes()
	pending := s.historyPending["pane-1"]
	if pending == nil || pending.identity.generation == first.identity.generation || pending.queued != first.queued {
		t.Fatal("same terminal discovery did not rekey pending request")
	}
	agent, _ = s.state.Agent("pane-1")
	agent.TerminalID = "replacement"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.syncHistoryPanes()
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
	last := s.historyLast["pane-1"]
	s.captureHistoryTick(s.historyNow())
	waitHistoryCapture(t, s)
	if s.historyLast["pane-1"] != last {
		t.Fatal("deferral changed last capture time")
	}
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
	s.syncHistoryPanes()
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
	s.syncHistoryPanes()
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
			entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{}, 2)
			var enteredOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			s.dispatcher = coordinator.NewDispatcher(nil, s.state, nil, s.logger)
			var reads atomic.Int32
			options := coordinator.PaneReadOptions{FollowerJoined: func() { joined <- struct{}{} }, Execute: func(ctx context.Context, request coordinator.PaneReadRequest, _ func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error)) (coordinator.PaneReadResult, error) {
				reads.Add(1)
				return s.executeHistoryRead(ctx, request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
					enteredOnce.Do(func() { close(entered) })
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
			awaitHistorySignal(t, entered, "owner read start")
			if mode == "created after sample" {
				pendingDeparture(s, 10)
			}
			if mode == "newer request" {
				pendingDeparture(s, 11)
			}
			go read()
			go read()
			for range 2 {
				awaitHistorySignal(t, joined, "follower attachment")
			}
			unblock()
			var first coordinator.PaneReadCapture
			for index := range 3 {
				capture := awaitHistorySignal(t, results, "shared capture result")
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
	s, f, _ := captureServer(t, false)
	older := s.state.InventorySnapshot()
	agent, _ := s.state.Agent("pane-1")
	agent.TerminalID = "terminal-new"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.syncHistoryPanes()
	s.paneSizeM = panesize.NewManagerWith(&namedHistoryProvider{paneSizeFakes: f, terminalID: "terminal-new"}, f, f, s.historyNow, nil)
	result := executeCapture(t, s, capturePage("Q A", "Q B"), false)
	if !result.Capture.Admitted {
		t.Fatal("replacement Q was not admitted")
	}
	s.syncHistorySnapshot(older)
	if got := s.historyM.Content("pane-1", 0); got != capturePage("Q A", "Q B") {
		t.Fatalf("stale inventory removed Q: %q", got)
	}
}

func TestOlderHistorySnapshotPreservesNewerPendingDeparture(t *testing.T) {
	s, _, _ := captureServer(t, false)
	older := s.state.InventorySnapshot()
	s.state.SetOnStatusTransition(s.enqueueHistoryTransition)
	agent, _ := s.state.Agent("pane-1")
	agent.TerminalID = "terminal-new"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "idle", 1)
	pending := s.historyPending["pane-1"]
	if pending == nil || pending.revision <= older.Revision || pending.identity.terminalID != "terminal-new" {
		t.Fatalf("replacement departure was not queued after snapshot: %+v", pending)
	}
	want := *pending
	s.syncHistorySnapshot(older)
	if got := s.historyPending["pane-1"]; got == nil || *got != want {
		t.Fatalf("older snapshot changed newer pending departure: got=%+v want=%+v", got, want)
	}
	s.syncHistorySnapshot(s.state.InventorySnapshot())
	if got := s.historyPending["pane-1"]; got == nil || *got != want {
		t.Fatalf("replacement snapshot changed pending departure: got=%+v want=%+v", got, want)
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

type pausedHistoryResizeOps struct {
	panesize.TerminalOps
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (o *pausedHistoryResizeOps) Open(path string) (panesize.Terminal, error) {
	terminal, err := o.TerminalOps.Open(path)
	if err != nil {
		return nil, err
	}
	return pausedHistoryResizeTerminal{Terminal: terminal, ops: o}, nil
}

type pausedHistoryResizeTerminal struct {
	panesize.Terminal
	ops *pausedHistoryResizeOps
}

func (t pausedHistoryResizeTerminal) SetSize(size panesize.Winsize) error {
	err := t.Terminal.SetSize(size)
	t.ops.once.Do(func() { close(t.ops.entered); <-t.ops.release })
	return err
}

func TestHistoryWritersRejectUnsafeIntervals(t *testing.T) {
	for _, phase := range []string{"acquire settle", "restore settle", "crossed epoch", "resize in progress"} {
		for _, writer := range []string{"phone", "non-owner", "scheduled", "completion"} {
			t.Run(phase+"/"+writer, func(t *testing.T) {
				s, f, advance := captureServer(t, true)
				baseline := capturePage("retained A", "retained B")
				executeCapture(t, s, baseline, true)
				var releaseResize func()
				if phase == "resize in progress" {
					ops := &pausedHistoryResizeOps{TerminalOps: f, entered: make(chan struct{}), release: make(chan struct{})}
					s.paneSizeM = panesize.NewManagerWith(f, f, ops, s.historyNow, nil)
					done := make(chan error, 1)
					go func() { _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 70, 20); done <- err }()
					select {
					case <-ops.entered:
					case <-time.After(time.Second):
						t.Fatal("resize did not pause")
					}
					var once sync.Once
					releaseResize = func() {
						once.Do(func() {
							close(ops.release)
							if err := <-done; err != nil {
								t.Error(err)
							}
						})
					}
					t.Cleanup(releaseResize)
				} else if phase == "acquire settle" {
					if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 70, 20); err != nil {
						t.Fatal(err)
					}
				} else if phase == "restore settle" {
					if err := s.paneSizeM.Release(context.Background(), "owner", "pane-1", true); err != nil {
						t.Fatal(err)
					}
				}
				var contentReads atomic.Int32
				installPaneReadFixture(t, s, func(context.Context) (string, error) {
					contentReads.Add(1)
					if phase == "crossed epoch" {
						if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 70, 20); err != nil {
							return "", err
						}
						advance(4 * time.Second)
					}
					return capturePage("unsafe A", "unsafe B"), nil
				})
				switch writer {
				case "phone", "non-owner":
					message := map[string]any{"pane_id": "pane-1", "format": "ansi", "lines": 100}
					reader := "owner"
					if writer == "non-owner" {
						reader = "reader"
					}
					s.applyPaneReadLease(reader, message)
					response := s.readPaneResponse(context.Background(), message)
					if response["error"] != nil {
						t.Fatalf("display failed: %+v", response)
					}
					if writer == "non-owner" && response["viewport_only"] == true {
						t.Fatal("non-owner acquired viewport lease metadata")
					}
				case "scheduled":
					s.historyTasks = newLifecycleTasks(context.Background())
					t.Cleanup(s.historyTasks.Stop)
					s.scheduleHistoryCapture(context.Background(), "pane-1")
					waitHistoryCapture(t, s)
				case "completion":
					s.captureFinishedPane(context.Background(), "pane-1", "claude", "", "")
				}
				if releaseResize != nil {
					releaseResize()
				}
				if got := s.historyM.Content("pane-1", 0); got != baseline {
					t.Fatalf("unsafe %s read changed history: %q", writer, got)
				}
				wantReads := int32(1)
				if writer == "scheduled" && phase != "crossed epoch" {
					wantReads = 0
				}
				if contentReads.Load() != wantReads {
					t.Fatalf("content reads=%d want=%d", contentReads.Load(), wantReads)
				}
			})
		}
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

func TestHistoryReadRejectsScrollMetricCycle(t *testing.T) {
	for _, mode := range []string{"stable", "positive", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := captureServer(t, true)
			s.state.SetOnStatusTransition(s.enqueueHistoryTransition)
			s.state.CommitEvent("pane-1", "idle", 1)
			pending := *s.historyPending["pane-1"]
			request := captureRequest(s, true)
			result, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
				if mode != "stable" {
					agent, _ := s.state.Agent("pane-1")
					if mode == "positive" {
						agent.ScrollMaxOffset = 10
					} else {
						agent.ScrollMetricsKnown = false
					}
					s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
					agent.ScrollMetricsKnown, agent.ScrollMaxOffset = true, 0
					s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
				}
				return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := s.state.ContentRevision("pane-1"); got != request.ContentRevision {
				t.Fatalf("metric fixture changed content revision: got=%d want=%d", got, request.ContentRevision)
			}
			if mode == "stable" {
				if !result.Capture.Admitted || s.historyPending["pane-1"] != nil || s.historyAcknowledgements != 1 {
					t.Fatal("stable read was not admitted and acknowledged")
				}
				return
			}
			if result.Capture.Admitted {
				t.Fatal("scroll metric cycle was admitted")
			}
			if got := s.historyPending["pane-1"]; got == nil || got.sequence != pending.sequence || s.historyAcknowledgements != 0 {
				t.Fatalf("metric cycle acknowledged pending request: %+v", got)
			}
			if result := executeCapture(t, s, capturePage("A", "B"), true); !result.Capture.Admitted || s.historyPending["pane-1"] != nil {
				t.Fatal("following stable read was not admitted and acknowledged")
			}
		})
	}
}

func TestHistoryBookkeepingDoesNotWaitForPublication(t *testing.T) {
	for _, operation := range []string{"status transition", "non-Claude read"} {
		t.Run(operation, func(t *testing.T) {
			s, _, _ := captureServer(t, false)
			s.state.SetOnStatusTransition(s.enqueueHistoryTransition)
			if operation == "non-Claude read" {
				agent, _ := s.state.Agent("pane-1")
				agent.Agent = "codex"
				s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
				s.syncHistoryPanes()
			}
			s.historyCaptureMu.Lock()
			var once sync.Once
			release := func() { once.Do(s.historyCaptureMu.Unlock) }
			t.Cleanup(release)
			done := make(chan error, 1)
			go func() {
				if operation == "status transition" {
					s.state.CommitEvent("pane-1", "idle", 1)
					done <- nil
					return
				}
				result, err := s.executeHistoryRead(context.Background(), captureRequest(s, false), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
					return herdr.PaneRead{Content: []byte("codex raw")}, nil
				})
				if err == nil && (result.Response["content"] != "codex raw" || result.Capture.Sequence != 0) {
					err = fmt.Errorf("non-Claude history bookkeeping: %+v", result)
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				release()
				awaitHistorySignal(t, done, "blocked bookkeeping cleanup")
				t.Fatal("history bookkeeping waited for publication mutex")
			}
			release()
		})
	}
}

func TestAsyncHistoryCaptureReturnsClosedError(t *testing.T) {
	s, _, _ := captureServer(t, true)
	s.historyPendingClosed = true
	outcome := awaitHistorySignal(t, startHistoryCapture(s, capturePage("A", "B"), true), "closed capture result")
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("closed capture error=%v", outcome.err)
	}
}

func TestFinishedReadPublishesOldEpochAfterLaterResize(t *testing.T) {
	s, _, advance := captureServer(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.historyReadFinishedObserver = func(historyReadToken) { enteredOnce.Do(func() { close(entered) }); <-release }
	done := startHistoryCapture(s, capturePage("old A", "old B"), true)
	awaitHistoryCaptureStart(t, entered, done)
	_, _, _ = s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 70, 20)
	unblock()
	if outcome := awaitHistorySignal(t, done, "finished old epoch"); outcome.err != nil || !outcome.result.Capture.Admitted {
		t.Fatalf("finished old epoch was rejected: error=%v capture=%+v", outcome.err, outcome.result.Capture)
	}
	s.historyReadFinishedObserver = nil
	advance(4 * time.Second)
	executeCapture(t, s, capturePage("new A", "new B"), true)
	if got := s.historyM.Content("pane-1", 0); got != capturePage("old A", "old B", "new A", "new B") {
		t.Fatalf("epoch composition = %q", got)
	}
}

func TestSingleFlightOwnerAdmitsOnceForSeveralFollowers(t *testing.T) {
	s, _, _ := captureServer(t, true)
	pendingDeparture(s, 10)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var reads, admissions, mutations atomic.Int32
	s.historyAdmissionObserver = func(_ historyReadToken, admission history.SnapshotAdmission) {
		if admission.Admitted {
			admissions.Add(1)
		}
		if admission.ContentChanged || admission.MetadataChanged {
			mutations.Add(1)
		}
	}
	installPaneReadFixture(t, s, func(ctx context.Context) (string, error) {
		if reads.Add(1) != 1 {
			return "", errors.New("follower started another underlying read")
		}
		close(entered)
		select {
		case <-release:
			return capturePage("owner A", "owner B"), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	physical, revision := s.physicalHistorySource(captureRequest(s, true).Agent)
	joined := make(chan struct{}, 5)
	options := coordinator.PaneReadOptions{Physical: physical, SourceRevision: revision, Execute: s.executeHistoryRead,
		FollowerJoined: func() { joined <- struct{}{} }}
	type readResult struct {
		response map[string]any
		capture  coordinator.PaneReadCapture
	}
	results := make(chan readResult, 6)
	read := func() {
		response, capture := s.dispatcher.HandleReadPaneWithOptions(context.Background(), map[string]any{"pane_id": "pane-1", "lines": 1000, "format": "ansi"}, options)
		results <- readResult{response, capture}
	}
	go read()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not start")
	}
	for range 5 {
		go read()
	}
	for range 5 {
		awaitHistorySignal(t, joined, "follower attachment")
	}
	unblock()
	var owner coordinator.PaneReadCapture
	for index := range 6 {
		result := awaitHistorySignal(t, results, "shared read result")
		if result.response["error"] != nil {
			t.Fatalf("shared read failed: %+v", result.response)
		}
		if index == 0 {
			owner = result.capture
		} else if result.capture != owner {
			t.Fatal("follower changed capture identity")
		}
	}
	if reads.Load() != 1 || admissions.Load() != 1 || mutations.Load() != 1 || s.historyAcknowledgements != 1 || s.historySequence != 1 || s.historyArchived["pane-1"] != 1 {
		t.Fatalf("reads=%d admissions=%d mutations=%d acknowledgements=%d allocated=%d archived=%d", reads.Load(), admissions.Load(), mutations.Load(), s.historyAcknowledgements, s.historySequence, s.historyArchived["pane-1"])
	}
	if s.historyPending["pane-1"] != nil || s.historyM.Content("pane-1", 0) != capturePage("owner A", "owner B") {
		t.Fatal("shared admission did not retain page and acknowledge request")
	}
	if counter := s.historyCounters["pane-1"]; counter == nil || counter.admissions != 1 || counter.boundary != 1 {
		t.Fatalf("shared followers double-counted class admissions: %+v", counter)
	}
}

func TestPaneReadZeroRevisionUsesDistinctUnderlyingSequences(t *testing.T) {
	s, _, _ := captureServer(t, true)
	var reads atomic.Int32
	installPaneReadFixture(t, s, func(context.Context) (string, error) {
		if reads.Add(1) == 1 {
			return capturePage("old A", "old B"), nil
		}
		return capturePage("new A", "new B"), nil
	})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.historyReadFinishedObserver = func(token historyReadToken) {
		if token.sequence == 1 {
			close(entered)
			<-release
		}
	}
	execute := func() (coordinator.PaneReadResult, error) {
		return s.executeHistoryRead(context.Background(), captureRequest(s, true), func(ctx context.Context, request coordinator.PaneReadRequest) (herdr.PaneRead, error) {
			return s.herdrC.ReadPaneRecent(ctx, request.PaneID, history.PhysicalCaptureLines, "ansi")
		})
	}
	type outcome struct {
		result coordinator.PaneReadResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := execute(); done <- outcome{result, err} }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first zero-revision read did not finish sampling")
	}
	newer, err := execute()
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	older := <-done
	if older.err != nil || older.result.Capture.Sequence != 1 || newer.Capture.Sequence != 2 || older.result.Capture.Admitted || !newer.Capture.Admitted {
		t.Fatalf("zero-revision captures: old=%+v error=%v new=%+v", older.result.Capture, older.err, newer.Capture)
	}
	if reads.Load() != 2 || s.historyArchived["pane-1"] != 2 || s.historyM.Content("pane-1", 0) != capturePage("new A", "new B") {
		t.Fatal("placeholder revision overrode underlying-read ordering")
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
	joined := make(chan struct{}, 1)
	options := coordinator.PaneReadOptions{Physical: true, SourceRevision: revision, FollowerJoined: func() { joined <- struct{}{} },
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
	go func() {
		response, capture := s.dispatcher.HandleReadPaneWithOptions(context.Background(), message(), options)
		followerDone <- callerResult{response, capture}
	}()
	awaitHistorySignal(t, joined, "follower attachment")
	release()
	owner := awaitHistorySignal(t, ownerDone, "owner response")
	follower := awaitHistorySignal(t, followerDone, "follower response")
	if ownerCalls.Load() != 1 || owner.capture.Sequence != 1 || follower.capture != owner.capture || follower.response["content"] != pageP {
		t.Fatalf("owner=%+v follower=%+v underlying calls=%d", owner.capture, follower.capture, ownerCalls.Load())
	}
	s.historyCaptureMu.Lock()
	sequence, archived := s.historySequence, s.historyArchived["pane-1"]
	s.historyCaptureMu.Unlock()
	if sequence != 2 || archived != 2 || follower.capture.PendingSequence != 0 {
		t.Fatalf("follower changed admission bookkeeping: sequence=%d archived=%d pending=%d", sequence, archived, follower.capture.PendingSequence)
	}
	// The old assertion required an unsupported whole-page P prefix. A2 replaces
	// this two-row body in place; the follower still cannot replace Q with P.
	if got := s.historyM.Content("pane-1", 0); got != pageQ {
		t.Fatalf("follower replaced Q with P: %q", got)
	}
}

func TestNewerArchivedReadRejectsOlderFinishedCompletion(t *testing.T) {
	s, _, _ := captureServer(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.historyReadFinishedObserver = func(token historyReadToken) {
		if token.sequence == 1 {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
	}
	done := startHistoryCapture(s, capturePage("old A", "old B"), true)
	awaitHistoryCaptureStart(t, entered, done)
	newer := executeCapture(t, s, capturePage("new A", "new B"), true)
	unblock()
	older := awaitHistorySignal(t, done, "older finished completion")
	if older.err != nil || !newer.Capture.Admitted || older.result.Capture.Admitted || older.result.Capture.Sequence >= newer.Capture.Sequence {
		t.Fatalf("older=%+v error=%v newer=%+v", older.result.Capture, older.err, newer.Capture)
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
			s.syncHistoryPanes()
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

func TestScrollbackLegacyHistoryMatchesAcrossPhoneLeaseCycle(t *testing.T) {
	s, _, advance := captureServer(t, false)
	agent, _ := s.state.Agent("pane-1")
	agent.ScrollMaxOffset = 10
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	base := history.NewManager(t.TempDir())
	check := func(stage string, body ...string) coordinator.PaneReadResult {
		t.Helper()
		if physical, _ := s.physicalHistorySource(captureRequest(s, false).Agent); physical {
			t.Fatalf("%s selected physical history for a scrollback pane", stage)
		}
		page := capturePage(body...)
		result := executeCapture(t, s, page, false)
		want := base.Merge("pane-1", page)
		if !result.Capture.Admitted || result.Response["content"] != want || s.historyM.Content("pane-1", 0) != want {
			t.Fatalf("%s: admitted=%v content=%q stored=%q, base Merge=%q", stage, result.Capture.Admitted,
				result.Response["content"], s.historyM.Content("pane-1", 0), want)
		}
		return result
	}
	before := check("before lease", "A", "B", "C", "D")
	crossed, err := s.executeHistoryRead(context.Background(), captureRequest(s, false), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20); err != nil {
			return herdr.PaneRead{}, err
		}
		return herdr.PaneRead{Content: []byte(capturePage("A", "B", "C", "D", "E"))}, nil
	})
	if err != nil || crossed.Capture.Admitted || s.historyM.Content("pane-1", 0) != capturePage("A", "B", "C", "D") {
		t.Fatalf("read crossing lease acquisition: admitted=%v error=%v stored=%q", crossed.Capture.Admitted, err,
			s.historyM.Content("pane-1", 0))
	}
	advance(4 * time.Second)
	leased := check("after lease", "A", "B", "C", "D", "E")
	if leased.Capture.Snapshot.LeaseRevision == before.Capture.Snapshot.LeaseRevision ||
		leased.Capture.Snapshot.GeometryEpoch == before.Capture.Snapshot.GeometryEpoch {
		t.Fatalf("lease did not change revision and epoch: before=%+v after=%+v", before.Capture.Snapshot, leased.Capture.Snapshot)
	}
	if err := s.paneSizeM.Release(context.Background(), "owner", "pane-1", false); err != nil {
		t.Fatal(err)
	}
	check("release grace", "A", "B", "C", "D", "E")
	advance(panesize.ReleaseGrace)
	s.paneSizeM.SweepExpired(context.Background())
	advance(4 * time.Second)
	restored := check("after restore cleanup", "A", "B", "C", "D", "E", "F")
	if restored.Capture.Snapshot.LeaseRevision == leased.Capture.Snapshot.LeaseRevision {
		t.Fatal("restore cleanup did not change the lease revision")
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

func TestLegacyHistoryUnknownSampleKeepsEpoch(t *testing.T) {
	s, f, _ := captureServer(t, false)
	page := capturePage("A", "B", "C")
	executeCapture(t, s, page, false)
	f.mu.Lock()
	columns := f.size.Columns
	f.size.Columns = 0
	f.mu.Unlock()
	executeCapture(t, s, page, false)
	f.mu.Lock()
	f.size.Columns = columns
	f.mu.Unlock()
	result := executeCapture(t, s, page, false)
	if got := s.historyM.Content("pane-1", 0); got != page || result.Response["content"] != page {
		t.Fatalf("failed geometry sample duplicated legacy page: %q", got)
	}
}

func TestScrollbackMetricGrowthDuringReadRemainsEligible(t *testing.T) {
	s, _, _ := captureServer(t, false)
	agent, _ := s.state.Agent("pane-1")
	agent.ScrollMaxOffset = 10
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	result, err := s.executeHistoryRead(context.Background(), captureRequest(s, false), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		agent.ScrollMaxOffset = 20
		s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
		return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
	})
	if err != nil || !result.Capture.Admitted {
		t.Fatalf("scrollback growth rejected capture: admitted=%v error=%v", result.Capture.Admitted, err)
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
	// Use a supported forward archive, not the old divergent-page fallback.
	executeCapture(t, s, capturePage("old A", "old B", "keep C", "keep D"), true)
	acknowledged := &paneWatchFrame{resizeSettling: true}
	if !paneWatchNeedsFrameRead("same", "same", acknowledged, "claude") {
		t.Fatal("unchanged probe skipped settled full read")
	}
	result := executeCapture(t, s, capturePage("keep C", "keep D", "new A", "new B"), true)
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

// Replaces TestDifferentKeySamplingInversionTakesConservativeFallback's frozen Q prefix.
func TestDifferentKeySamplingInversionHoldsPushedRows(t *testing.T) {
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
	want := capturePage("H1", "H2", "A", "B", "C", "D", "E")
	if err := s.historyM.SaveAll(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.CacheDir, "claude-history", "pane-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var retained history.PaneState
	if err := json.Unmarshal(data, &retained); err != nil {
		t.Fatal(err)
	}
	if len(retained.Held) != 1 || !slices.Equal(retained.Held[0].Rows, []string{"F", "G"}) {
		t.Fatalf("inversion held rows=%+v", retained.Held)
	}
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

func TestNonClaudeInventoryDepartureForgetsGeometry(t *testing.T) {
	for _, departure := range []string{"removed", "terminal changed"} {
		t.Run(departure, func(t *testing.T) {
			s, _, advance := captureServer(t, false)
			agent, _ := s.state.Agent("pane-1")
			agent.Agent = "codex"
			s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
			s.syncHistorySnapshot(s.state.InventorySnapshot())
			older := s.state.InventorySnapshot()
			if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20); err != nil {
				t.Fatal(err)
			}
			advance(panesize.HistorySettleWindow)
			read, err := s.paneSizeM.BeginHistoryRead(context.Background(), "pane-1", "terminal-1")
			if err != nil {
				t.Fatal(err)
			}
			original, err := read.Finish()
			if err != nil {
				t.Fatal(err)
			}
			if departure == "removed" {
				s.state.CommitInventory(nil, s.state.RevisionCounter())
			} else {
				agent.TerminalID = "terminal-new"
				s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
			}
			s.syncHistorySnapshot(s.state.InventorySnapshot())
			s.syncHistorySnapshot(older)
			read, err = s.paneSizeM.BeginHistoryRead(context.Background(), "pane-1", "terminal-1")
			if err != nil {
				t.Fatal(err)
			}
			recreated, err := read.Finish()
			if err != nil {
				t.Fatal(err)
			}
			if recreated.Epoch <= original.Epoch {
				t.Fatalf("departed non-Claude pane retained geometry: old epoch=%d new epoch=%d", original.Epoch, recreated.Epoch)
			}
		})
	}
}

func TestHistorySameTerminalDiscoveryKeepsArchive(t *testing.T) {
	s, _, _ := captureServer(t, true)
	executeCapture(t, s, capturePage("A", "B", "C", "D", "E"), true)
	agent, _ := s.state.Agent("pane-1")
	agent.SessionID = "discovered"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.syncHistoryPanes()
	result := executeCapture(t, s, capturePage("C", "D", "E", "F", "G"), true)
	if !result.Capture.Admitted || result.Response["content"] != capturePage("A", "B", "C", "D", "E", "F", "G") {
		t.Fatalf("discovery result = %+v", result)
	}
}

func TestViewportReadResizeFallbackSamplesAfterContent(t *testing.T) {
	for _, format := range []string{"text", "ansi"} {
		t.Run(format, func(t *testing.T) {
			s, _, _ := captureServer(t, true)
			agent, _ := s.state.Agent("pane-1")
			agent.ScrollMaxOffset = 10
			s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
			if s.paneSizeM.ResizedWithin("pane-1", paneResizeSettleWindow) {
				t.Fatal("fixture started inside the resize settle window")
			}
			raw := capturePage("visible A", "visible B")
			installPaneReadFixture(t, s, func(ctx context.Context) (string, error) {
				_, _, err := s.paneSizeM.Acquire(ctx, "owner", "pane-1", 70, 20)
				return raw, err
			})
			response := s.readPaneResponse(context.Background(), map[string]any{
				"pane_id": "pane-1", "format": format, "lines": 100, "terminal_columns": 70,
			})
			if response["error"] != nil || response["content"] != raw || response["viewport_only"] != true || response["resize_settling"] != true {
				t.Fatalf("read missed resize during content capture: %+v", response)
			}
		})
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

func TestUnleasedTextReadLeavesLegacyANSIArchiveUnchanged(t *testing.T) {
	s, _, _ := captureServer(t, false)
	retained := capturePage("retained A", "retained B")
	if result := executeCapture(t, s, retained, false); !result.Capture.Admitted {
		t.Fatal("legacy ANSI fixture was not admitted")
	}
	raw := capturePage("visible C", "visible D")
	installPaneReadFixture(t, s, func(context.Context) (string, error) { return raw, nil })
	response := s.readPaneResponse(context.Background(), map[string]any{"pane_id": "pane-1", "format": "text", "lines": 100})
	if response["error"] != nil || response["content"] != raw || response["format"] != "text" {
		t.Fatalf("text read did not return raw visible page: %+v", response)
	}
	if got := s.historyM.Content("pane-1", 0); got != retained {
		t.Fatalf("text read changed ANSI archive: %q", got)
	}
	response = s.readPaneResponse(context.Background(), map[string]any{"pane_id": "pane-1", "format": "ansi", "lines": 100})
	want := capturePage("retained A", "retained B", "visible C", "visible D")
	if response["error"] != nil || response["content"] != want {
		t.Fatalf("ANSI read did not compose retained rows: %+v", response)
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

// A held-majority freeze emits one reason log without the supplied page rows.
func TestHistoryMatcherFreezeLogsSafeReasonOnce(t *testing.T) {
	s, _, _ := captureServer(t, true)
	var output bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&output, nil))
	old := []string{"private-row-a", "private-row-b", "private-row-c", "private-row-d", "private-row-e", "private-row-f", "private-row-g", "private-row-h"}
	executeCapture(t, s, capturePage(old...), true)
	executeCapture(t, s, capturePage("new-a", "new-b", "new-c", "new-d"), true)
	text := output.String()
	if strings.Count(text, "physical history discontinuity") != 1 || !strings.Contains(text, "reason=held_majority") || !strings.Contains(text, "held_nonblank=8") || !strings.Contains(text, "old_nonblank=8") {
		t.Fatalf("missing one safe freeze log: %s", text)
	}
	for _, row := range append(old, "new-a", "new-b", "new-c", "new-d", "f1", "f2", "f3", "f4", "f5", "f6") {
		if strings.Contains(text, row) {
			t.Fatal("freeze log exposed page row text")
		}
	}
}

func TestHistoryMatcherHeldEvictionLogsCountsWithoutRows(t *testing.T) {
	s, _, _ := captureServer(t, true)
	var output bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&output, nil))
	// Rewriting this four-row slot holds its old values without a page freeze.
	old := []string{"h1", "h2", "private-slot-a", "private-slot-b", "private-slot-c", "private-slot-d", "t1", "t2"}
	executeCapture(t, s, capturePage(old...), true)
	for frame := range 502 {
		body := []string{"h1", "h2", fmt.Sprintf("private-slot-%d-a", frame), fmt.Sprintf("private-slot-%d-b", frame), fmt.Sprintf("private-slot-%d-c", frame), fmt.Sprintf("private-slot-%d-d", frame), "t1", "t2"}
		executeCapture(t, s, capturePage(body...), true)
	}
	text := output.String()
	if !strings.Contains(text, "reason=held_capacity") || !strings.Contains(text, "held_evicted_rows=4") {
		t.Fatal("held eviction did not log a physical count")
	}
	if strings.Contains(text, "private-slot-") {
		t.Fatal("held eviction log exposed page row text")
	}
	for _, row := range []string{"h1", "h2", "t1", "t2", "f1", "f2", "f3", "f4", "f5", "f6"} {
		if strings.Contains(text, row) {
			t.Fatalf("eviction log exposed page row %q", row)
		}
	}
}

func TestHistoryMinuteCountersFlushQuietPaneAndPrune(t *testing.T) {
	s, _, advance := captureServer(t, true)
	var output bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&output, nil))
	executeCapture(t, s, capturePage("A", "B"), true)
	executeCapture(t, s, capturePage("A", "B"), true)
	counter := s.historyCounters["pane-1"]
	if counter.admissions != 2 || counter.boundary != 1 || counter.stationary != 1 || counter.freezes != 0 {
		t.Fatalf("class counters=%+v", counter)
	}
	advance(59 * time.Second)
	s.flushHistoryCounters(s.historyNow())
	if output.Len() != 0 {
		t.Fatal("minute counters flushed before deadline")
	}
	advance(time.Second)
	s.flushHistoryCounters(s.historyNow())
	if strings.Count(output.String(), "physical history minute") != 1 || !strings.Contains(output.String(), "admissions=2") || !strings.Contains(output.String(), "boundary=1") {
		t.Fatalf("minute output=%s", output.String())
	}
	if s.historyCounters["pane-1"].admissions != 0 {
		t.Fatal("minute counters did not reset")
	}
	advance(time.Minute)
	s.flushHistoryCounters(s.historyNow())
	if strings.Count(output.String(), "physical history minute") != 2 {
		t.Fatal("quiet pane did not flush on the next minute")
	}
	s.state.CommitInventory(nil, s.state.RevisionCounter())
	s.syncHistoryPanes()
	if len(s.historyCounters) != 0 {
		t.Fatal("pane disappearance retained counters")
	}
}

func TestHistoryCounterFieldsAggregateWithoutDirtyingHistory(t *testing.T) {
	s, _, advance := captureServer(t, true)
	var output bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&output, nil))
	token := historyReadToken{paneID: "pane-1"}
	s.historyCaptureMu.Lock()
	s.recordHistoryAdmissionLocked(token, history.SnapshotAdmission{Class: "reverse", MatchedRows: 4, RetiredRows: 2, ReplacedRows: 1, HeldAddedRows: 3, HeldEvictedRows: 4, HeldExpiredRows: 5, HeldTotalRows: 17, RetractionCount: 1, RetractedRows: 2, DPPairs: 9})
	s.recordHistoryAdmissionLocked(token, history.SnapshotAdmission{Class: "freeze", Reason: "pair_budget", DPPairs: 65537, HeldTotalRows: 17})
	s.historyCaptureMu.Unlock()
	advance(time.Minute)
	s.flushHistoryCounters(s.historyNow())
	for _, field := range []string{"reverse=1", "freezes=1", "pair_budget=1", "matched_rows=4", "retired_rows=2", "replaced_rows=1", "held_added_rows=3", "held_evicted_rows=4", "held_expired_rows=5", "held_total_rows=17", "retraction_events=1", "retracted_rows=2", "dp_pairs=65546", "max_dp_pairs=65537"} {
		if !strings.Contains(output.String(), field) {
			t.Errorf("minute report lacks %s", field)
		}
	}
}

type historyLockProbeHandler struct {
	server   *Server
	acquired chan bool
}

func (h *historyLockProbeHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *historyLockProbeHandler) Handle(context.Context, slog.Record) error {
	ok := h.server.historyCaptureMu.TryLock()
	if ok {
		h.server.historyCaptureMu.Unlock()
	}
	h.acquired <- ok
	return nil
}
func (h *historyLockProbeHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *historyLockProbeHandler) WithGroup(string) slog.Handler      { return h }

func TestHistoryLogsRunOutsideCaptureLock(t *testing.T) {
	s, _, advance := captureServer(t, true)
	probe := &historyLockProbeHandler{server: s, acquired: make(chan bool, 4)}
	s.logger = slog.New(probe)
	executeCapture(t, s, capturePage("a", "b", "c", "d", "e", "f", "g", "h"), true)
	executeCapture(t, s, capturePage("new-a", "new-b", "new-c", "new-d"), true)
	if !awaitHistorySignal(t, probe.acquired, "log lock probe") {
		t.Fatal("freeze logging held capture mutex")
	}
	advance(time.Minute)
	s.flushHistoryCounters(s.historyNow())
	if !awaitHistorySignal(t, probe.acquired, "log lock probe") {
		t.Fatal("minute logging held capture mutex")
	}
}

func TestHistoryArchiveExpiryAndPairBudgetLogsSafeCounts(t *testing.T) {
	s, _, _ := captureServer(t, true)
	var output bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&output, nil))
	token := historyReadToken{paneID: "pane-1", sequence: 7, leaseRevision: 3}
	s.logHistoryAdmission(token, history.SnapshotAdmission{Class: "freeze", Reason: "pair_budget", OldNonblank: 257, HeldNonblank: 257, DPPairs: 65537, HeldExpiredRows: 2})
	text := output.String()
	for _, field := range []string{"sequence=7", "source=recent", "lease_revision=3", "reason=pair_budget", "held_share=1", "dp_pairs=65537", "reason=archive_pin_expiry", "held_expired_rows=2"} {
		if !strings.Contains(text, field) {
			t.Errorf("safe diagnostic lacks %s", field)
		}
	}
	if strings.Contains(text, "held_majority") {
		t.Fatal("pair budget was mislabeled as held majority")
	}
}

func TestHistoryCounterTerminalReplacementPrunesOldMinute(t *testing.T) {
	s, _, _ := captureServer(t, true)
	executeCapture(t, s, capturePage("A", "B"), true)
	agent, _ := s.state.Agent("pane-1")
	agent.TerminalID = "replacement"
	s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
	s.syncHistoryPanes()
	if len(s.historyCounters) != 0 {
		t.Fatal("terminal replacement retained the old minute")
	}
}

type namedHistoryProvider struct {
	*paneSizeFakes
	terminalID string
}

func (p *namedHistoryProvider) PaneGet(_ context.Context, paneID string) (*herdr.Pane, error) {
	return &herdr.Pane{ID: paneID, TerminalID: p.terminalID}, nil
}

type replacedHistoryProvider struct{ *paneSizeFakes }

func (p *replacedHistoryProvider) PaneGet(_ context.Context, paneID string) (*herdr.Pane, error) {
	return &herdr.Pane{ID: paneID, TerminalID: "terminal-2"}, nil
}
func TestHistoryIdentityErrorCannotAdmitOrPreview(t *testing.T) {
	s, f, _ := captureServer(t, false)
	executeCapture(t, s, capturePage("archive A", "archive B", "current A", "current B"), false)
	executeCapture(t, s, capturePage("current A", "current B"), false)
	before := s.historyM.Content("pane-1", 0)
	s.paneSizeM = panesize.NewManagerWith(&replacedHistoryProvider{f}, f, f, s.historyNow, nil)
	result := executeCapture(t, s, capturePage("replacement A", "replacement B"), false)
	if result.Capture.Admitted || result.Composed || result.Response["history_retry"] == true || s.historyM.Content("pane-1", 0) != before {
		t.Fatalf("identity error admitted or previewed: admitted=%v composed=%v retry=%v", result.Capture.Admitted, result.Composed, result.Response["history_retry"])
	}
}

func TestHistoryLayoutOnlyPageKeepsPreviewArchive(t *testing.T) {
	s, _, _ := captureServer(t, true)
	executeCapture(t, s, capturePage("archive A", "archive B", "current A", "current B"), true)
	executeCapture(t, s, capturePage("current A", "current B", "new A", "new B"), true)
	raw := "partial dialog\n6. Chat about this"
	if !question.LayoutHint(raw) || question.Parse(raw, "claude") != nil {
		t.Fatal("fixture is not layout-only")
	}
	result := executeCapture(t, s, raw, true)
	if result.Capture.Admitted || !result.Composed || result.Response["content"] != "archive A\narchive B\n"+raw {
		t.Fatalf("layout-only preview=%+v", result)
	}
}
func TestHistorySameTerminalGenerationLagKeepsPreviewArchive(t *testing.T) {
	s, _, _ := captureServer(t, true)
	executeCapture(t, s, capturePage("archive A", "archive B", "current A", "current B"), true)
	executeCapture(t, s, capturePage("current A", "current B", "new A", "new B"), true)
	s.historyCaptureMu.Lock()
	identity := s.historyActive["pane-1"]
	identity.generation--
	s.historyActive["pane-1"] = identity
	s.historyCaptureMu.Unlock()
	result := executeCapture(t, s, capturePage("current A", "current B", "new A", "new B"), true)
	if result.Capture.Admitted || !result.Composed || !strings.Contains(result.Response["content"].(string), "archive A") {
		t.Fatal("same-terminal generation lag lost preview archive")
	}
}

const rejectedCustomAnswerPage = `
←  ☒ Existing work  ☒ Work type  ☒ Time budget  ☒ Top outcome  ✔ Submit  →

Roughly how much time do you want to put into this over the weekend?

❯ 1. A few focused hours
     One short session, keep scope tight
  2. Half a day
     One solid block
  3. Most of the weekend ✔
     Multiple sessions
  4. Just exploring / no fixed budget
     Casual
  5. Private typed answer
  6. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`

func TestRejectedPaneReadDoesNotRecordCustomAnswer(t *testing.T) {
	s, _, _ := captureServer(t, false)
	if parsed := question.Parse(rejectedCustomAnswerPage, "claude"); parsed == nil || parsed.Other.Text != "Private typed answer" {
		t.Fatal("custom-answer fixture did not parse")
	}
	installPaneReadFixture(t, s, func(context.Context) (string, error) {
		agent, _ := s.state.Agent("pane-1")
		agent.TerminalID = "replacement"
		s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
		return rejectedCustomAnswerPage, nil
	})
	response := s.readPaneResponse(context.Background(), map[string]any{"pane_id": "pane-1", "format": "ansi", "lines": 100})
	if response["error"] == nil {
		t.Fatal("fixture read was not rejected")
	}
	if len(s.state.CustomAnswers("pane-1")) != 0 {
		t.Fatal("rejected response recorded a custom answer")
	}
}

type delayedBudgetHistoryProvider struct{ *paneSizeFakes }

func (p *delayedBudgetHistoryProvider) PaneGet(ctx context.Context, paneID string) (*herdr.Pane, error) {
	timer := time.NewTimer(30 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return p.paneSizeFakes.PaneGet(ctx, paneID)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestBackgroundHistoryContentGetsIndependentBudget(t *testing.T) {
	s, f, _ := captureServer(t, false)
	s.paneSizeM = panesize.NewManagerWith(&delayedBudgetHistoryProvider{f}, f, f, s.historyNow, nil)
	request := captureRequest(s, false)
	request.Background = true
	_, err := s.executeHistoryRead(context.Background(), request, func(ctx context.Context, _ coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 2900*time.Millisecond {
			t.Errorf("content budget after validation=%v deadline=%v", time.Until(deadline), ok)
		}
		return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestPhysicalGeometryFailureDoesNotRequestPollRetry(t *testing.T) {
	s, f, _ := captureServer(t, true)
	executeCapture(t, s, capturePage("A", "B"), true)
	f.mu.Lock()
	f.size.Columns = 0
	f.mu.Unlock()
	result := executeCapture(t, s, capturePage("A", "B"), true)
	if result.Capture.Admitted || result.Response["history_retry"] == true {
		t.Fatal("failed geometry sample requested unbounded poll retry")
	}
}

func TestNonClaudeBackgroundReadHasContentDeadline(t *testing.T) {
	s, _, _ := captureServer(t, false)
	request := captureRequest(s, false)
	request.Background = true
	request.Agent.Agent = "codex"
	_, err := s.executeHistoryRead(context.Background(), request, func(ctx context.Context, _ coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second || time.Until(deadline) < 2900*time.Millisecond {
			t.Errorf("non-Claude background deadline=%v remaining=%s", ok, time.Until(deadline))
		}
		return herdr.PaneRead{Content: []byte("answer")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type boundedHistoryProvider struct {
	*paneSizeFakes
	t *testing.T
}

func (p *boundedHistoryProvider) PaneGet(ctx context.Context, paneID string) (*herdr.Pane, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 3*time.Second || time.Until(deadline) < 2*time.Second {
		p.t.Errorf("geometry validation lacks existing three-second bound: deadline=%v remaining=%s", ok, time.Until(deadline))
	}
	return p.paneSizeFakes.PaneGet(ctx, paneID)
}
func TestClaudeGeometryValidationKeepsPaneSizeOperationBound(t *testing.T) {
	s, f, _ := captureServer(t, false)
	s.paneSizeM = panesize.NewManagerWith(&boundedHistoryProvider{f, t}, f, f, s.historyNow, nil)
	request := captureRequest(s, false)
	request.Background = true
	_, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHistoryJitteredTicksKeepOneCapturePerSlot(t *testing.T) {
	s, _, advance := captureServer(t, false)
	var reads atomic.Int32
	installPaneReadFixture(t, s, func(context.Context) (string, error) { reads.Add(1); return capturePage("A", "B"), nil })
	s.historyTasks = newLifecycleTasks(context.Background())
	t.Cleanup(s.historyTasks.Stop)
	base := s.historyNow()
	for i, jitter := range []time.Duration{300 * time.Millisecond, 100 * time.Millisecond, 400 * time.Millisecond, 50 * time.Millisecond} {
		tick := base.Add(time.Duration(i) * history.CaptureInterval)
		advance(tick.Add(jitter).Sub(s.historyNow()))
		s.captureHistoryTick(tick)
		waitHistoryCapture(t, s)
		s.captureHistoryTick(tick)
		waitHistoryCapture(t, s)
		if got := reads.Load(); got != int32(i+1) {
			t.Fatalf("jittered tick %d reads=%d, want %d without skips or bursts", i, got, i+1)
		}
	}
}
func TestPendingReadExpiryEmitsPaneDiagnostic(t *testing.T) {
	s, _, advance := captureServer(t, false)
	var output bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&output, nil))
	pendingDeparture(s, 10)
	advance(trailingCaptureLifetime)
	executeCapture(t, s, capturePage("private page A", "private page B"), false)
	if !strings.Contains(output.String(), "trailing pane capture expired") || !strings.Contains(output.String(), "pane_id=pane-1") {
		t.Fatal("read-path expiry omitted bounded diagnostic")
	}
	if strings.Contains(output.String(), "private page") {
		t.Fatal("expiry log exposed content")
	}
}
func TestHistoryStatusRevisionsPruneWithSnapshotProtection(t *testing.T) {
	s, _, _ := captureServer(t, false)
	snapshot := s.state.InventorySnapshot()
	snapshot.Agents = nil
	s.historyPendingMu.Lock()
	s.historyStatusRevision["past"] = snapshot.Revision
	s.historyStatusRevision["future"] = snapshot.Revision + 1
	s.historyPendingMu.Unlock()
	s.syncHistorySnapshot(snapshot)
	if _, ok := s.historyStatusRevision["past"]; ok {
		t.Fatal("absent pane revision was not pruned")
	}
	if _, ok := s.historyStatusRevision["future"]; !ok {
		t.Fatal("older snapshot pruned a future accepted status revision")
	}
}

func TestLegacyOnlyHistoryDoesNotCreatePhysicalMinuteLog(t *testing.T) {
	s, _, advance := captureServer(t, false)
	var output bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&output, nil))
	executeCapture(t, s, capturePage("A", "B"), false)
	advance(time.Minute)
	s.flushHistoryCounters(s.historyNow())
	if len(s.historyCounters) != 0 || strings.Contains(output.String(), "physical history minute") {
		t.Fatal("legacy-only admission emitted physical telemetry")
	}
}

func TestLimitedPhysicalQuestionKeepsCanonicalInteraction(t *testing.T) {
	s, _, _ := captureServer(t, true)
	expected := question.Parse(rejectedCustomAnswerPage, "claude")
	if expected == nil {
		t.Fatal("question fixture did not parse")
	}
	installPaneReadFixture(t, s, func(context.Context) (string, error) { return rejectedCustomAnswerPage, nil })
	response := s.readPaneResponse(context.Background(), map[string]any{"pane_id": "pane-1", "format": "ansi", "lines": 4})
	actual, _ := response["interaction"].(*question.Interaction)
	if actual == nil {
		t.Fatalf("limited question lost canonical interaction: attention=%v layout=%v", response["attention_kind"], response["question_layout"])
	}
	if actual.ID != expected.ID || actual.Question != expected.Question || len(actual.Options) != len(expected.Options) {
		t.Fatalf("limited question changed canonical interaction: got=%+v want=%+v", actual, expected)
	}
	if response["attention_kind"] != question.AttentionQuestion || response["question_layout"] != true {
		t.Fatal("question metadata contradicts the canonical interaction")
	}
	if response["content"] == rejectedCustomAnswerPage || question.Parse(response["content"].(string), "claude") != nil {
		t.Fatal("fixture did not clip away question context")
	}
}

func TestSharedPaneQuestionReadersOwnNestedInteraction(t *testing.T) {
	s, _, _ := captureServer(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	installPaneReadFixture(t, s, func(context.Context) (string, error) {
		if reads.Add(1) == 1 {
			close(entered)
			<-release
		}
		return rejectedCustomAnswerPage, nil
	})
	results := make(chan map[string]any, 2)
	joined := make(chan struct{}, 1)
	physical, revision := s.physicalHistorySource(captureRequest(s, true).Agent)
	options := coordinator.PaneReadOptions{Physical: physical, SourceRevision: revision, Execute: s.executeHistoryRead, FollowerJoined: func() { joined <- struct{}{} }}
	read := func() {
		response, _ := s.dispatcher.HandleReadPaneWithOptions(context.Background(), map[string]any{"pane_id": "pane-1", "format": "ansi", "lines": 4}, options)
		s.prepareAcceptedPaneInteraction("pane-1", response)
		results <- response
	}
	go read()
	awaitHistorySignal(t, entered, "question owner read")
	go read()
	awaitHistorySignal(t, joined, "question follower join")
	close(release)
	first := awaitHistorySignal(t, results, "first question reader")
	second := awaitHistorySignal(t, results, "second question reader")
	a, _ := first["interaction"].(*question.Interaction)
	b, _ := second["interaction"].(*question.Interaction)
	if a == nil || b == nil {
		t.Fatal("shared clipped readers lost interaction")
	}
	if a == b || &a.Options[0] == &b.Options[0] {
		t.Fatal("readers share mutable interaction storage")
	}
	a.Options[0].Label = "changed first reader"
	a.Other.Text = "changed note"
	if b.Options[0].Label == a.Options[0].Label || b.Other.Text == a.Other.Text {
		t.Fatal("first reader mutated second interaction")
	}
}
func TestPaneInteractionCloneOwnsReviewSummaries(t *testing.T) {
	original := &question.Interaction{Options: []question.Option{{Label: "Submit", Summary: []question.SummaryEntry{{Question: "Name one outcome", Answer: "custom answer"}}}}}
	a, b := clonePaneInteraction(original), clonePaneInteraction(original)
	question.FillCustomAnswers(a, map[string]string{"name one outcome": "first reader answer"})
	a.Options[0].Summary[0].Answer = "first reader changed"
	if b.Options[0].Summary[0].Answer != "custom answer" || original.Options[0].Summary[0].Answer != "custom answer" {
		t.Fatal("review summary clone shares nested state")
	}
}

func TestLateHistoryTickCannotBurstWithPromptTick(t *testing.T) {
	s, _, advance := captureServer(t, false)
	var reads atomic.Int32
	installPaneReadFixture(t, s, func(context.Context) (string, error) { reads.Add(1); return capturePage("A", "B"), nil })
	s.historyTasks = newLifecycleTasks(context.Background())
	t.Cleanup(s.historyTasks.Stop)
	base := s.historyNow()
	advance(3900 * time.Millisecond)
	s.captureHistoryTick(base)
	waitHistoryCapture(t, s)
	advance(100 * time.Millisecond)
	s.captureHistoryTick(base.Add(history.CaptureInterval))
	waitHistoryCapture(t, s)
	s.scheduleHistoryCapture(context.Background(), "pane-1")
	waitHistoryCapture(t, s)
	if got := reads.Load(); got != 1 {
		t.Fatalf("late and prompt tick started %d captures 100ms apart, want one", got)
	}
	advance(3 * time.Second)
	s.scheduleHistoryCapture(context.Background(), "pane-1")
	waitHistoryCapture(t, s)
	if got := reads.Load(); got != 2 {
		t.Fatalf("capture did not resume after actual-time floor: reads=%d", got)
	}
}
