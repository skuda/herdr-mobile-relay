package app

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
)

func BenchmarkBoundaryCaptureLock(b *testing.B) {
	s := &Server{historyCounters: make(map[string]*historyMinuteCounters), historyNow: time.Now}
	m := history.NewManager(b.TempDir())
	oldContext := history.SnapshotContext{TerminalID: "terminal", ReadSource: history.SourceRecent, Columns: 150, Rows: 87}
	nextContext := oldContext
	nextContext.Columns = 62
	rows := []string{"Amber lanterns guide quiet ships beside distant rocky shores.", "Patient keepers tend bright lamps through heavy winter storms."}
	wrapped := append([]string{"prefix not shared"}, rows...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		oldContext.TerminalID = strconv.Itoa(i)
		nextContext.TerminalID = oldContext.TerminalID
		s.historyCaptureMu.Lock()
		_, _, d := m.AdmitSnapshot("p", capturePage(wrapped...), oldContext, 0)
		s.recordHistoryAdmissionLocked(historyReadToken{paneID: "p"}, d)
		s.historyCaptureMu.Unlock()
		s.historyCaptureMu.Lock()
		_, _, d = m.AdmitSnapshot("p", capturePage(strings.Join(rows, " ")), nextContext, 0)
		if d.BoundaryReconciledRows != 2 {
			b.Fatalf("benchmark boundary refused: %+v", d)
		}
		s.recordHistoryAdmissionLocked(historyReadToken{paneID: "p"}, d)
		s.historyCaptureMu.Unlock()
	}
}

func TestHistoryBoundaryCountersIncludeLegacyDestination(t *testing.T) {
	s, _, advance := captureServer(t, true)
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))
	body := []string{"Amber lanterns guide quiet ships beside distant rocky shores.", "Patient keepers tend bright lamps through heavy winter storms."}
	executeCapture(t, s, capturePage(body...), true)
	if err := s.paneSizeM.Release(context.Background(), "owner", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	advance(4 * time.Second)
	result := executeCapture(t, s, capturePage(strings.Join(body, " ")), false)
	counter := s.historyCounters["pane-1"]
	if !result.Capture.Admitted || counter.boundaryReconciledRows != 2 || counter.boundaryRefused != 0 || counter.admissions != 1 {
		t.Fatalf("legacy boundary counters=%+v capture=%+v", counter, result.Capture)
	}
	s.flushHistoryCounters(s.historyNow().Add(time.Minute))
	for _, field := range []string{"boundary_reconciled_rows=2", "boundary_refused=0", "boundary_seam_kept=0"} {
		if !strings.Contains(logs.String(), field) {
			t.Errorf("missing boundary count %s", field)
		}
	}
	if strings.Contains(logs.String(), "Amber") || strings.Contains(logs.String(), "Patient") {
		t.Fatal("boundary log contains transcript text")
	}
}

func TestHistoryBoundaryFollowersCountOnce(t *testing.T) {
	s, _, advance := captureServer(t, true)
	body := []string{"Amber lanterns guide quiet ships beside distant rocky shores.", "Patient keepers tend bright lamps through heavy winter storms."}
	executeCapture(t, s, capturePage(body...), true)
	if err := s.paneSizeM.Release(context.Background(), "owner", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	advance(4 * time.Second)
	entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{}, 3)
	installPaneReadFixture(t, s, func(context.Context) (string, error) {
		close(entered)
		<-release
		return capturePage(strings.Join(body, " ")), nil
	})
	options := coordinator.PaneReadOptions{Execute: s.executeHistoryRead, FollowerJoined: func() { joined <- struct{}{} }}
	_, revision := s.physicalHistorySource(captureRequest(s, false).Agent)
	options.SourceRevision = revision
	done := make(chan coordinator.PaneReadCapture, 4)
	read := func() {
		_, capture := s.dispatcher.HandleReadPaneWithOptions(context.Background(), map[string]any{"pane_id": "pane-1", "lines": 1000, "format": "ansi"}, options)
		done <- capture
	}
	go read()
	<-entered
	for range 3 {
		go read()
	}
	for range 3 {
		awaitHistorySignal(t, joined, "boundary follower")
	}
	close(release)
	for range 4 {
		if capture := awaitHistorySignal(t, done, "boundary result"); !capture.Admitted {
			t.Error("shared boundary not admitted")
		}
	}
	if counter := s.historyCounters["pane-1"]; counter.boundaryReconciledRows != 2 {
		t.Errorf("followers double-counted boundary: %+v", counter)
	}
}

func TestHistoryBoundaryPhysicalSeamAndRefusalCounters(t *testing.T) {
	for _, kind := range []string{"seam", "refusal"} {
		t.Run(kind, func(t *testing.T) {
			s, _, advance := captureServer(t, false)
			rows := []string{"Amber lanterns guide quiet ships beside distant rocky shores.", "Patient keepers tend bright lamps through heavy winter storms.", "Morning sunlight reveals calm waters beyond the sheltered harbor."}
			old := append([]string{"earlier words " + rows[0]}, rows[1:]...)
			if kind == "refusal" {
				old = []string{"short row", "second short row"}
			}
			executeCapture(t, s, capturePage(old...), false)
			if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20); err != nil {
				t.Fatal(err)
			}
			advance(4 * time.Second)
			result := executeCapture(t, s, capturePage(rows...), true)
			counter := s.historyCounters["pane-1"]
			if !result.Capture.Admitted {
				t.Fatal("boundary not admitted")
			}
			if kind == "seam" && (counter.boundaryReconciledRows != 2 || counter.boundarySeamKept != 1 || counter.boundaryRefused != 0) {
				t.Errorf("physical seam counts=%+v", counter)
			}
			if kind == "refusal" && (counter.boundaryRefused != 1 || counter.boundaryReconciledRows != 0) {
				t.Errorf("physical refusal counts=%+v", counter)
			}
		})
	}
}

func TestHistoryLegacyExpiryUpdatesHeldMinuteCounters(t *testing.T) {
	s, _, advance := captureServer(t, true)
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))
	for _, body := range [][]string{
		{"R", "A", "B", "C", "S1", "S2"},
		{"A", "B", "C", "S1'", "S2'", "N"},
		{"R", "A", "B", "C", "N"},
	} {
		executeCapture(t, s, capturePage(body...), true)
	}
	counter := s.historyCounters["pane-1"]
	held := counter.currentHeldRows
	if held != 2 {
		t.Fatalf("physical transition held=%d, want 2", held)
	}
	admissions := counter.admissions
	if err := s.paneSizeM.Release(context.Background(), "owner", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	advance(panesize.HistorySettleWindow + time.Second)
	executeCapture(t, s, capturePage("legacy seed"), false)
	rows := make([]string, history.MaxLines+1)
	rows[0] = "legacy seed"
	for i := 1; i < len(rows); i++ {
		rows[i] = "legacy-row-" + strconv.Itoa(i)
	}
	var expired int
	s.historyAdmissionObserver = func(_ historyReadToken, d history.SnapshotAdmission) { expired += d.HeldExpiredRows }
	if !executeCapture(t, s, capturePage(rows...), false).Capture.Admitted || expired != held {
		t.Fatalf("legacy trim did not expire fixture holds: expired=%d held=%d", expired, held)
	}
	if counter.admissions != admissions {
		t.Error("legacy reads incremented physical admission counts")
	}
	s.flushHistoryCounters(s.historyNow().Add(time.Minute))
	var minute string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "physical history minute") {
			minute = line
		}
	}
	for _, field := range []string{"held_total_rows=0", "held_expired_rows=" + strconv.Itoa(held), "admissions=" + strconv.Itoa(admissions)} {
		if !strings.Contains(minute, field) {
			t.Errorf("minute counter missing %s: %s", field, minute)
		}
	}
}

func TestHistoryBoundaryCounterAggregationDoesNotCountRoutineLegacy(t *testing.T) {
	s, _, _ := captureServer(t, false)
	token := historyReadToken{paneID: "pane-1"}
	if !executeCapture(t, s, capturePage("plain legacy"), false).Capture.Admitted || s.historyCounters["pane-1"] != nil {
		t.Fatal("plain legacy admission created a minute counter")
	}
	s.recordHistoryAdmissionLocked(token, history.SnapshotAdmission{Class: "legacy", BoundaryRefused: 1, BoundarySeamKept: 1,
		HeldExpiredRows: 2, HeldEvictedRows: 3, HeldTotalRows: 4, DPPairs: 5, MatchedRows: 6})
	counter := s.historyCounters["pane-1"]
	if counter.admissions != 0 || counter.legacy != 0 || counter.dpPairs != 0 || counter.matchedRows != 0 || counter.boundaryRefused != 1 || counter.boundarySeamKept != 1 {
		t.Errorf("legacy boundary counted physical matcher work: %+v", counter)
	}
	if counter.heldExpiredRows != 2 || counter.heldEvictedRows != 3 || counter.currentHeldRows != 4 {
		t.Errorf("legacy held counts not updated: %+v", counter)
	}
	executeCapture(t, s, capturePage("plain legacy"), false)
	if counter.currentHeldRows != 0 || counter.admissions != 0 {
		t.Error("routine legacy read did not refresh the existing gauge without admission counts")
	}
}
