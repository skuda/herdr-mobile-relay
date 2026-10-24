package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
)

type countingHistoryProvider struct {
	*paneSizeFakes
	gets  atomic.Int32
	after func()
}

func (p *countingHistoryProvider) PaneGet(ctx context.Context, paneID string) (*herdr.Pane, error) {
	p.gets.Add(1)
	if p.after != nil {
		p.after()
	}
	return p.paneSizeFakes.PaneGet(ctx, paneID)
}

func TestIneligibleHistoryShapesSkipGeometryAndAttempts(t *testing.T) {
	for _, shape := range []string{"text", "legacy viewport"} {
		t.Run(shape, func(t *testing.T) {
			s, f, _ := captureServer(t, false)
			provider := &countingHistoryProvider{paneSizeFakes: f}
			s.paneSizeM = panesize.NewManagerWith(provider, f, f, s.historyNow, nil)
			pendingDeparture(s, 10)
			sequence := s.historyPending["pane-1"].sequence
			request := captureRequest(s, false)
			if shape == "text" {
				request.Format = "text"
			} else {
				request.Resized = true
			}
			for range 5 {
				_, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
					return herdr.PaneRead{Content: []byte(capturePage("A", "B"))}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			pending := s.historyPending["pane-1"]
			if pending == nil || pending.attempts != 0 || provider.gets.Load() != 0 {
				t.Fatalf("ineligible shape charged history work: pending=%+v geometry lookups=%d", pending, provider.gets.Load())
			}
			result := executeCapture(t, s, capturePage("A", "B"), false)
			if !result.Capture.Admitted || result.Capture.PendingSequence != sequence || s.historyPending["pane-1"] != nil {
				t.Fatal("eligible capture did not acknowledge original request")
			}
		})
	}
}

func TestBackgroundHistoryUsesSchedulingTimeAndOneHandle(t *testing.T) {
	s, f, advance := captureServer(t, false)
	provider := &countingHistoryProvider{paneSizeFakes: f, after: func() { advance(time.Millisecond) }}
	s.paneSizeM = panesize.NewManagerWith(provider, f, f, s.historyNow, nil)
	var reads atomic.Int32
	installPaneReadFixture(t, s, func(context.Context) (string, error) {
		reads.Add(1)
		return capturePage("A", "B"), nil
	})
	s.historyTasks = newLifecycleTasks(context.Background())
	t.Cleanup(s.historyTasks.Stop)
	start := s.historyNow()
	s.scheduleHistoryCapture(context.Background(), "pane-1")
	waitHistoryCapture(t, s)
	advance(start.Add(history.CaptureInterval).Sub(s.historyNow()))
	s.scheduleHistoryCapture(context.Background(), "pane-1")
	waitHistoryCapture(t, s)
	if reads.Load() != 2 {
		t.Fatalf("four-second opportunity skipped after validation delay: reads=%d", reads.Load())
	}
	if got := provider.gets.Load(); got != 3 {
		t.Fatalf("background validation repeated: pane.get calls=%d want=3", got)
	}
}
