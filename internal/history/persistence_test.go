package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func persistedState(t *testing.T, m *Manager, pane string) PaneState {
	t.Helper()
	data, err := os.ReadFile(m.stateFile(pane))
	if err != nil {
		t.Fatal(err)
	}
	var state PaneState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestDirtyHistoryFlushesAtSaveDeadlineWithoutNewFrames(t *testing.T) {
	m := NewManager(t.TempDir())
	now := time.Unix(10000, 0)
	m.now = func() time.Time { return now }
	m.Merge("p", snapshotPage("A", "B"))
	if err := m.FlushDue(now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	m.Merge("p", snapshotPage("B", "C"))
	due := m.states["p"].saveDue
	now = now.Add(2 * time.Second)
	m.Merge("p", snapshotPage("B", "C"))
	if m.states["p"].saveDue != due {
		t.Fatal("unchanged read postponed dirty deadline")
	}
	if err := m.FlushDue(due); err != nil {
		t.Fatal(err)
	}
	if got := persistedState(t, m, "p"); strings.Join(got.History, "\n") != "A\nB\nC" || m.states["p"].dirty {
		t.Fatalf("saved state=%+v", got)
	}
}

func TestSaveAllPreservesUnadmittedOldFormatCache(t *testing.T) {
	for _, load := range []string{"rejected preview", "empty admission"} {
		t.Run(load, func(t *testing.T) {
			root := t.TempDir()
			m := NewManager(root)
			data := []byte(`{"history":["A","B","C"],"footer":[],"stale_refusals":0,"last_hash":""}`)
			if err := os.WriteFile(m.stateFile("p"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := SnapshotContext{TerminalID: "terminal-1", ReadSource: SourceLegacy}
			page := snapshotPage("A", "B", "C")
			if load == "rejected preview" {
				if _, accepted := m.PreviewSnapshot("p", page, ctx); accepted {
					t.Fatal("preview unexpectedly accepted the terminal identity")
				}
			} else if _, _, admission := m.AdmitSnapshot("p", "", ctx, 0); admission.Admitted {
				t.Fatal("empty snapshot was admitted")
			}
			if err := m.SaveAll(); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(m.stateFile("p"))
			if err != nil {
				t.Fatal(err)
			}
			if string(saved) != string(data) {
				t.Errorf("SaveAll changed unadmitted old-format cache: got %s, want %s", saved, data)
			}
			restarted := NewManager(root)
			if got := restarted.MergeSnapshot("p", page, ctx); got != page {
				t.Fatalf("scrollback restart duplicated old-format body: %q", got)
			}
		})
	}
}

func TestSaveAllWritesDirtyStateBeforeDeadline(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	now := time.Unix(10000, 0)
	m.now = func() time.Time { return now }
	m.Merge("p", snapshotPage("A", "B"))
	if err := m.FlushDue(now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	page := snapshotPage("B", "C")
	want := m.Merge("p", page)
	if state := m.states["p"]; !state.dirty || !state.saveDue.After(now) {
		t.Fatalf("expected dirty state with future save deadline: %+v", state)
	}
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	if m.states["p"].dirty {
		t.Fatal("SaveAll left the saved state dirty")
	}
	if got := NewManager(root).Content("p", 0); got != want {
		t.Fatalf("SaveAll missed the future-deadline mutation: got %q, want %q", got, want)
	}
}

func TestPersistenceReportsChangedFailureAndRecovery(t *testing.T) {
	m := NewManager(t.TempDir())
	m.Merge("p", snapshotPage("A", "B"))
	var calls atomic.Int32
	m.writeFile = func(path string, data []byte, mode os.FileMode) error {
		switch calls.Add(1) {
		case 1, 2:
			return errors.New("disk full")
		case 3:
			return errors.New("read only")
		default:
			return os.WriteFile(path, data, mode)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	reports := make(chan error, 5)
	go func() { m.RunPersistenceWithReport(ctx, func(err error) { reports <- err }); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("persistence did not stop")
		}
	})
	for index, text := range []string{"disk full", "read only", ""} {
		select {
		case err := <-reports:
			if text == "" {
				if err != nil {
					t.Fatalf("recovery error=%v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), text) {
				t.Fatalf("report %d error=%v want=%q", index, err, text)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("missing persistence report %d", index)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("persistence attempts=%d", calls.Load())
	}
}

func TestPersistenceUnrelatedSaveDoesNotReportRecovery(t *testing.T) {
	m := NewManager(t.TempDir())
	var clock atomic.Int64
	clock.Store(time.Unix(10000, 0).UnixNano())
	m.now = func() time.Time { return time.Unix(0, clock.Load()) }
	m.Merge("a", snapshotPage("A", "B"))
	bSaved, retryStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var attempts int
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	m.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == m.stateFile("a")+".tmp" {
			attempts++
			if attempts == 1 {
				return errors.New("disk full")
			}
			close(retryStarted)
			<-release
			return os.WriteFile(path, data, mode)
		}
		err := os.WriteFile(path, data, mode)
		close(bSaved)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done, reports := make(chan struct{}), make(chan error, 5)
	go func() { m.RunPersistenceWithReport(ctx, func(err error) { reports <- err }); close(done) }()
	t.Cleanup(func() { cancel(); unblock(); <-done })
	select {
	case err := <-reports:
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("initial failure=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing initial failure")
	}
	m.Merge("b", snapshotPage("C", "D"))
	select {
	case <-bSaved:
	case <-time.After(3 * time.Second):
		t.Fatal("unrelated pane did not save")
	}
	clock.Add(int64(2 * time.Second))
	select {
	case m.wake <- struct{}{}:
	default:
	}
	select {
	case <-retryStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("failed pane did not retry")
	}
	select {
	case err := <-reports:
		t.Fatalf("unrelated save reported recovery before failed pane succeeded: %v", err)
	default:
	}
	m.mu.Lock()
	if !m.states["a"].dirty {
		m.mu.Unlock()
		t.Fatal("failed pane became clean before its retry finished")
	}
	m.mu.Unlock()
	cancel()
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("persistence did not finish the successful retry")
	}
	select {
	case err := <-reports:
		if err != nil {
			t.Fatalf("recovery error=%v", err)
		}
	default:
		t.Fatal("missing recovery after failed pane succeeded")
	}
	select {
	case err := <-reports:
		t.Fatalf("duplicate recovery report: %v", err)
	default:
	}
}

func TestPersistenceErrorsFollowCurrentPaneStates(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	for _, paneID := range []string{"b", "a"} {
		m.MergeSnapshot(paneID, snapshotPage("A", "B"), ctx)
	}
	m.writeFile = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
	if err := m.SaveAll(); err == nil {
		t.Fatal("missing save failure")
	}
	want := "save pane history \"a\": disk full\nsave pane history \"b\": disk full"
	for range 5 {
		if err := m.persistenceError(); err == nil || err.Error() != want {
			t.Fatalf("persistence errors=%v, want %q", err, want)
		}
	}
	m.Discard("a")
	if err := m.persistenceError(); err == nil || err.Error() != "save pane history \"b\": disk full" {
		t.Fatalf("discard retained obsolete error: %v", err)
	}
	ctx.TerminalID = "replacement"
	m.MergeSnapshot("b", snapshotPage("new A", "new B"), ctx)
	if err := m.persistenceError(); err != nil {
		t.Fatalf("replacement retained obsolete error: %v", err)
	}
}

func TestFooterOnlyDirtyStatePersists(t *testing.T) {
	m := NewManager(t.TempDir())
	now := time.Unix(10000, 0)
	m.now = func() time.Time { return now }
	m.Merge("p", snapshotPage("A", "B"))
	if err := m.FlushDue(now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	m.Merge("p", strings.ReplaceAll(snapshotPage("A", "B"), "footer", "fresh"))
	if err := m.FlushDue(now.Add(SaveInterval)); err != nil {
		t.Fatal(err)
	}
	if got := persistedState(t, m, "p"); got.Footer[0] != "fresh1" {
		t.Fatalf("footer=%v", got.Footer)
	}
}

func TestSlowSaveBatchRetainsRetryGapAfterFailure(t *testing.T) {
	m := NewManager(t.TempDir())
	now := time.Unix(10000, 0)
	m.now = func() time.Time { return now }
	m.Merge("first", snapshotPage("A", "B"))
	m.Merge("second", snapshotPage("C", "D"))
	calls := 0
	m.writeFile = func(path string, data []byte, mode os.FileMode) error {
		calls++
		if calls == 1 {
			now = now.Add(5 * time.Second)
			return os.WriteFile(path, data, mode)
		}
		return errors.New("failed after slow job")
	}
	if err := m.FlushDue(now); err == nil {
		t.Fatal("failed job did not report error")
	}
	for paneID, state := range m.states {
		if state.dirty && state.saveDue.Before(now.Add(time.Second)) {
			t.Fatalf("pane %s retry=%v failure time=%v", paneID, state.saveDue, now)
		}
	}
}

func TestFailedSaveRetainsDirtyStateAndDoesNotAdvanceLastSave(t *testing.T) {
	m := NewManager(t.TempDir())
	now := time.Unix(10000, 0)
	m.now = func() time.Time { return now }
	m.Merge("p", snapshotPage("A", "B"))
	m.writeFile = func(string, []byte, os.FileMode) error { return errors.New("write failed") }
	if err := m.FlushDue(now); err == nil {
		t.Fatal("save failure hidden")
	}
	if !m.states["p"].dirty || !m.lastSave["p"].IsZero() || m.states["p"].saveDue.Before(now.Add(time.Second)) {
		t.Fatal("failed save cleared dirty state or retry bound")
	}
	m.writeFile = os.WriteFile
	if err := m.FlushDue(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardOrReplacementFencesQueuedSaveRename(t *testing.T) {
	for _, discard := range []bool{true, false} {
		m := NewManager(t.TempDir())
		m.Merge("p", snapshotPage("A", "B"))
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		m.beforeRename = func() { close(entered); <-release }
		go func() { done <- m.FlushDue(time.Now().Add(time.Hour)) }()
		<-entered
		m.Discard("p")
		if !discard {
			m.Merge("p", snapshotPage("replacement A", "replacement B"))
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(m.stateFile("p")); !os.IsNotExist(err) {
			t.Fatalf("obsolete save recreated file: %v", err)
		}
		if _, err := os.Stat(m.stateFile("p") + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("obsolete save retained temporary file: %v", err)
		}
	}
}

func TestProgressTailUpdatesPersistWithoutArchiveGrowth(t *testing.T) {
	m := NewManager(t.TempDir())
	now := time.Unix(10000, 0)
	m.now = func() time.Time { return now }
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage("A", "B", "C", "progress 0"), ctx)
	if err := m.FlushDue(now); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 5; index++ {
		now = now.Add(time.Second)
		m.MergeSnapshot("p", snapshotPage("A", "B", "C", fmt.Sprintf("progress %d", index)), ctx)
	}
	if err := m.FlushDue(now.Add(SaveInterval)); err != nil {
		t.Fatal(err)
	}
	state := persistedState(t, m, "p")
	if len(state.History) != 4 || state.LiveStart != 0 || state.History[3] != "progress 5" {
		t.Fatalf("persisted progress=%+v", state)
	}
}

func TestSaveSnapshotKeepsNewerMutationDirty(t *testing.T) {
	m := NewManager(t.TempDir())
	m.Merge("p", snapshotPage("A", "B"))
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	m.beforeRename = func() { close(entered); <-release }
	go func() { done <- m.FlushDue(time.Now().Add(time.Hour)) }()
	<-entered
	m.Merge("p", snapshotPage("B", "C"))
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !m.states["p"].dirty {
		t.Fatal("older save cleared a newer mutation")
	}
	m.beforeRename = nil
	if err := m.FlushDue(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := persistedState(t, m, "p"); strings.Join(got.History, "\n") != "A\nB\nC" {
		t.Fatalf("newer saved history=%v", got.History)
	}
}

func TestRestartReadsFinalAdmittedPage(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	m.MergeSnapshot("p", snapshotPage("A", "B"), physicalContext(1, 40))
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	m.MergeSnapshot("p", snapshotPage("A", "B", "final"), physicalContext(1, 40))
	if err := m.FlushDue(time.Now().Add(SaveInterval)); err != nil {
		t.Fatal(err)
	}
	if got := NewManager(root).Content("p", 0); got != snapshotPage("A", "B", "final") {
		t.Fatalf("restart content=%q", got)
	}
}

func TestUnchangedMergeDoesNotPostponeDirtyFlush(t *testing.T) {
	m := NewManager(t.TempDir())
	now := time.Unix(10000, 0)
	m.now = func() time.Time { return now }
	m.Merge("p", snapshotPage("A", "B"))
	if err := m.FlushDue(now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	m.Merge("p", snapshotPage("B", "C"))
	due := m.states["p"].saveDue
	for range 5 {
		now = now.Add(time.Second)
		m.Merge("p", snapshotPage("B", "C"))
	}
	if m.states["p"].saveDue != due {
		t.Fatal("unchanged merges reset save deadline")
	}
	if err := m.FlushDue(due); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalReplacementInvalidatesQueuedSave(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := physicalContext(1, 40)
	m.MergeSnapshot("p", snapshotPage("old A", "old B"), ctx)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	m.beforeRename = func() { close(entered); <-release }
	go func() { done <- m.FlushDue(time.Now().Add(time.Hour)) }()
	<-entered
	ctx.TerminalID = "replacement"
	m.MergeSnapshot("p", snapshotPage("new A", "new B"), ctx)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.stateFile("p")); !os.IsNotExist(err) {
		t.Fatal("replaced state was recreated by old save")
	}
	m.beforeRename = nil
	if err := m.SaveAll(); err != nil {
		t.Fatal(err)
	}
	if got := persistedState(t, m, "p"); got.TerminalID != "replacement" || strings.Join(got.History, "\n") != "new A\nnew B" {
		t.Fatalf("replacement saved=%+v", got)
	}
}

func TestRunPersistenceSavesWithoutAnotherMerge(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.RunPersistence(ctx); close(done) }()
	m.Merge("p", snapshotPage("A", "B"))
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(m.stateFile("p")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("worker did not save")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}
