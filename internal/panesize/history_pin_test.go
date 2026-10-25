package panesize

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type historyCountParts struct {
	*fixture
	gets, processes, resolves, opens, devices, sizes, closes atomic.Int32
	beforeOpen                                               func()
	beforeSize                                               func()
	sizeErr                                                  atomic.Bool
}

func (p *historyCountParts) PaneGet(ctx context.Context, id string) (*herdr.Pane, error) {
	p.gets.Add(1)
	return p.herdr.PaneGet(ctx, id)
}
func (p *historyCountParts) PaneProcessInfoFresh(ctx context.Context, id string) (*herdr.PaneProcessInfo, error) {
	p.processes.Add(1)
	return p.herdr.PaneProcessInfoFresh(ctx, id)
}
func (p *historyCountParts) TTYPath(ctx context.Context, pid int) (string, error) {
	p.resolves.Add(1)
	return p.ttys.TTYPath(ctx, pid)
}
func (p *historyCountParts) Open(path string) (Terminal, error) {
	p.opens.Add(1)
	if p.beforeOpen != nil {
		p.beforeOpen()
	}
	terminal, err := p.ops.Open(path)
	if err != nil {
		return nil, err
	}
	return historyCountTerminal{terminal, p}, nil
}

type historyCountTerminal struct {
	Terminal
	parts *historyCountParts
}

func (t historyCountTerminal) Device() (uint64, bool, error) {
	t.parts.devices.Add(1)
	return t.Terminal.Device()
}
func (t historyCountTerminal) Size() (Winsize, error) {
	t.parts.sizes.Add(1)
	if t.parts.beforeSize != nil {
		t.parts.beforeSize()
	}
	if t.parts.sizeErr.Load() {
		return Winsize{}, syscall.EIO
	}
	return t.Terminal.Size()
}
func (t historyCountTerminal) Close() error { t.parts.closes.Add(1); return t.Terminal.Close() }
func (p *historyCountParts) counts() [7]int32 {
	return [7]int32{p.gets.Load(), p.processes.Load(), p.resolves.Load(), p.opens.Load(), p.devices.Load(), p.sizes.Load(), p.closes.Load()}
}
func historyCountFixture(t *testing.T, leased bool) (*fixture, *historyCountParts) {
	t.Helper()
	f := newFixture(t)
	f.addPane("p", 100, "/dev/pts/1", 1)
	p := &historyCountParts{fixture: f}
	f.m.provider, f.m.resolver, f.m.ops = p, p, p
	if leased {
		f.acquire("owner", "p", 60, 20)
		f.clock.Advance(4 * time.Second)
	}
	return f, p
}
func historyCountDelta(a, b [7]int32) [7]int32 {
	var d [7]int32
	for i := range d {
		d[i] = b[i] - a[i]
	}
	return d
}
func TestHistoryDisabledPinA2Counts(t *testing.T) {
	f, p := historyCountFixture(t, false)
	f.m.disableHistoryPin = true
	sample := func(name string, want [7]int32) {
		t.Helper()
		before := p.counts()
		finishSafe(t, historyRead(t, f, "p"))
		got := historyCountDelta(before, p.counts())
		t.Logf("%s [get process resolver open device size close]=%v", name, got)
		if got != want {
			t.Errorf("%s calls=%v want A2=%v", name, got, want)
		}
	}
	sample("cold", [7]int32{2, 2, 2, 1, 1, 2, 1})
	sample("warm", [7]int32{1, 1, 1, 1, 1, 2, 1})
	f.herdr.update("p", func(p *fakePane) { p.shellPID = 200 })
	f.ttys.set(200, "/dev/pts/2")
	f.ops.put("/dev/pts/2", &fakeDevice{rdev: 2, char: true, size: Winsize{Columns: 90, Rows: 30}})
	sample("recovery", [7]int32{3, 3, 2, 2, 2, 2, 2})
}

func TestHistoryDisabledPinLeasedReadsStayReadOwned(t *testing.T) {
	f, p := historyCountFixture(t, true)
	f.m.disableHistoryPin = true
	before := p.counts()
	for range 3 {
		finishSafe(t, historyRead(t, f, "p"))
	}
	if got := historyCountDelta(before, p.counts()); got != ([7]int32{3, 3, 3, 3, 3, 6, 3}) {
		t.Errorf("disabled leased reads=%v, want read-owned warm validation", got)
	}
}

func TestHistoryPinWarmReadsBorrowOneDescriptor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	before := p.counts()
	finishSafe(t, historyRead(t, f, "p"))
	for range 4 {
		finishSafe(t, historyRead(t, f, "p"))
	}
	got := historyCountDelta(before, p.counts())
	if want := [7]int32{2, 2, 2, 1, 1, 10, 0}; got != want {
		t.Errorf("pinned reads [get process resolver open device size close]=%v want=%v", got, want)
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
	if got := historyCountDelta(before, p.counts()); got[6] != 1 {
		t.Errorf("Forget pin closes=%d want=1", got[6])
	}
}
func TestHistoryPinBorrowersDoNotCloseSibling(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	a, b := historyRead(t, f, "p"), historyRead(t, f, "p")
	closed := p.closes.Load()
	_ = a.Close()
	finishSafe(t, b)
	if p.closes.Load() != closed {
		t.Error("read completion closed the shared pin")
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
	if p.closes.Load() != closed+1 {
		t.Error("Forget did not close the pin once")
	}
}
func TestHistoryPinInvalidationDefersCloseUntilBorrowRelease(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	a, b := historyRead(t, f, "p"), historyRead(t, f, "p")
	closed := p.closes.Load()
	f.m.ForgetHistoryPane("p", "terminal-p")
	if p.closes.Load() != closed {
		t.Error("invalidation closed under borrowers")
	}
	if next, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p"); err == nil {
		_ = next.Close()
		t.Error("retired pin allowed a replacement while borrowers remain")
	}
	finishUnsafe(t, a)
	if p.closes.Load() != closed {
		t.Error("first borrower closed sibling descriptor")
	}
	finishUnsafe(t, b)
	if p.closes.Load() != closed+1 {
		t.Error("last borrower did not close retired pin")
	}
}
func TestHistoryPinConcurrentFinishCloseCancellation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := f.m.BeginHistoryRead(ctx, "p", "terminal-p")
	if err != nil {
		t.Fatal(err)
	}
	b := historyRead(t, f, "p")
	closed := p.closes.Load()
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%3 == 0 {
				cancel()
			} else if i%3 == 1 {
				_ = a.Close()
			} else {
				_, _ = a.Finish()
			}
		}()
	}
	wg.Wait()
	finishSafe(t, b)
	if p.closes.Load() != closed {
		t.Error("cancelled borrower closed valid manager pin")
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
	if p.closes.Load() != closed+1 {
		t.Error("concurrent completions released borrow more than once")
	}
}
func TestHistoryPinColdReadersCoalesce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.beforeOpen = func() { once.Do(func() { close(entered); <-release }) }
	before := p.counts()
	results := make(chan *HistoryRead, 8)
	failures := make(chan error, 8)
	for range 8 {
		go func() {
			r, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p")
			results <- r
			failures <- err
		}()
	}
	<-entered
	close(release)
	for range 8 {
		r := <-results
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
		finishSafe(t, r)
	}
	if got := historyCountDelta(before, p.counts()); got != ([7]int32{2, 2, 2, 1, 1, 16, 0}) {
		t.Errorf("cold concurrent calls=%v", got)
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
}

func TestHistoryPinBlockedSampleInvalidationAndShutdown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, operation := range []string{"forget", "shutdown", "shutdown-deadline"} {
		t.Run(operation, func(t *testing.T) {
			f, p := historyCountFixture(t, true)
			finishSafe(t, historyRead(t, f, "p"))
			entered, release := make(chan struct{}), make(chan struct{})
			var blocked atomic.Bool
			p.beforeSize = func() {
				if blocked.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}
			result := make(chan *HistoryRead, 1)
			failures := make(chan error, 1)
			go func() {
				r, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p")
				result <- r
				failures <- err
			}()
			<-entered
			closed := p.closes.Load()
			if operation == "forget" {
				f.m.ForgetHistoryPane("p", "terminal-p")
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				if err := f.m.Shutdown(ctx); err == nil {
					t.Error("shutdown claimed closure with blocked sample")
				}
				cancel()
			}
			if p.closes.Load() != closed && operation == "forget" {
				t.Error("closed pin under blocked Size")
			}
			close(release)
			r := <-result
			err := <-failures
			if err == nil {
				finishUnsafe(t, r)
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("blocked begin error=%v", err)
			}
			if operation != "forget" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := f.m.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if p.opens.Load() != p.closes.Load() {
				t.Errorf("released sample leaked descriptors: %v", p.counts())
			}
		})
	}
}

func TestHistoryPinQuietExpiryAndShutdownBorrowRelease(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, operation := range []string{"quiet-expiry", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			f, p := historyCountFixture(t, true)
			r := historyRead(t, f, "p")
			if operation == "quiet-expiry" {
				finishSafe(t, r)
				_ = f.m.Release(context.Background(), "owner", "p", true)
				f.clock.Advance(HistorySettleWindow)
				f.m.SweepExpired(context.Background())
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := f.m.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Finish(); !errors.Is(err, context.Canceled) {
					t.Errorf("shutdown read=%v", err)
				}
			}
			if p.opens.Load() != p.closes.Load() {
				t.Errorf("lifecycle descriptor leak: %v", p.counts())
			}
		})
	}
}

func TestHistoryPinColdPublicationRefusesObsoleteResolution(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, change := range []string{"cancel", "forget", "gate-transition", "terminal-replacement", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			f, p := historyCountFixture(t, true)
			entered, release := make(chan struct{}), make(chan struct{})
			var blocked atomic.Bool
			p.beforeOpen = func() {
				if blocked.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				r, err := f.m.BeginHistoryRead(ctx, "p", "terminal-p")
				if r != nil {
					_ = r.Close()
				}
				done <- err
			}()
			<-entered
			switch change {
			case "cancel":
				cancel()
			case "forget":
				f.m.ForgetHistoryPane("p", "terminal-p")
			case "gate-transition":
				_ = f.m.Release(context.Background(), "owner", "p", true)
			case "terminal-replacement":
				f.herdr.update("p", func(p *fakePane) { p.terminalID = "terminal-new" })
			case "shutdown":
				shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
				if err := f.m.Shutdown(shutdownCtx); err == nil {
					t.Error("shutdown claimed drained blocked resolver")
				}
				stop()
			}
			close(release)
			if err := <-done; err == nil {
				t.Error("obsolete cold resolver published a pin")
			}
			if p.opens.Load() != p.closes.Load() {
				t.Errorf("unpublished descriptor leaked: %v", p.counts())
			}
		})
	}
}

func TestHistoryPinFreshCaptureAndColdAcquireFailureInvalidate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, outcome := range []string{"capture-changed", "capture-failed", "created-acquire-failed"} {
		t.Run(outcome, func(t *testing.T) {
			f, p := historyCountFixture(t, true)
			r := historyRead(t, f, "p")
			if outcome == "capture-changed" {
				f.herdr.update("p", func(p *fakePane) { p.shellPID = 200 })
				f.ttys.set(200, "/dev/pts/2")
				f.ops.put("/dev/pts/2", &fakeDevice{rdev: 2, char: true, size: Winsize{Columns: 90, Rows: 30}})
				identity, err := f.m.lookupIdentity(context.Background(), "p")
				if err != nil {
					t.Fatal(err)
				}
				terminal, err := f.m.openValidated(context.Background(), "p", &identity, true)
				if err != nil {
					t.Fatal(err)
				}
				_ = terminal.Close()
			} else {
				f.herdr.setFailure("p", errors.New("identity unavailable"))
				if outcome == "created-acquire-failed" {
					f.m.mu.Lock()
					old := f.m.panes["p"]
					f.m.dropLocked("p", old)
					f.m.mu.Unlock()
					_, _, _ = f.m.Acquire(context.Background(), "new-owner", "p", 60, 20)
				} else {
					identity := terminalIdentity{terminalID: "terminal-p", shellPID: 100, ttyPath: "/dev/pts/1"}
					if terminal, err := f.m.openValidated(context.Background(), "p", &identity, true); err == nil {
						_ = terminal.Close()
						t.Error("failed capture succeeded")
					}
				}
			}
			finishUnsafe(t, r)
			if p.opens.Load() != p.closes.Load() {
				t.Errorf("fresh validation left old pin open: %v", p.counts())
			}
		})
	}
}

func TestHistoryPinSettlingShutdownWithoutLeaseState(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	finishSafe(t, historyRead(t, f, "p"))
	_ = f.m.Release(context.Background(), "owner", "p", true)
	if f.state("p") != nil {
		t.Fatal("restore retained lease state")
	}
	if p.opens.Load() == p.closes.Load() {
		t.Fatal("restore did not retain settle pin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if p.opens.Load() != p.closes.Load() {
		t.Errorf("shutdown missed settle pin: %v", p.counts())
	}
}

func TestHistoryPinCancelledColdFollowerDoesNotCancelOwner(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.beforeOpen = func() { once.Do(func() { close(entered); <-release }) }
	owner := make(chan *HistoryRead, 1)
	ownerErr := make(chan error, 1)
	go func() {
		r, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p")
		owner <- r
		ownerErr <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := f.m.BeginHistoryRead(ctx, "p", "terminal-p"); !errors.Is(err, context.Canceled) {
		if r != nil {
			_ = r.Close()
		}
		t.Errorf("cancelled follower=%v", err)
	}
	close(release)
	r := <-owner
	if err := <-ownerErr; err != nil {
		t.Fatal(err)
	}
	finishSafe(t, r)
	f.m.ForgetHistoryPane("p", "terminal-p")
}

func TestHistoryPinFreshValidationFailuresInvalidate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, failure := range []string{"lookup-unknown", "device", "write", "state-identity", "zero-size"} {
		t.Run(failure, func(t *testing.T) {
			f, p := historyCountFixture(t, true)
			r := historyRead(t, f, "p")
			closed := p.closes.Load()
			switch failure {
			case "lookup-unknown":
				f.herdr.setFailure("p", errors.New("lookup unavailable"))
				_, _, _ = f.m.Acquire(context.Background(), "owner", "p", 60, 20)
			case "device":
				f.ops.put("/dev/pts/1", &fakeDevice{rdev: 2, char: true, size: Winsize{Columns: 60, Rows: 20}})
				_, _, _ = f.m.Acquire(context.Background(), "owner", "p", 60, 20)
			case "write":
				f.ops.mu.Lock()
				f.ops.devices["/dev/pts/1"].setErr = errors.New("write failed")
				f.ops.mu.Unlock()
				_, _, _ = f.m.Acquire(context.Background(), "owner", "p", 70, 20)
			case "state-identity":
				f.m.mu.Lock()
				identity := f.m.historyGeometry["p"].identity
				identity.shellPID = 200
				f.m.historyStateLocked("p", identity)
				f.m.mu.Unlock()
			case "zero-size":
				f.ops.mu.Lock()
				f.ops.devices["/dev/pts/1"].size.Columns = 0
				f.ops.mu.Unlock()
			}
			_, err := r.Finish()
			if err == nil {
				t.Error("invalidated borrower finished safely")
			}
			if p.closes.Load() <= closed {
				t.Error("invalidated last borrower did not close pin")
			}
		})
	}
}

func TestHistoryPinNoOpRestoreRetainsPreSignalWindow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, false)
	f.acquire("owner", "p", 120, 40)
	finishSafe(t, historyRead(t, f, "p"))
	f.herdr.update("p", func(p *fakePane) { p.shellPID = 200 })
	f.ttys.set(200, "/dev/pts/2")
	before := p.counts()
	_ = f.m.Release(context.Background(), "owner", "p", true)
	if got := historyCountDelta(before, p.counts()); got != ([7]int32{}) {
		t.Errorf("no-op restore unexpectedly validated or closed: %v", got)
	}
	f.m.SweepExpired(context.Background())
	if p.closes.Load() != before[6]+1 {
		t.Error("gate-off expiry did not retire no-op restore pin")
	}
}

func TestHistoryPinWriteDescriptorsStayFresh(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, write := range []string{"renewal", "reconcile", "restore", "shutdown"} {
		t.Run(write, func(t *testing.T) {
			f, p := historyCountFixture(t, true)
			finishSafe(t, historyRead(t, f, "p"))
			before := p.counts()
			switch write {
			case "renewal":
				f.acquire("owner", "p", 60, 20)
			case "reconcile":
				if err := f.m.reconcileActive(context.Background(), "p", f.state("p")); err != nil {
					t.Fatal(err)
				}
			case "restore":
				_ = f.m.Release(context.Background(), "owner", "p", true)
			case "shutdown":
				if err := f.m.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			got := historyCountDelta(before, p.counts())
			if got[0] != 1 || got[1] != 1 || got[2] != 1 || got[3] != 1 || got[4] != 1 || got[5] != 1 {
				t.Errorf("%s did not use fresh validated write descriptor: %v", write, got)
			}
			f.m.ForgetHistoryPane("p", "terminal-p")
		})
	}
}

func TestHistoryPinExpiryReacquireAndOldTerminalCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	finishSafe(t, historyRead(t, f, "p"))
	_ = f.m.Release(context.Background(), "owner", "p", true)
	f.clock.Advance(HistorySettleWindow)
	f.m.SweepExpired(context.Background())
	f.acquire("owner", "p", 60, 20)
	f.clock.Advance(HistorySettleWindow)
	finishSafe(t, historyRead(t, f, "p"))
	f.m.ForgetHistoryPane("p", "terminal-p")
	f.herdr.update("p", func(p *fakePane) { p.terminalID = "terminal-new"; p.shellPID = 200 })
	f.ttys.set(200, "/dev/pts/2")
	f.ops.put("/dev/pts/2", &fakeDevice{rdev: 2, char: true, size: Winsize{Columns: 90, Rows: 30}})
	_, _, _ = f.m.Acquire(context.Background(), "new-owner", "p", 60, 20)
	f.acquire("new-owner", "p", 60, 20)
	f.clock.Advance(HistorySettleWindow)
	r, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-new")
	if err != nil {
		t.Fatal(err)
	}
	finishSafe(t, r)
	before := p.counts()
	f.m.ForgetHistoryPane("p", "terminal-p")
	r, err = f.m.BeginHistoryRead(context.Background(), "p", "terminal-new")
	if err != nil {
		t.Fatal(err)
	}
	finishSafe(t, r)
	if got := historyCountDelta(before, p.counts()); got != ([7]int32{0, 0, 0, 0, 0, 2, 0}) {
		t.Errorf("old terminal cleanup affected newer pin: %v", got)
	}
	f.m.ForgetHistoryPane("p", "terminal-new")
}

func TestHistoryPinCancelledBorrowDoesNotCloseBlockedSibling(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := f.m.BeginHistoryRead(ctx, "p", "terminal-p")
	if err != nil {
		t.Fatal(err)
	}
	b := historyRead(t, f, "p")
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	p.beforeSize = func() {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	done := make(chan error, 1)
	go func() { _, err := b.Finish(); done <- err }()
	<-entered
	closed := p.closes.Load()
	cancel()
	_ = a.Close()
	if p.closes.Load() != closed {
		t.Error("cancelled borrow closed descriptor under sibling sample")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
}

func TestHistoryPinFinishedTokenRemainsImmutable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, _ := historyCountFixture(t, true)
	r := historyRead(t, f, "p")
	before := finishSafe(t, r)
	f.acquire("owner", "p", 70, 30)
	if after := finishSafe(t, r); after != before {
		t.Errorf("finished token changed: before=%+v after=%+v", before, after)
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
}

func TestHistoryPinCrossedEpochAndLeaseABA(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, change := range []string{"epoch", "lease-ABA", "settling"} {
		t.Run(change, func(t *testing.T) {
			f, _ := historyCountFixture(t, true)
			if change == "settling" {
				f.acquire("owner", "p", 70, 20)
			}
			r := historyRead(t, f, "p")
			switch change {
			case "epoch":
				f.acquire("owner", "p", 70, 20)
			case "lease-ABA":
				_ = f.m.Release(context.Background(), "owner", "p", true)
				f.acquire("owner", "p", 60, 20)
			}
			f.clock.Advance(HistorySettleWindow)
			finishUnsafe(t, r)
			f.m.ForgetHistoryPane("p", "terminal-p")
		})
	}
}

func TestHistoryDisabledPinLeasedCompletionClosesOwnedDescriptor(t *testing.T) {
	for _, end := range []string{"close", "cancel"} {
		t.Run(end, func(t *testing.T) {
			f, p := historyCountFixture(t, true)
			f.m.disableHistoryPin = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, err := f.m.BeginHistoryRead(ctx, "p", "terminal-p")
			if err != nil {
				t.Fatal(err)
			}
			before := p.closes.Load()
			if end == "cancel" {
				cancel()
			}
			_ = r.Close()
			if p.closes.Load() != before+1 {
				t.Error("disabled pin completion did not close its read-owned descriptor")
			}
		})
	}
}

func TestHistoryPinSameIdentityRespawnSignals(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, signal := range []string{"size-EIO", "renewal", "reconcile", "restore", "forget"} {
		t.Run(signal, func(t *testing.T) {
			f, p := historyCountFixture(t, true)
			old := historyRead(t, f, "p")
			f.herdr.update("p", func(p *fakePane) { p.shellPID = 200 })
			f.ttys.set(200, "/dev/pts/2")
			f.ops.put("/dev/pts/2", &fakeDevice{rdev: 2, char: true, size: Winsize{Columns: 90, Rows: 30}})
			if signal != "size-EIO" {
				before := p.counts()
				window := historyRead(t, f, "p")
				finishSafe(t, window)
				if got := historyCountDelta(before, p.counts()); got != ([7]int32{0, 0, 0, 0, 0, 2, 0}) {
					t.Errorf("pre-signal residual window performed validation: %v", got)
				}
			}
			switch signal {
			case "size-EIO":
				p.sizeErr.Store(true)
				if _, err := old.Finish(); !errors.Is(err, ErrSizeUnavailable) {
					t.Errorf("hangup result=%v", err)
				}
				p.sizeErr.Store(false)
			case "renewal":
				if _, _, err := f.m.Acquire(context.Background(), "owner", "p", 60, 20); err == nil {
					t.Error("respawn renewal succeeded")
				}
			case "reconcile":
				if err := f.m.reconcileActive(context.Background(), "p", f.state("p")); err == nil {
					t.Error("respawn reconcile succeeded")
				}
			case "restore":
				_ = f.m.Release(context.Background(), "owner", "p", true)
			case "forget":
				f.m.ForgetHistoryPane("p", "terminal-p")
			}
			if signal != "size-EIO" {
				finishUnsafe(t, old)
			}
			fresh, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p")
			if err != nil {
				t.Fatal(err)
			}
			if geometry := finishSafe(t, fresh); geometry.Columns != 90 {
				t.Errorf("post-signal refresh=%+v", geometry)
			}
			f.m.ForgetHistoryPane("p", "terminal-p")
		})
	}
}
