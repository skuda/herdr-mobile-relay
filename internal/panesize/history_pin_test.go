package panesize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
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

func TestHistoryTerminalReplacedUsesRetainedStateWithoutTTYWork(t *testing.T) {
	f, parts := historyCountFixture(t, false)
	before := parts.counts()
	if (*Manager)(nil).HistoryTerminalReplaced("p", "terminal-p") || f.m.HistoryTerminalReplaced("p", "terminal-p") {
		t.Error("absent manager or geometry reported a replacement")
	}
	if parts.counts() != before {
		t.Error("absent-state query performed terminal work")
	}
	f.acquire("owner", "p", 60, 20)
	before = parts.counts()
	if f.m.HistoryTerminalReplaced("p", "terminal-p") || f.m.HistoryTerminalReplaced("p", "") {
		t.Error("matching or empty terminal ID reported a replacement")
	}
	if !f.m.HistoryTerminalReplaced("p", "another-terminal") {
		t.Error("different retained terminal ID did not report a replacement")
	}
	if parts.counts() != before {
		t.Error("retained-state query performed terminal work")
	}
}

func TestHistoryReadFenceTracksLeaseTargetChangesWithoutTTYWork(t *testing.T) {
	for _, client := range []string{"owner", "second-client"} {
		t.Run(client, func(t *testing.T) {
			f, p := historyCountFixture(t, true)
			beforeCalls := p.counts()
			before := f.m.HistoryReadFence("p", "terminal-p")
			if again := f.m.HistoryReadFence("p", "terminal-p"); again != before || p.counts() != beforeCalls {
				t.Fatal("fence changed or performed terminal work without a mutation")
			}
			_, revision := f.m.LeaseHistoryState("p", "terminal-p")
			f.acquire(client, "p", 50, 20)
			afterWrites := p.counts()
			after := f.m.HistoryReadFence("p", "terminal-p")
			if _, next := f.m.LeaseHistoryState("p", "terminal-p"); next != revision {
				t.Fatal("lease renewal changed the lifecycle revision")
			}
			if before == after {
				t.Error("lease target resize did not change the history fence")
			}
			if p.counts() != afterWrites {
				t.Error("fence read performed terminal work")
			}
		})
	}
}

func TestHistoryReadFenceTracksGeometryStateLifecycle(t *testing.T) {
	f, p := historyCountFixture(t, false)
	absent := f.m.HistoryReadFence("p", "terminal-p")
	if absent != f.m.HistoryReadFence("p", "terminal-p") || absent != (*Manager)(nil).HistoryReadFence("p", "terminal-p") {
		t.Fatal("absent geometry does not have a stable zero fence")
	}
	f.acquire("owner", "p", 60, 20)
	created := f.m.HistoryReadFence("p", "terminal-p")
	if created == absent || f.m.HistoryReadFence("p", "another-terminal") != absent {
		t.Fatal("fence does not qualify state creation by terminal")
	}
	f.m.historyUnknown("p", "terminal-p")
	unknown := f.m.HistoryReadFence("p", "terminal-p")
	if unknown == created {
		t.Error("unknown geometry did not change the fence")
	}
	f.clock.Advance(4 * time.Second)
	f.m.ForgetHistoryPane("p", "terminal-p")
	deleted := f.m.HistoryReadFence("p", "terminal-p")
	if deleted.state != nil || deleted.revision != 0 || !deleted.leased || deleted == unknown {
		t.Error("deletion did not clear geometry while retaining the lease-history gate")
	}
	f.acquire("owner", "p", 50, 20)
	recreated := f.m.HistoryReadFence("p", "terminal-p")
	if recreated == created || recreated == unknown || recreated == absent {
		t.Error("recreated state reused an earlier fence")
	}
	beforeCalls := p.counts()
	f.m.mu.Lock()
	identity := f.m.historyGeometry["p"].identity
	identity.shellPID++
	f.m.historyStateLocked("p", identity)
	f.m.mu.Unlock()
	if replaced := f.m.HistoryReadFence("p", "terminal-p"); replaced == recreated || replaced == absent {
		t.Error("same-terminal state replacement did not change the fence")
	}
	if p.counts() != beforeCalls {
		t.Error("in-memory replacement and fence read performed terminal work")
	}
}

func TestHistoryReadFenceAbsentGeometryDetectsRestoreGateChange(t *testing.T) {
	f, _ := historyCountFixture(t, true)
	f.m.ForgetHistoryPane("p", "terminal-p")
	before := f.m.HistoryReadFence("p", "terminal-p")
	f.m.mu.Lock()
	absent := f.m.historyGeometry["p"] == nil
	f.m.mu.Unlock()
	if !absent {
		t.Fatal("fixture did not start with absent history geometry")
	}
	if leased, _ := f.m.LeaseHistoryState("p", "terminal-p"); !leased {
		t.Fatal("fixture did not start with the lease-history gate on")
	}
	if err := f.m.Release(context.Background(), "owner", "p", true); err != nil {
		t.Fatal(err)
	}
	if during := f.m.HistoryReadFence("p", "terminal-p"); during == before {
		t.Fatal("restore did not create geometry state")
	}
	f.clock.Advance(HistorySettleWindow + time.Second)
	f.m.SweepExpired(context.Background())
	if after := f.m.HistoryReadFence("p", "terminal-p"); after == before {
		t.Fatal("lease-history gate change did not change the absent-geometry fence")
	}
	f.m.mu.Lock()
	absent = f.m.historyGeometry["p"] == nil
	f.m.mu.Unlock()
	if !absent {
		t.Fatal("restore and sweep did not remove the forgotten geometry state")
	}
	if leased, _ := f.m.LeaseHistoryState("p", "terminal-p"); leased {
		t.Fatal("fixture did not finish with the lease-history gate off")
	}
}

func TestHistoryReadFenceRetainsForgottenReconcileStateWhileLeased(t *testing.T) {
	f, _ := historyCountFixture(t, true)
	f.m.ForgetHistoryPane("p", "terminal-p")
	before := f.m.HistoryReadFence("p", "terminal-p")
	f.m.mu.Lock()
	absent := f.m.historyGeometry["p"] == nil
	f.m.mu.Unlock()
	if !absent {
		t.Fatal("fixture did not start with absent history geometry")
	}
	f.ops.mu.Lock()
	device := f.ops.devices["/dev/pts/1"]
	device.size.Columns = 100
	writes := len(device.writes)
	f.ops.mu.Unlock()
	state := f.state("p")
	if err := state.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := f.m.reconcileActive(context.Background(), "p", state)
	state.release()
	if err != nil {
		t.Fatal(err)
	}
	f.ops.mu.Lock()
	resized := len(device.writes) == writes+1 && device.size.Columns == 60
	f.ops.mu.Unlock()
	if !resized {
		t.Fatal("reconcile did not resize the terminal")
	}
	f.m.mu.Lock()
	recreated := f.m.historyGeometry["p"]
	forgotten := recreated != nil && recreated.forgotten
	f.m.mu.Unlock()
	if !forgotten {
		t.Fatal("reconcile did not recreate forgotten geometry")
	}
	f.clock.Advance(HistorySettleWindow + time.Second)
	f.m.SweepExpired(context.Background())
	f.m.mu.Lock()
	retained := f.m.historyGeometry["p"] == recreated
	f.m.mu.Unlock()
	if !retained {
		t.Error("sweep removed forgotten geometry while the lease-history gate was on")
	}
	if after := f.m.HistoryReadFence("p", "terminal-p"); after == before {
		t.Error("reconcile resize did not change the history fence")
	}
	if leased, _ := f.m.LeaseHistoryState("p", "terminal-p"); !leased {
		t.Error("fixture did not retain its lease-history gate")
	}
}

func TestHistoryReadFenceResidualAcquireForgetRestoreSweep(t *testing.T) {
	f, _ := historyCountFixture(t, false)
	before := f.m.HistoryReadFence("p", "terminal-p")
	if before != (HistoryFence{}) {
		t.Fatal("fixture did not start absent and unleased")
	}
	f.acquire("owner", "p", 60, 20)
	f.clock.Advance(HistorySettleWindow + time.Second)
	f.m.ForgetHistoryPane("p", "terminal-p")
	if err := f.m.Release(context.Background(), "owner", "p", true); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(HistorySettleWindow + time.Second)
	f.m.SweepExpired(context.Background())
	if after := f.m.HistoryReadFence("p", "terminal-p"); after != before {
		t.Error("absent and unleased endpoints no longer compare equal")
	}
	f.m.mu.Lock()
	absent := f.m.historyGeometry["p"] == nil
	f.m.mu.Unlock()
	if leased, _ := f.m.LeaseHistoryState("p", "terminal-p"); leased || !absent {
		t.Error("fixture did not finish absent and unleased")
	}
	f.ops.mu.Lock()
	restored := f.ops.devices["/dev/pts/1"].size.Columns == 120 && len(f.ops.devices["/dev/pts/1"].writes) == 2
	f.ops.mu.Unlock()
	if !restored {
		t.Error("residual sequence did not apply and restore the leased size")
	}
}

func TestHistoryDisabledPinReadCounts(t *testing.T) {
	f, p := historyCountFixture(t, false)
	f.m.disableHistoryPin = true
	sample := func(name string, want [7]int32) {
		t.Helper()
		before := p.counts()
		finishSafe(t, historyRead(t, f, "p"))
		got := historyCountDelta(before, p.counts())
		t.Logf("%s [get process resolver open device size close]=%v", name, got)
		if got != want {
			t.Errorf("%s calls=%v want %v", name, got, want)
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

type historyCloseActionTerminal struct {
	Terminal
	onClose func()
}

func (t historyCloseActionTerminal) Close() error {
	t.onClose()
	return t.Terminal.Close()
}

func TestHistoryReadGateOffRejectsReplacementDuringCloseDrain(t *testing.T) {
	if !platformHistoryPin {
		t.Skip("manager pin platform")
	}
	f, parts := historyCountFixture(t, true)
	borrow := historyRead(t, f, "p")
	if err := f.m.Release(context.Background(), "owner", "p", true); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(HistorySettleWindow + time.Second)
	f.addPane("other", 200, "/dev/pts/2", 2)
	f.acquire("owner", "other", 60, 20)
	f.clock.Advance(HistorySettleWindow + time.Second)
	finishSafe(t, historyRead(t, f, "other"))
	replacements := 0
	f.m.mu.Lock()
	f.m.retireHistoryPinLocked(f.m.historyGeometry["p"])
	other := f.m.historyGeometry["other"]
	other.pin.terminal = historyCloseActionTerminal{Terminal: other.pin.terminal, onClose: func() {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		identity := f.m.historyGeometry["p"].identity
		identity.terminalID = "terminal-B"
		f.m.historyStateLocked("p", identity)
		replacements++
	}}
	f.m.retireHistoryPinLocked(other)
	f.m.mu.Unlock()
	read, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p")
	if read != nil {
		_ = read.Close()
	}
	if replacements != 1 {
		t.Fatalf("close queue did not replace the geometry once: %d", replacements)
	}
	if read != nil || !errors.Is(err, ErrTerminalGone) || errors.Is(err, ErrHistoryPinPending) {
		t.Errorf("gate-off read treated replacement during close drain as pending: read=%v error=%v", read != nil, err)
	}
	_ = borrow.Close()
	if parts.opens.Load() != parts.closes.Load() {
		t.Errorf("replacement close drain leaked descriptors: %v", parts.counts())
	}
}

type historyPublicationResolver struct {
	TTYResolver
	calls           int
	afterValidation func()
}

func (r *historyPublicationResolver) TTYPath(ctx context.Context, pid int) (string, error) {
	path, err := r.TTYResolver.TTYPath(ctx, pid)
	r.calls++
	if r.calls == 2 && err == nil {
		r.afterValidation()
	}
	return path, err
}

func TestHistoryPinColdPublicationRejectsReplacementGeometry(t *testing.T) {
	if !platformHistoryPin {
		t.Skip("manager pin platform")
	}
	f, parts := historyCountFixture(t, true)
	replaced := false
	f.m.resolver = &historyPublicationResolver{TTYResolver: parts, afterValidation: func() {
		f.herdr.update("p", func(pane *fakePane) { pane.terminalID = "terminal-B" })
		if _, _, err := f.m.Acquire(context.Background(), "owner", "p", 60, 20); !errors.Is(err, ErrTerminalGone) {
			t.Fatalf("old lease did not reject the replacement: %v", err)
		}
		f.acquire("owner", "p", 60, 20)
		replaced = true
	}}
	read, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p")
	if read != nil {
		_ = read.Close()
	}
	if !replaced || read != nil || !errors.Is(err, ErrTerminalGone) || errors.Is(err, ErrHistoryPinPending) {
		t.Errorf("replacement geometry did not reject the cold read as terminal gone: read=%v error=%v replaced=%v", read != nil, err, replaced)
	}
	f.m.mu.Lock()
	current := f.m.historyGeometry["p"]
	currentTerminal := current.identity.terminalID
	pending := len(f.m.historyResolving)
	f.m.mu.Unlock()
	if currentTerminal != "terminal-B" || pending != 0 || parts.opens.Load() != parts.closes.Load() {
		t.Errorf("cold replacement cleanup changed current geometry or leaked work: terminal=%s pending=%d counts=%v", currentTerminal, pending, parts.counts())
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

type waitingHistoryContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *waitingHistoryContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
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
	defer cancel()
	waiting := &waitingHistoryContext{Context: ctx, waiting: make(chan struct{})}
	follower := make(chan error, 1)
	go func() {
		r, err := f.m.BeginHistoryRead(waiting, "p", "terminal-p")
		if r != nil {
			_ = r.Close()
		}
		follower <- err
	}()
	select {
	case <-waiting.waiting:
	case <-time.After(time.Second):
		t.Fatal("live follower did not enter coalesced wait")
	}
	select {
	case err := <-follower:
		t.Fatalf("follower returned before cancellation: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-follower:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled waiting follower=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting follower did not return promptly after cancellation")
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

func TestHistoryPinStaleTerminalBeginPreservesCurrentPin(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, entry := range []string{"BeginHistoryRead", "beginPinnedHistoryRead"} {
		for _, borrowed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/borrowed=%v", entry, borrowed), func(t *testing.T) {
				f, p := historyCountFixture(t, false)
				f.herdr.update("p", func(p *fakePane) { p.terminalID = "terminal-B" })
				f.acquire("owner", "p", 60, 20)
				f.clock.Advance(HistorySettleWindow)
				current, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-B")
				if err != nil {
					t.Fatal(err)
				}
				if !borrowed {
					finishSafe(t, current)
				}
				before := p.counts()
				var stale *HistoryRead
				if entry == "BeginHistoryRead" {
					stale, err = f.m.BeginHistoryRead(context.Background(), "p", "terminal-A")
				} else {
					stale, err = f.m.beginPinnedHistoryRead(context.Background(), "p", "terminal-A")
				}
				if stale != nil {
					_ = stale.Close()
				}
				if !errors.Is(err, ErrTerminalGone) && !errors.Is(err, ErrHistoryReadUnsafe) {
					t.Errorf("stale terminal outcome=%v", err)
				}
				if p.closes.Load() != before[6] {
					t.Error("stale terminal request closed current pin")
				}
				if borrowed {
					finishSafe(t, current)
				}
				beforeWarm := p.counts()
				next, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-B")
				if err != nil {
					t.Fatal(err)
				}
				finishSafe(t, next)
				if got := historyCountDelta(beforeWarm, p.counts()); got != ([7]int32{0, 0, 0, 0, 0, 2, 0}) {
					t.Errorf("stale terminal request invalidated current pin: warm calls=%v", got)
				}
				f.m.ForgetHistoryPane("p", "terminal-B")
			})
		}
	}
}

func TestHistoryPinReplacementsWaitForPriorTerminalBorrow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, p := historyCountFixture(t, true)
	finishSafe(t, historyRead(t, f, "p"))
	currentID := "terminal-p"
	for _, replacement := range []string{"terminal-B", "terminal-C"} {
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
		oldID := currentID
		go func() { r, err := f.m.BeginHistoryRead(context.Background(), "p", oldID); result <- r; failures <- err }()
		<-entered
		f.m.ForgetHistoryPane("p", currentID)
		f.herdr.update("p", func(p *fakePane) { p.terminalID = replacement })
		_, _, _ = f.m.Acquire(context.Background(), "owner", "p", 60, 20)
		f.acquire("owner", "p", 60, 20)
		f.clock.Advance(HistorySettleWindow)
		before := p.counts()
		next, err := f.m.BeginHistoryRead(context.Background(), "p", replacement)
		if next != nil {
			_ = next.Close()
		}
		if !errors.Is(err, ErrHistoryPinPending) {
			t.Errorf("replacement published before prior borrow drained: %v", err)
		}
		f.m.mu.Lock()
		pins := 0
		for pin := range f.m.historyPins {
			if pin.paneID == "p" {
				pins++
			}
		}
		f.m.mu.Unlock()
		if pins != 1 || p.opens.Load() != before[3] {
			t.Errorf("replacement exceeded one retained pin: pins=%d calls=%v before=%v", pins, p.counts(), before)
		}
		close(release)
		old := <-result
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
		finishUnsafe(t, old)
		p.beforeSize = nil
		fresh, err := f.m.BeginHistoryRead(context.Background(), "p", replacement)
		if err != nil {
			t.Fatal(err)
		}
		finishSafe(t, fresh)
		f.m.mu.Lock()
		pins = 0
		for pin := range f.m.historyPins {
			if pin.paneID == "p" {
				pins++
			}
		}
		f.m.mu.Unlock()
		if pins != 1 {
			t.Errorf("drained replacement pin count=%d want=1", pins)
		}
		currentID = replacement
	}
	f.m.ForgetHistoryPane("p", currentID)
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

func TestForegroundTurnoverKeepsHistoryGeometryAndPin(t *testing.T) {
	if !platformHistoryPin {
		t.Skip("manager pin platform")
	}
	f := newFixture(t)
	f.addPane("p", 100, "/dev/pts/1", 1)
	f.herdr.update("p", func(p *fakePane) { p.shellPID = 0; p.fgPID = 200 })
	r := &fingerprintTTYResolver{TTYResolver: f.ttys, start: 10, path: "/dev/pts/1", rdev: 1}
	f.m.resolver = r
	f.acquire("owner", "p", 60, 20)
	f.clock.Advance(HistorySettleWindow)
	finishSafe(t, historyRead(t, f, "p"))
	state := f.m.historyGeometry["p"]
	changed, revision := state.geometry.ChangedAt, state.geometry.LeaseRevision
	f.m.historyUnknown("p", "terminal-p")
	f.herdr.update("p", func(p *fakePane) { p.fgPID = 201 })
	r.start = 20
	finishSafe(t, historyRead(t, f, "p"))
	pin := f.m.historyGeometry["p"].pin
	f.acquire("owner", "p", 60, 20)
	got := f.m.historyGeometry["p"]
	if got != state || got.geometry.ChangedAt != changed || got.geometry.LeaseRevision != revision || got.pin != pin || got.identity.processPID != 201 {
		t.Error("foreground turnover replaced geometry, settling, lease revision, or pin")
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
}

type historyCloseFailureTerminal struct {
	Terminal
	calls int
}

func (t *historyCloseFailureTerminal) Close() error {
	t.calls++
	_ = t.Terminal.Close()
	return errors.New("injected close failure")
}

func TestOtherPanePinCloseFailureDoesNotFailFinish(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	f, _ := historyCountFixture(t, true)
	f.addPane("other", 200, "/dev/pts/2", 2)
	f.acquire("owner", "other", 60, 20)
	f.clock.Advance(HistorySettleWindow)
	other := historyRead(t, f, "other")
	finishSafe(t, other)
	read := historyRead(t, f, "p")
	var logs bytes.Buffer
	f.m.logger = slog.New(slog.NewTextHandler(&logs, nil))
	f.m.mu.Lock()
	state := f.m.historyGeometry["other"]
	terminal := &historyCloseFailureTerminal{Terminal: state.pin.terminal}
	state.pin.terminal = terminal
	f.m.retireHistoryPinLocked(state)
	f.m.mu.Unlock()
	if _, err := read.Finish(); err != nil {
		t.Errorf("other pane close failure contaminated Finish: %v", err)
	}
	f.m.closeRetiredHistoryPins()
	if terminal.calls != 1 || strings.Count(logs.String(), "injected close failure") != 1 || !strings.Contains(logs.String(), "pane_id=other") {
		t.Errorf("close failure not logged once with pane attribution: calls=%d log=%s", terminal.calls, logs.String())
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
}

func TestRetiredPinCloseFailureDiagnostic(t *testing.T) {
	if !platformHistoryPin {
		t.Skip("manager pin platform")
	}
	for _, via := range []string{"Close", "shutdown"} {
		t.Run(via, func(t *testing.T) {
			f, _ := historyCountFixture(t, true)
			read := historyRead(t, f, "p")
			var logs bytes.Buffer
			f.m.logger = slog.New(slog.NewTextHandler(&logs, nil))
			f.m.mu.Lock()
			terminal := &historyCloseFailureTerminal{Terminal: read.pin.terminal}
			read.pin.terminal = terminal
			f.m.mu.Unlock()
			if via == "Close" {
				f.m.ForgetHistoryPane("p", "terminal-p")
				defer read.Close()
				_ = read.Close()
			} else {
				if err := f.m.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			f.m.closeRetiredHistoryPins()
			if terminal.calls != 1 || strings.Count(logs.String(), "injected close failure") != 1 || !strings.Contains(logs.String(), "pane_id=p") {
				t.Errorf("retired close failure not logged once: closes=%d log=%s", terminal.calls, logs.String())
			}
		})
	}
}

func TestHistoryDescriptorDeadlineSeparateFromRestore(t *testing.T) {
	if !platformHistoryPin {
		t.Skip("manager pin platform")
	}
	for _, restoreFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "restored", true: "both"}[restoreFailure], func(t *testing.T) {
			f, parts := historyCountFixture(t, true)
			finishSafe(t, historyRead(t, f, "p"))
			entered, release := make(chan struct{}), make(chan struct{})
			var blocked atomic.Bool
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			parts.beforeSize = func() {
				if blocked.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				read, _ := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p")
				if read != nil {
					_ = read.Close()
				}
			}()
			<-entered
			if restoreFailure {
				f.ops.mu.Lock()
				f.ops.devices["/dev/pts/1"].setErr = errors.New("restore failed")
				f.ops.mu.Unlock()
			}
			var logs bytes.Buffer
			f.m.logger = slog.New(slog.NewTextHandler(&logs, nil))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			err := f.m.Shutdown(ctx)
			cancel()
			if err == nil || !strings.Contains(err.Error(), "history descriptors") {
				t.Errorf("missing descriptor deadline: %v", err)
			}
			if !strings.Contains(logs.String(), "history descriptors did not drain at shutdown") {
				t.Error("missing separate history descriptor warning")
			}
			if !restoreFailure && (strings.Contains(err.Error(), "pane sizes") || strings.Contains(logs.String(), "pane sizes")) {
				t.Errorf("descriptor deadline described as restore failure: %v; %s", err, logs.String())
			}
			if restoreFailure && (!strings.Contains(err.Error(), "pane sizes") || !strings.Contains(logs.String(), "pane sizes")) {
				t.Error("missing actual restore diagnostic")
			}
			unblock()
			<-done
		})
	}
}
