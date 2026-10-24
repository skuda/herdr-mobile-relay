package panesize

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

func historyRead(t *testing.T, f *fixture, paneID string) *HistoryRead {
	t.Helper()
	read, err := f.m.BeginHistoryRead(context.Background(), paneID, "terminal-"+paneID)
	if err != nil {
		t.Fatalf("BeginHistoryRead() error = %v", err)
	}
	t.Cleanup(func() { _ = read.Close() })
	return read
}

func historySample(t *testing.T, f *fixture, paneID string) HistoryGeometry {
	t.Helper()
	read := historyRead(t, f, paneID)
	geometry := read.Geometry()
	_ = read.Close()
	return geometry
}

func finishSafe(t *testing.T, read *HistoryRead) HistoryGeometry {
	t.Helper()
	geometry, err := read.Finish()
	if err != nil || !geometry.Known || geometry.InProgress {
		t.Fatalf("Finish() = %+v, %v, want safe known geometry", geometry, err)
	}
	return geometry
}

func finishUnsafe(t *testing.T, read *HistoryRead) HistoryGeometry {
	t.Helper()
	geometry, err := read.Finish()
	if !errors.Is(err, ErrHistoryReadUnsafe) {
		t.Fatalf("Finish() = %+v, %v, want unsafe read", geometry, err)
	}
	return geometry
}

func TestHistoryGeometryTracksAcquireRestoreAndReacquire(t *testing.T) {
	f := newFixture(t)
	f.addPane("p", 100, "/dev/pts/1", 1)
	initial := finishSafe(t, historyRead(t, f, "p"))
	if initial.Columns != 120 || initial.Rows != 40 || !initial.ChangedAt.IsZero() {
		t.Fatalf("baseline = %+v", initial)
	}
	f.acquire("owner", "p", 60, 20)
	leased := historySample(t, f, "p")
	if leased.Epoch <= initial.Epoch || leased.Columns != 60 || leased.Rows != 20 || !leased.ChangedAt.Equal(f.clock.Now()) {
		t.Fatalf("leased = %+v, initial = %+v", leased, initial)
	}
	settling := historyRead(t, f, "p")
	f.clock.Advance(4 * time.Second)
	finishUnsafe(t, settling)
	finishSafe(t, historyRead(t, f, "p"))
	if err := f.m.Release(context.Background(), "owner", "p", true); err != nil {
		t.Fatal(err)
	}
	if f.state("p") != nil {
		t.Fatal("successful cleanup retained lease state")
	}
	restored := historySample(t, f, "p")
	if restored.Epoch <= leased.Epoch || restored.Columns != 120 || restored.Rows != 40 || !restored.ChangedAt.Equal(f.clock.Now()) {
		t.Fatalf("restore = %+v, leased = %+v", restored, leased)
	}
	f.clock.Advance(4 * time.Second)
	finishSafe(t, historyRead(t, f, "p"))
	f.acquire("owner", "p", 60, 20)
	reacquired := historySample(t, f, "p")
	if reacquired.Epoch <= restored.Epoch || reacquired.LeaseRevision <= restored.LeaseRevision {
		t.Fatalf("reacquired = %+v, restored = %+v", reacquired, restored)
	}
}

func TestHistoryGeometryChangesOnHeightAndExternalResize(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("p", 100, "/dev/pts/1", 1)
	initial := historySample(t, f, "p")
	f.clock.Advance(time.Second)
	f.ops.mu.Lock()
	device.size.Rows = 30
	f.ops.mu.Unlock()
	read := historyRead(t, f, "p")
	height := read.Geometry()
	finishUnsafe(t, read)
	if height.Epoch <= initial.Epoch || height.Rows != 30 || !height.ChangedAt.Equal(f.clock.Now()) {
		t.Fatalf("height observation = %+v", height)
	}
	f.clock.Advance(4 * time.Second)
	read = historyRead(t, f, "p")
	f.ops.mu.Lock()
	device.size.Columns = 80
	f.ops.mu.Unlock()
	width := finishUnsafe(t, read)
	if width.Epoch <= height.Epoch || width.Columns != 80 || !width.ChangedAt.Equal(f.clock.Now()) {
		t.Fatalf("width observation = %+v", width)
	}
	f.clock.Advance(4 * time.Second)
	finishSafe(t, historyRead(t, f, "p"))
	f.acquire("wide", "p", 80, 30)
	f.acquire("narrow", "p", 60, 20)
	f.clock.Advance(4 * time.Second)
	narrow := historySample(t, f, "p")
	if err := f.m.Release(context.Background(), "narrow", "p", true); err != nil {
		t.Fatal(err)
	}
	reconciled := historySample(t, f, "p")
	if reconciled.Epoch <= narrow.Epoch || reconciled.Columns != 80 || reconciled.Rows != 30 {
		t.Fatalf("reconciled geometry = %+v, narrow = %+v", reconciled, narrow)
	}
}

func TestHistoryGeometrySameSizeRenewalKeepsEpoch(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("p", 100, "/dev/pts/1", 1)
	f.acquire("owner", "p", 60, 20)
	f.clock.Advance(4 * time.Second)
	before := historySample(t, f, "p")
	read := historyRead(t, f, "p")
	f.acquire("owner", "p", 60, 20)
	after := finishSafe(t, read)
	if after.Epoch != before.Epoch || after.Revision != before.Revision || after.LeaseRevision != before.LeaseRevision || after.ChangedAt != before.ChangedAt {
		t.Fatalf("same-size renewal changed geometry: before=%+v after=%+v", before, after)
	}
	if len(device.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(device.writes))
	}
}

type historyBarrierOps struct {
	TerminalOps
	before func()
	after  func()
}

func (o historyBarrierOps) Open(path string) (Terminal, error) {
	terminal, err := o.TerminalOps.Open(path)
	if err != nil {
		return nil, err
	}
	return historyBarrierTerminal{Terminal: terminal, before: o.before, after: o.after}, nil
}

type historyBarrierTerminal struct {
	Terminal
	before func()
	after  func()
}

func (t historyBarrierTerminal) SetSize(next Winsize) error {
	if t.before != nil {
		t.before()
	}
	err := t.Terminal.SetSize(next)
	if t.after != nil {
		t.after()
	}
	return err
}

func TestHistoryReadRejectsResizeInProgressAndCrossedEpoch(t *testing.T) {
	for _, phase := range []string{"before ioctl", "before publication"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			f.addPane("p", 100, "/dev/pts/1", 1)
			read := historyRead(t, f, "p")
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var barrierOnce sync.Once
			barrier := func() { barrierOnce.Do(func() { close(entered); <-release }) }
			ops := historyBarrierOps{TerminalOps: f.ops}
			if phase == "before ioctl" {
				ops.before = barrier
			} else {
				ops.after = barrier
			}
			f.m.ops = ops
			go func() {
				_, _, err := f.m.Acquire(context.Background(), "owner", "p", 60, 20)
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("resize did not reach barrier")
			}
			finishUnsafe(t, read)
			during := historyRead(t, f, "p")
			if !during.Geometry().InProgress {
				t.Fatalf("in-progress geometry = %+v", during.Geometry())
			}
			finishUnsafe(t, during)
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			f.clock.Advance(4 * time.Second)
			crossed := historyRead(t, f, "p")
			f.acquire("owner", "p", 70, 20)
			finishUnsafe(t, crossed)
		})
	}
}

func TestFinishedHistoryTokenKeepsRecordedPTYEpoch(t *testing.T) {
	f := newFixture(t)
	f.addPane("p", 100, "/dev/pts/1", 1)
	read := historyRead(t, f, "p")
	finished := finishSafe(t, read)
	f.acquire("owner", "p", 60, 20)
	current := historySample(t, f, "p")
	again := finishSafe(t, read)
	if finished != again || finished.Epoch == current.Epoch || finished.Columns != 120 {
		t.Fatalf("finished=%+v repeated=%+v current=%+v", finished, again, current)
	}
}

func TestLeaseHistoryStateIsPaneWideThroughRestore(t *testing.T) {
	for _, end := range []string{"restore success", "exhaustion"} {
		t.Run(end, func(t *testing.T) {
			f := newFixture(t)
			device := f.addPane("p", 100, "/dev/pts/1", 1)
			f.acquire("owner", "p", 60, 20)
			active, revision := f.m.LeaseHistoryState("p", "terminal-p")
			if !active || revision == 0 {
				t.Fatalf("gate = %v revision=%d", active, revision)
			}
			if _, _, own := f.m.ReadGeometry("reader", "p"); own {
				t.Fatal("non-owner has a client lease")
			}
			if err := f.m.Release(context.Background(), "owner", "p", false); err != nil {
				t.Fatal(err)
			}
			if on, rev := f.m.LeaseHistoryState("p", "terminal-p"); !on || rev != revision {
				t.Fatalf("grace gate = %v revision=%d", on, rev)
			}
			f.clock.Advance(ReleaseGrace)
			if on, rev := f.m.LeaseHistoryState("p", "terminal-p"); !on || rev != revision {
				t.Fatalf("expired-unswept gate = %v revision=%d", on, rev)
			}
			f.ops.mu.Lock()
			device.setErr = errors.New("retry restore")
			f.ops.mu.Unlock()
			f.m.SweepExpired(context.Background())
			if on, rev := f.m.LeaseHistoryState("p", "terminal-p"); !on || rev != revision || f.state("p").cleanup == nil {
				t.Fatalf("pending cleanup gate = %v revision=%d", on, rev)
			}
			if end == "restore success" {
				f.ops.mu.Lock()
				device.setErr = nil
				f.ops.mu.Unlock()
				f.m.ops = beforeSetOps{TerminalOps: f.ops, beforeSet: func() {
					if on, rev := f.m.LeaseHistoryState("p", "terminal-p"); !on || rev != revision {
						t.Errorf("restore-in-progress gate = %v revision=%d", on, rev)
					}
					read := historyRead(t, f, "p")
					if !read.Geometry().InProgress {
						t.Errorf("restore geometry = %+v", read.Geometry())
					}
					finishUnsafe(t, read)
				}}
				f.clock.Advance(time.Second)
			} else {
				f.clock.Advance(CleanupBudget)
			}
			f.m.SweepExpired(context.Background())
			if on, rev := f.m.LeaseHistoryState("p", "terminal-p"); on || rev <= revision || f.state("p") != nil {
				t.Fatalf("finished cleanup gate = %v revision=%d", on, rev)
			}
		})
	}
}

func TestLeaseHistoryStateRejectsPlaceholderFailedAcquireAndWrongTerminal(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("p", 100, "/dev/pts/1", 1)
	placeholder, _, err := f.m.lockPane(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := f.m.LeaseHistoryState("p", "terminal-p"); on {
		t.Fatal("unresolved placeholder enabled gate")
	}
	f.m.mu.Lock()
	f.m.dropLocked("p", placeholder)
	f.m.mu.Unlock()
	placeholder.release()
	device.setErr = errors.New("write failed")
	if _, _, err := f.m.Acquire(context.Background(), "owner", "p", 60, 20); err == nil {
		t.Fatal("failed acquisition succeeded")
	}
	if on, _ := f.m.LeaseHistoryState("p", "terminal-p"); on {
		t.Fatal("failed acquisition enabled gate")
	}
	device.setErr = nil
	f.acquire("owner", "p", 60, 20)
	for _, terminalID := range []string{"", "terminal-other"} {
		if on, _ := f.m.LeaseHistoryState("p", terminalID); on {
			t.Fatalf("terminal %q enabled gate", terminalID)
		}
	}
}

func TestHistoryReadRejectsCrossedLeaseLifecycle(t *testing.T) {
	for _, mode := range []string{"off on off", "reacquire cleanup"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.addPane("p", 100, "/dev/pts/1", 1)
			if mode == "off on off" {
				read := historyRead(t, f, "p")
				f.acquire("owner", "p", 120, 40)
				_ = f.m.Release(context.Background(), "owner", "p", true)
				finishUnsafe(t, read)
				return
			}
			f.acquire("owner", "p", 60, 20)
			f.clock.Advance(4 * time.Second)
			state := f.state("p")
			if err := state.acquire(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = f.m.Release(ctx, "owner", "p", true)
			state.release()
			read := historyRead(t, f, "p")
			f.acquire("owner", "p", 60, 20)
			finishUnsafe(t, read)
		})
	}
}

func TestHistoryGeometryDoesNotInferEmulatorOnlyChange(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("p", 100, "/dev/pts/1", 1)
	before := finishSafe(t, historyRead(t, f, "p"))
	f.ops.mu.Lock()
	device.size.XPixel++
	device.size.YPixel++
	f.ops.mu.Unlock()
	after := finishSafe(t, historyRead(t, f, "p"))
	if after.Epoch != before.Epoch || after.ChangedAt != before.ChangedAt {
		t.Fatalf("equal PTY rows/columns created epoch: before=%+v after=%+v", before, after)
	}
	if len(device.writes) != 0 {
		t.Fatal("history sampling wrote a terminal size")
	}
}

func TestHistoryReadRejectsABAThroughManager(t *testing.T) {
	f := newFixture(t)
	f.addPane("p", 100, "/dev/pts/1", 1)
	read := historyRead(t, f, "p")
	f.acquire("owner", "p", 60, 20)
	_ = f.m.Release(context.Background(), "owner", "p", true)
	f.clock.Advance(4 * time.Second)
	geometry := finishUnsafe(t, read)
	if geometry.Columns != 120 || geometry.Rows != 40 || geometry.Epoch <= read.Geometry().Epoch {
		t.Fatalf("ABA result = %+v, start=%+v", geometry, read.Geometry())
	}
	first := historyRead(t, f, "p")
	f.acquire("owner", "p", 60, 20)
	f.clock.Advance(4 * time.Second)
	second := historyRead(t, f, "p")
	_ = f.m.Release(context.Background(), "owner", "p", true)
	f.clock.Advance(4 * time.Second)
	finishUnsafe(t, first)
	finishUnsafe(t, second)
	final := finishSafe(t, historyRead(t, f, "p"))
	if final.Columns != 120 || final.Rows != 40 || final.Epoch <= second.Geometry().Epoch {
		t.Fatalf("multiple-reader ABA result = %+v", final)
	}
}

type historyFailSizeOps struct{ TerminalOps }

type historyFailSizeTerminal struct{ Terminal }

func (o historyFailSizeOps) Open(path string) (Terminal, error) {
	terminal, err := o.TerminalOps.Open(path)
	if err != nil {
		return nil, err
	}
	return historyFailSizeTerminal{terminal}, nil
}

func (historyFailSizeTerminal) Size() (Winsize, error) {
	return Winsize{}, errors.New("size unavailable")
}

func TestHistoryGeometryUnknownOnLookupSizeOrWriteFailure(t *testing.T) {
	for _, failure := range []string{"lookup", "size", "write"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			device := f.addPane("p", 100, "/dev/pts/1", 1)
			before := historySample(t, f, "p")
			switch failure {
			case "lookup":
				f.herdr.setFailure("p", errors.New("lookup failed"))
			case "size":
				f.m.ops = historyFailSizeOps{f.ops}
			case "write":
				device.setErr = errors.New("ioctl failed")
				if _, _, err := f.m.Acquire(context.Background(), "owner", "p", 60, 20); err == nil {
					t.Fatal("write failure succeeded")
				}
			}
			if failure != "write" {
				if read, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p"); err == nil {
					_ = read.Close()
					t.Fatal("failed history sample succeeded")
				}
			}
			f.m.mu.Lock()
			unknown := f.m.historyGeometry["p"].geometry
			f.m.mu.Unlock()
			if unknown.Known || unknown.InProgress || unknown.Revision <= before.Revision {
				t.Fatalf("failed sample geometry = %+v", unknown)
			}
			f.herdr.setFailure("p", nil)
			f.m.ops = f.ops
			device.setErr = nil
			finishSafe(t, historyRead(t, f, "p"))
		})
	}
}

func TestHistoryReadHandleClosesOnCancellationAndError(t *testing.T) {
	for _, end := range []string{"close", "finish", "cancel", "finish size error", "validation error", "unsupported"} {
		t.Run(end, func(t *testing.T) {
			f := newFixture(t)
			f.addPane("p", 100, "/dev/pts/1", 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if end == "validation error" {
				f.m.resolver = &historyFlakyResolver{TTYResolver: f.ttys}
			}
			if end == "unsupported" {
				f.m.available = false
			}
			read, err := f.m.BeginHistoryRead(ctx, "p", "terminal-p")
			if end == "validation error" || end == "unsupported" {
				if err == nil {
					t.Fatal("validation failure succeeded")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				switch end {
				case "close":
					_ = read.Close()
				case "finish":
					finishSafe(t, read)
				case "cancel":
					cancel()
					deadline := time.NewTimer(time.Second)
					defer deadline.Stop()
					for {
						f.ops.mu.Lock()
						closed := f.ops.opens == f.ops.closes
						f.ops.mu.Unlock()
						if closed {
							break
						}
						select {
						case <-deadline.C:
							t.Fatal("cancellation did not close descriptor")
						default:
							runtime.Gosched()
						}
					}
					if _, err := read.Finish(); !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled Finish() error = %v", err)
					}
				case "finish size error":
					read.terminal = historyFailSizeTerminal{read.terminal}
					if _, err := read.Finish(); err == nil {
						t.Fatal("finish size failure succeeded")
					}
				}
				_ = read.Close()
			}
			f.ops.mu.Lock()
			defer f.ops.mu.Unlock()
			if f.ops.opens != f.ops.closes {
				t.Fatalf("descriptor opens=%d closes=%d", f.ops.opens, f.ops.closes)
			}
		})
	}
}

type historyFlakyResolver struct {
	TTYResolver
	calls int
}

func (r *historyFlakyResolver) TTYPath(ctx context.Context, pid int) (string, error) {
	r.calls++
	if r.calls > 1 {
		return "", ErrTTYUnavailable
	}
	return r.TTYResolver.TTYPath(ctx, pid)
}

func TestForgetHistoryPaneRetainsResizeAndSettleFence(t *testing.T) {
	f := newFixture(t)
	f.addPane("p", 100, "/dev/pts/1", 1)
	initial := historySample(t, f, "p")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.m.ops = historyBarrierOps{TerminalOps: f.ops, after: func() { close(entered); <-release }}
	go func() {
		_, _, err := f.m.Acquire(context.Background(), "owner", "p", 60, 20)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("resize did not reach publication barrier")
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
	during := historyRead(t, f, "p")
	finishUnsafe(t, during)
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
	settling := historyRead(t, f, "p")
	geometry := finishUnsafe(t, settling)
	if geometry.Epoch <= initial.Epoch || geometry.ChangedAt.IsZero() {
		t.Fatalf("forgotten resize lost completion geometry: %+v", geometry)
	}
	f.clock.Advance(HistorySettleWindow)
	finishSafe(t, historyRead(t, f, "p"))
	f.m.ForgetHistoryPane("p", "terminal-p")
	f.m.mu.Lock()
	remaining := len(f.m.historyGeometry)
	f.m.mu.Unlock()
	if remaining != 0 {
		t.Fatal("settled forget retained geometry")
	}
}

func TestHistoryReadRefreshesCachedRespawnIdentity(t *testing.T) {
	f := newFixture(t)
	f.addPane("p", 100, "/dev/pts/1", 1)
	stale := historyRead(t, f, "p")
	f.herdr.update("p", func(p *fakePane) { p.shellPID = 200 })
	f.ttys.set(200, "/dev/pts/2")
	f.ops.put("/dev/pts/2", &fakeDevice{rdev: 2, char: true, size: Winsize{Columns: 90, Rows: 30}})
	fresh, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p")
	if err != nil {
		t.Fatalf("cached respawn identity did not refresh: %v", err)
	}
	if geometry := finishSafe(t, fresh); geometry.Columns != 90 {
		t.Fatalf("fresh geometry=%+v", geometry)
	}
	finishUnsafe(t, stale)
}

func TestForgottenHistoryGeometryReapedAfterSettling(t *testing.T) {
	for _, phase := range []string{"settling", "in progress"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			f.addPane("p", 100, "/dev/pts/1", 1)
			read := historyRead(t, f, "p")
			var done chan error
			var unblock func()
			if phase == "in progress" {
				entered, release := make(chan struct{}), make(chan struct{})
				done = make(chan error, 1)
				var once sync.Once
				unblock = func() { once.Do(func() { close(release) }) }
				t.Cleanup(unblock)
				f.m.ops = historyBarrierOps{TerminalOps: f.ops, after: func() { close(entered); <-release }}
				go func() { _, _, err := f.m.Acquire(context.Background(), "owner", "p", 60, 20); done <- err }()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("resize did not reach publication barrier")
				}
			} else {
				f.acquire("owner", "p", 60, 20)
			}
			f.m.ForgetHistoryPane("p", "terminal-p")
			f.m.SweepExpired(context.Background())
			if !f.m.HistoryReadUnsafe("p", "terminal-p") {
				t.Fatal("forget dropped unsafe resize fence")
			}
			finishUnsafe(t, read)
			if unblock != nil {
				unblock()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			f.m.SweepExpired(context.Background())
			if !f.m.HistoryReadUnsafe("p", "terminal-p") {
				t.Fatal("sweep dropped settle fence")
			}
			f.clock.Advance(HistorySettleWindow)
			f.m.SweepExpired(context.Background())
			f.m.mu.Lock()
			remaining := len(f.m.historyGeometry)
			f.m.mu.Unlock()
			if remaining != 0 {
				t.Fatal("sweep retained forgotten settled geometry")
			}
		})
	}
}

func TestRestoreCleanupPreservesForgottenHistoryGeometry(t *testing.T) {
	for _, departure := range []string{"settling", "settled", "present"} {
		t.Run(departure, func(t *testing.T) {
			f := newFixture(t)
			f.addPane("p", 100, "/dev/pts/1", 1)
			f.acquire("owner", "p", 60, 20)
			if departure == "settled" {
				f.clock.Advance(HistorySettleWindow)
			}
			if departure != "present" {
				f.m.ForgetHistoryPane("p", "terminal-p")
			}
			if err := f.m.Release(context.Background(), "owner", "p", true); err != nil {
				t.Fatal(err)
			}
			if f.state("p") != nil {
				t.Fatal("restore cleanup retained lease ownership")
			}
			if !f.m.HistoryReadUnsafe("p", "terminal-p") {
				t.Fatal("restore cleanup lost its settle fence")
			}
			f.clock.Advance(HistorySettleWindow)
			f.m.SweepExpired(context.Background())
			f.m.mu.Lock()
			remaining := f.m.historyGeometry["p"]
			f.m.mu.Unlock()
			if departure == "present" {
				if remaining == nil {
					t.Fatal("sweep removed geometry for a present agent")
				}
			} else if remaining != nil {
				t.Fatalf("restore cleanup revived forgotten geometry: %+v", remaining)
			}
		})
	}
}

func TestReusedForgottenHistoryGeometrySurvivesSweep(t *testing.T) {
	for _, reuse := range []string{"read", "size change", "state lookup", "failed read"} {
		t.Run(reuse, func(t *testing.T) {
			f := newFixture(t)
			f.addPane("p", 100, "/dev/pts/1", 1)
			f.acquire("owner", "p", 60, 20)
			f.m.mu.Lock()
			original := f.m.historyGeometry["p"]
			f.m.mu.Unlock()
			f.m.ForgetHistoryPane("p", "terminal-p")
			switch reuse {
			case "read":
				finishUnsafe(t, historyRead(t, f, "p"))
			case "size change":
				f.acquire("owner", "p", 70, 20)
			case "state lookup":
				f.m.mu.Lock()
				f.m.historyStateLocked("p", original.identity)
				f.m.mu.Unlock()
			case "failed read":
				f.ops.mu.Lock()
				f.ops.devices["/dev/pts/1"].size.Columns = 0
				f.ops.mu.Unlock()
				if read, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p"); err == nil {
					_ = read.Close()
					t.Fatal("zero-width read succeeded")
				}
			}
			f.clock.Advance(HistorySettleWindow)
			f.m.SweepExpired(context.Background())
			f.m.mu.Lock()
			remaining := f.m.historyGeometry["p"]
			f.m.mu.Unlock()
			if remaining != original {
				t.Fatal("sweep reaped reused geometry")
			}
		})
	}
}

func TestForgetHistoryGeometryDoesNotDropNewTerminalOrLeaseCleanup(t *testing.T) {
	f := newFixture(t)
	f.addPane("p", 100, "/dev/pts/1", 1)
	f.acquire("owner", "p", 60, 20)
	f.clock.Advance(4 * time.Second)
	read := historyRead(t, f, "p")
	f.m.ForgetHistoryPane("p", "terminal-other")
	finishSafe(t, read)
	forgotten := historyRead(t, f, "p")
	f.m.ForgetHistoryPane("p", "terminal-p")
	finishUnsafe(t, forgotten)
	if f.state("p") == nil {
		t.Fatal("forget removed lease cleanup ownership")
	}
	f.herdr.update("p", func(p *fakePane) { p.terminalID = "terminal-new" })
	newRead, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-new")
	if err != nil {
		t.Fatal(err)
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
	finishSafe(t, newRead)
	if on, _ := f.m.LeaseHistoryState("p", "terminal-new"); on {
		t.Fatal("old lease enabled a replacement terminal")
	}
	if err := f.m.Release(context.Background(), "owner", "p", true); err != nil {
		t.Fatal(err)
	}
	if f.state("p") != nil {
		t.Fatal("replacement cleanup was not dropped")
	}
	f.m.ForgetHistoryPane("p", "terminal-new")
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if len(f.m.historyGeometry) != 0 {
		t.Fatal("confirmed disappearance retained geometry")
	}
}
