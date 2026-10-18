package app

import (
	"context"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/history"
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
	s.drainLifecycleWork()
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
