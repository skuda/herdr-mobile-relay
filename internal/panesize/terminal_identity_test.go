package panesize

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
)

type fingerprintTTYResolver struct {
	TTYResolver
	start    uint64
	path     string
	rdev     uint64
	fail     error
	pid      int
	calls    int
	changeAt int
}

func (r *fingerprintTTYResolver) ttyIdentity(ctx context.Context, pid int) (string, uint64, uint64, error) {
	r.pid = pid
	r.calls++
	if r.changeAt > 0 && r.calls == r.changeAt {
		r.start++
	}
	if r.fail != nil {
		return "", 0, 0, r.fail
	}
	return r.path, r.start, r.rdev, nil
}
func TestPlatformControllingTTYResolverSelection(t *testing.T) {
	got := fmt.Sprintf("%T", NewManager(&fakeHerdr{}, nil).resolver)
	want := "panesize.psTTYResolver"
	if runtime.GOOS == "linux" {
		want = "panesize.procTTYResolver"
	}
	if got != want {
		t.Errorf("production resolver=%s want=%s", got, want)
	}
}
func TestProcessBirthChangeRefusesSameNumericPID(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("p", 100, "/dev/pts/1", 1)
	r := &fingerprintTTYResolver{TTYResolver: f.ttys, start: 10, path: "/dev/pts/1", rdev: 1}
	f.m.resolver = r
	f.acquire("owner", "p", 60, 20)
	r.start = 11
	if _, _, err := f.m.Acquire(context.Background(), "owner", "p", 70, 20); err == nil {
		t.Error("PID reuse accepted the same numeric PID and TTY")
	}
	if len(f.writes(device)) != 1 {
		t.Error("PID reuse wrote a terminal")
	}
}
func TestProcessBirthChangeBetweenResolveAndPostOpenValidationRefuses(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("p", 100, "/dev/pts/1", 1)
	r := &fingerprintTTYResolver{TTYResolver: f.ttys, start: 10, path: "/dev/pts/1", rdev: 1, changeAt: 2}
	f.m.resolver = r
	if _, _, err := f.m.Acquire(context.Background(), "owner", "p", 60, 20); err == nil {
		t.Error("process replacement between resolve and validation accepted")
	}
	if len(f.writes(device)) != 0 {
		t.Error("uncertain process birth wrote terminal")
	}
	f.assertDescriptorsClosed()
}

func TestProcessFingerprintForegroundFallbackAndPinnedReads(t *testing.T) {
	f := newFixture(t)
	f.addPane("p", 100, "/dev/pts/1", 1)
	f.herdr.update("p", func(p *fakePane) { p.shellPID = 0; p.fgPID = 200 })
	r := &fingerprintTTYResolver{TTYResolver: f.ttys, start: 10, path: "/dev/pts/1", rdev: 1}
	f.m.resolver = r
	f.acquire("owner", "p", 60, 20)
	if r.pid != 200 {
		t.Errorf("fingerprint resolved pid=%d, want foreground 200", r.pid)
	}
	f.clock.Advance(HistorySettleWindow)
	finishSafe(t, historyRead(t, f, "p"))
	before := r.calls
	for range 3 {
		finishSafe(t, historyRead(t, f, "p"))
	}
	if runtime.GOOS == "linux" && r.calls != before {
		t.Errorf("published pin polled process fingerprint: calls=%d want=%d", r.calls, before)
	}
	f.m.ForgetHistoryPane("p", "terminal-p")
}

func TestProcessFingerprintUncertaintyRefusesInitialCapture(t *testing.T) {
	for _, failure := range []string{"process-exit", "device-mismatch"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			f.addPane("p", 100, "/dev/pts/1", 1)
			r := &fingerprintTTYResolver{TTYResolver: f.ttys, start: 10, path: "/dev/pts/1", rdev: 1}
			if failure == "process-exit" {
				r.fail = errors.New("process gone")
			} else {
				r.rdev = 2
			}
			f.m.resolver = r
			if read, err := f.m.BeginHistoryRead(context.Background(), "p", "terminal-p"); err == nil {
				_ = read.Close()
				t.Error("uncertain process/device fingerprint allowed physical capture")
			}
		})
	}
}
