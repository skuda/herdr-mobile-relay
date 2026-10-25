package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
)

func TestShutdownJoinsCaptureBeforeFinalSave(t *testing.T) {
	s := testServerWithCacheDir(t.TempDir())
	s.historyTasks = newLifecycleTasks(context.Background())
	s.persistenceTasks = newLifecycleTasks(context.Background())
	s.persistenceTasks.Start(s.historyM.RunPersistence)
	entered, finished := make(chan struct{}), make(chan struct{})
	s.historyTasks.Start(func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		s.historyM.Merge("p", capturePage("admitted A", "admitted B"))
		close(finished)
	})
	<-entered
	if err := s.drainLifecycleWork(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("shutdown saved before capture worker joined")
	}
	restarted := history.NewManager(s.cfg.CacheDir)
	if got := restarted.Content("p", 0); got != capturePage("admitted A", "admitted B") {
		t.Fatalf("final persisted content=%q", got)
	}
	if s.historyTasks.Start(func(context.Context) {}) {
		t.Fatal("shutdown left capture admission open")
	}
}

type lifecycleBlockingSize struct {
	*paneSizeFakes
	block            atomic.Bool
	entered, release chan struct{}
}

func (f *lifecycleBlockingSize) Open(string) (panesize.Terminal, error) { return f, nil }

func (f *lifecycleBlockingSize) Size() (panesize.Winsize, error) {
	if f.block.CompareAndSwap(true, false) {
		close(f.entered)
		<-f.release
	}
	return f.paneSizeFakes.Size()
}

func TestShutdownBlockedHistorySampleStillSaves(t *testing.T) {
	s, f, advance := captureServer(t, false)
	g := &lifecycleBlockingSize{paneSizeFakes: f, entered: make(chan struct{}), release: make(chan struct{})}
	s.paneSizeM = panesize.NewManagerWith(g, g, g, s.historyNow, nil)
	if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20); err != nil {
		t.Fatal(err)
	}
	advance(4 * time.Second)
	executeCapture(t, s, capturePage("initial A", "initial B"), true)
	want := capturePage("initial B", "dirty C")
	executeCapture(t, s, want, true)
	if got := history.NewManager(s.cfg.CacheDir).Content("pane-1", 0); got == s.historyM.Content("pane-1", 0) {
		t.Fatal("fixture has no unsaved admitted history")
	}
	want = s.historyM.Content("pane-1", 0)
	installPaneReadFixture(t, s, func(context.Context) (string, error) { return capturePage("late D"), nil })
	s.historyTasks = newLifecycleTasks(context.Background())
	s.transitionTasks = newLifecycleTasks(context.Background())
	s.persistenceTasks = newLifecycleTasks(context.Background())
	persistenceCanceled := make(chan struct{})
	s.persistenceTasks.Start(func(ctx context.Context) { <-ctx.Done(); close(persistenceCanceled) })
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(g.release) }) }
	t.Cleanup(func() {
		release()
		s.historyTasks.Stop()
		s.transitionTasks.Stop()
		s.persistenceTasks.Stop()
	})
	g.block.Store(true)
	s.scheduleHistoryCapture(context.Background(), "pane-1")
	awaitHistorySignal(t, g.entered, "scheduled physical Size")
	started := time.Now()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	err := s.paneSizeM.Shutdown(shutdownCtx)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "history descriptors (shutdown deadline: context deadline exceeded)") {
		t.Fatalf("manager did not report the blocked descriptor: %v", err)
	}
	s.lifecycleDrainTimeout = 25 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- s.drainLifecycleWork() }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "unfinished history capture work") {
			t.Errorf("shutdown did not name unfinished capture work: %v", err)
		}
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("shutdown waited indefinitely for the blocked history sample")
	}
	t.Logf("blocked-sample shutdown duration: %s", time.Since(started))
	awaitHistorySignal(t, persistenceCanceled, "persistence cancellation")
	for _, tasks := range []*lifecycleTasks{s.transitionTasks, s.historyTasks, s.persistenceTasks} {
		if tasks.Start(func(context.Context) {}) {
			t.Error("shutdown left lifecycle admission open")
		}
	}
	if got := history.NewManager(s.cfg.CacheDir).Content("pane-1", 0); got != want {
		t.Fatal("shutdown did not save admitted dirty history")
	}
	release()
	s.historyTasks.Stop()
	if s.historyM.Content("pane-1", 0) != want {
		t.Fatal("late capture changed history after the final save")
	}
}

func TestShutdownLateEligibleCaptureCannotAdmit(t *testing.T) {
	s, _, _ := captureServer(t, false)
	baseline := capturePage("A", "B", "C")
	executeCapture(t, s, baseline, false)
	entered, release := make(chan struct{}), make(chan struct{})
	s.historyReadFinishedObserver = func(historyReadToken) { close(entered); <-release }
	s.historyTasks = newLifecycleTasks(context.Background())
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); s.historyTasks.Stop() })
	var outcome historyCaptureOutcome
	s.historyTasks.Start(func(context.Context) {
		outcome.result, outcome.err = s.executeHistoryRead(context.Background(), captureRequest(s, false),
			func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
				return herdr.PaneRead{Content: []byte(capturePage("B", "C", "D"))}, nil
			})
	})
	awaitHistorySignal(t, entered, "eligible capture before admission")
	s.lifecycleDrainTimeout = 25 * time.Millisecond
	if err := s.drainLifecycleWork(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished capture not reported: %v", err)
	}
	unblock()
	s.historyTasks.Stop()
	if outcome.err != nil || outcome.result.Capture.Admitted || !outcome.result.Composed || outcome.result.Response["history_retry"] != true {
		t.Errorf("late eligible capture did not remain a preview: %+v", outcome)
	}
	if s.historyM.Content("pane-1", 0) != baseline || history.NewManager(s.cfg.CacheDir).Content("pane-1", 0) != baseline {
		t.Fatal("late preview changed live or persisted history")
	}
}

func TestShutdownLifecycleGroupsShareDeadline(t *testing.T) {
	s := testServerWithCacheDir(t.TempDir())
	s.transitionTasks = newLifecycleTasks(context.Background())
	s.historyTasks = newLifecycleTasks(context.Background())
	s.persistenceTasks = newLifecycleTasks(context.Background())
	release := make(chan struct{})
	groups := []*lifecycleTasks{s.transitionTasks, s.historyTasks, s.persistenceTasks}
	for _, tasks := range groups {
		tasks.Start(func(context.Context) { <-release })
	}
	t.Cleanup(func() {
		close(release)
		for _, tasks := range groups {
			tasks.Stop()
		}
	})
	s.lifecycleDrainTimeout = 100 * time.Millisecond
	started := time.Now()
	err := s.drainLifecycleWork()
	if err == nil {
		t.Fatal("shutdown did not report unfinished lifecycle groups")
	}
	for _, name := range []string{"transition", "history capture", "history persistence"} {
		if !strings.Contains(err.Error(), "unfinished "+name+" work") {
			t.Errorf("shutdown omitted unfinished %s work: %v", name, err)
		}
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Errorf("lifecycle joins exceeded the shared deadline: %s", elapsed)
	}
	for _, tasks := range groups {
		if tasks.ctx.Err() == nil || tasks.Start(func(context.Context) {}) {
			t.Error("deadline left a later lifecycle group open or uncanceled")
		}
	}
}

func TestLifecycleTasksStopContextDeadlineClosesAdmission(t *testing.T) {
	tasks := newLifecycleTasks(context.Background())
	release, canceled := make(chan struct{}), make(chan struct{})
	tasks.Start(func(ctx context.Context) { <-ctx.Done(); close(canceled); <-release })
	t.Cleanup(func() { close(release); tasks.Stop() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := tasks.StopContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished worker not reported: %v", err)
	}
	awaitHistorySignal(t, canceled, "worker cancellation")
	if tasks.Start(func(context.Context) {}) {
		t.Fatal("deadline left group admission open")
	}
}

func TestLifecycleTasksStopCancelsDrainsAndClosesAdmission(t *testing.T) {
	tasks := newLifecycleTasks(context.Background())
	started := make(chan struct{})
	finished := make(chan struct{})
	if !tasks.Start(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(finished)
	}) {
		t.Fatal("initial task was rejected")
	}
	<-started

	tasks.Stop()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Stop returned before task observed cancellation")
	}
	if tasks.Start(func(context.Context) {}) {
		t.Fatal("task admitted after Stop")
	}
}
