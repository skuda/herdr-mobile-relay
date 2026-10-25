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
	after    func(int)
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
	path, start, rdev := r.path, r.start, r.rdev
	if r.after != nil {
		r.after(r.calls)
	}
	return path, start, rdev, nil
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

func TestForegroundFingerprintTurnoverPreservesTerminal(t *testing.T) {
	for _, operation := range []string{"acquisition", "renewal", "reconcile", "restore"} {
		for _, target := range []string{"same-terminal", "other-path", "other-device"} {
			t.Run(operation+"/"+target, func(t *testing.T) {
				f := newFixture(t)
				device := f.addPane("p", 100, "/dev/pts/1", 1)
				f.herdr.update("p", func(p *fakePane) { p.shellPID = 0; p.fgPID = 200 })
				r := &fingerprintTTYResolver{TTYResolver: f.ttys, start: 10, path: "/dev/pts/1", rdev: 1}
				f.m.resolver = r
				turnover := func() {
					f.herdr.update("p", func(p *fakePane) { p.fgPID = 201 })
					r.start = 20
					if target == "other-path" {
						r.path = "/dev/pts/2"
					}
					if target == "other-device" {
						r.rdev = 2
					}
				}
				if operation == "acquisition" {
					r.after = func(call int) {
						if call == 1 {
							turnover()
						}
					}
				} else {
					f.acquire("owner", "p", 60, 20)
					turnover()
				}
				var err error
				switch operation {
				case "acquisition", "renewal":
					_, _, err = f.m.Acquire(context.Background(), "owner", "p", 70, 20)
				case "reconcile":
					err = f.m.reconcileActive(context.Background(), "p", f.state("p"))
				case "restore":
					err = f.m.restoreOnce(context.Background(), "p", f.state("p"))
				}
				if target != "same-terminal" {
					if !errors.Is(err, ErrTerminalGone) {
						t.Errorf("changed foreground TTY outcome=%v, want gone", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("same-TTY foreground turnover rejected %s: %v", operation, err)
				}
				if f.state("p") == nil {
					t.Error("same-TTY foreground turnover dropped lease state")
				}
				if operation == "restore" && f.size(device) != (Winsize{Rows: 40, Columns: 120, XPixel: 960, YPixel: 640}) {
					t.Errorf("foreground turnover skipped restore: %+v", f.size(device))
				}
			})
		}
	}
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
