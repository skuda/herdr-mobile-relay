package app

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestPaneReadDefersNoEchoUntilFinalFitting(t *testing.T) {
	s, _, _ := captureServer(t, false)
	installPaneReadFixture(t, s, func(context.Context) (string, error) { return "[sudo] password for cv:", nil })
	response := s.readPaneResponse(context.Background(), map[string]any{"pane_id": "pane-1", "format": "ansi"})
	if _, exists := response["no_echo"]; exists {
		t.Fatal("intermediate pane response computed no-echo before final fitting")
	}
	response["target"] = "final target"
	response = fitPaneContentResponse(response)
	if response["no_echo"] != true {
		t.Fatal("final fitting omitted no-echo state")
	}
}

func TestRejectedWatchRetryStopsAfterStableRead(t *testing.T) {
	s, _, advance := captureServer(t, true)
	executeCapture(t, s, capturePage("archive A", "archive B"), true)
	if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 70, 20); err != nil {
		t.Fatal(err)
	}
	result := executeCapture(t, s, capturePage("current A", "current B"), true)
	frame := &paneWatchFrame{classificationAgent: "claude", resizeSettling: result.Response["resize_settling"] == true, historyRetry: result.Response["history_retry"] == true}
	if !paneWatchNeedsFrameRead("same", "same", frame, "claude") {
		t.Fatal("transient rejection did not request unchanged-probe retry")
	}
	advance(panesize.HistorySettleWindow)
	result = executeCapture(t, s, capturePage("current A", "current B"), true)
	frame.historyRetry = result.Response["history_retry"] == true
	frame.resizeSettling = result.Response["resize_settling"] == true
	if !result.Capture.Admitted || paneWatchNeedsFrameRead("same", "same", frame, "claude") {
		t.Fatal("settled admission retained watch retry")
	}
	request := captureRequest(s, false)
	request.Resized = true
	result, err := s.executeHistoryRead(context.Background(), request, func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		return herdr.PaneRead{Content: []byte(capturePage("raw A", "raw B"))}, nil
	})
	if err != nil || result.Response["history_retry"] == true || result.Composed {
		t.Fatalf("deliberate raw viewport requested retry: %+v error=%v", result, err)
	}
}

func TestRejectedHistoryPreviewCannotAttachReplacementArchive(t *testing.T) {
	s, _, _ := captureServer(t, true)
	executeCapture(t, s, capturePage("old archive A", "old archive B"), true)
	result, err := s.executeHistoryRead(context.Background(), captureRequest(s, true), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
		agent, _ := s.state.Agent("pane-1")
		agent.TerminalID = "new terminal"
		s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
		s.syncHistoryPanes()
		return herdr.PaneRead{Content: []byte(capturePage("old raw A", "old raw B"))}, nil
	})
	if err != nil || result.Composed || result.Capture.Admitted || strings.Contains(result.Response["content"].(string), "old archive") {
		t.Fatalf("replacement attached rejected archive: %+v error=%v", result, err)
	}
}

func TestRejectedHistoryReadComposesWithoutAdmission(t *testing.T) {
	for _, physical := range []bool{false, true} {
		s, _, _ := captureServer(t, physical)
		// Replaces fallback-generated history with supported forward retirement.
		executeCapture(t, s, capturePage("archive A", "archive B", "current A", "current B"), physical)
		executeCapture(t, s, capturePage("current A", "current B"), physical)
		stored := s.historyM.Content("pane-1", 0)
		pendingDeparture(s, 10)
		sequence := s.historyPending["pane-1"].sequence
		result, err := s.executeHistoryRead(context.Background(), captureRequest(s, physical), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
			agent, _ := s.state.Agent("pane-1")
			agent.ScrollMaxOffset = 10
			s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
			agent.ScrollMaxOffset = 0
			s.state.CommitInventory([]*coordinator.AgentState{agent}, s.state.RevisionCounter())
			return herdr.PaneRead{Content: []byte(capturePage("current A", "current B"))}, nil
		})
		if err != nil || result.Capture.Admitted {
			t.Fatalf("rejected capture admitted=%v error=%v", result.Capture.Admitted, err)
		}
		if content, _ := result.Response["content"].(string); !strings.Contains(content, "archive A") {
			t.Fatalf("rejected display lost archive: %q", content)
		}
		if got := s.historyM.Content("pane-1", 0); got != stored {
			t.Fatalf("preview mutated retained content: %q", got)
		}
		if pending := s.historyPending["pane-1"]; pending == nil || pending.sequence != sequence || s.historyAcknowledgements != 0 {
			t.Fatalf("preview acknowledged pending request: %+v", pending)
		}
	}
}

type pendingHistoryReadGeometry struct {
	*paneSizeFakes
	beforeOpen func()
	terminalID atomic.Value
}

func (g *pendingHistoryReadGeometry) Open(string) (panesize.Terminal, error) {
	if g.beforeOpen != nil {
		g.beforeOpen()
	}
	return g, nil
}

func (g *pendingHistoryReadGeometry) PaneGet(ctx context.Context, id string) (*herdr.Pane, error) {
	p, err := g.paneSizeFakes.PaneGet(ctx, id)
	if v := g.terminalID.Load(); v != nil {
		p.TerminalID = v.(string)
	}
	return p, err
}

func TestBeginHistoryReadPendingPinKeepsPreview(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pin gate")
	}
	for _, kind := range []string{"draining", "obsolete"} {
		t.Run(kind, func(t *testing.T) {
			s, f, advance := captureServer(t, false)
			g := &pendingHistoryReadGeometry{paneSizeFakes: f}
			s.paneSizeM = panesize.NewManagerWith(g, g, g, s.historyNow, nil)
			if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20); err != nil {
				t.Fatal(err)
			}
			advance(4 * time.Second)
			executeCapture(t, s, capturePage("archive A", "archive B", "current C", "current D"), true)
			executeCapture(t, s, capturePage("current C", "current D"), true)
			stored := s.historyM.Content("pane-1", 0)
			borrow, err := s.paneSizeM.BeginHistoryRead(context.Background(), "pane-1", "terminal-1")
			if err != nil {
				t.Fatal(err)
			}
			s.paneSizeM.ForgetHistoryPane("pane-1", "terminal-1")
			if kind == "obsolete" {
				_ = borrow.Close()
				var once sync.Once
				g.beforeOpen = func() { once.Do(func() { s.paneSizeM.ForgetHistoryPane("pane-1", "terminal-1") }) }
			}
			t.Cleanup(func() { _ = borrow.Close() })
			result := executeCapture(t, s, capturePage("current C", "current D"), true)
			if result.Capture.Admitted || !result.Composed || result.Response["resize_settling"] != true || !strings.Contains(result.Response["content"].(string), "archive A") {
				t.Errorf("transient Begin refusal lost preview/settling: %+v", result)
			}
			if s.historyM.Content("pane-1", 0) != stored {
				t.Error("transient preview mutated history")
			}
			frame := &paneWatchFrame{classificationAgent: "claude", resizeSettling: result.Response["resize_settling"] == true, historyRetry: result.Response["history_retry"] == true}
			if !paneWatchNeedsFrameRead("same", "same", frame, "claude") {
				t.Error("transient Begin refusal did not retry unchanged probe")
			}
		})
	}
}

func TestBoundaryPreviewDoesNotAcknowledgePending(t *testing.T) {
	s, _, _ := captureServer(t, false)
	body := []string{"Amber lanterns guide quiet ships beside distant rocky shores.", "Patient keepers tend bright lamps through heavy winter storms."}
	executeCapture(t, s, capturePage(body...), false)
	pendingDeparture(s, 10)
	sequence := s.historyPending["pane-1"].sequence
	stored := s.historyM.Content("pane-1", 0)
	if _, _, err := s.paneSizeM.Acquire(context.Background(), "owner", "pane-1", 60, 20); err != nil {
		t.Fatal(err)
	}
	result := executeCapture(t, s, capturePage(strings.Join(body, " ")), true)
	if result.Capture.Admitted || !result.Composed || result.Response["content"] != capturePage(strings.Join(body, " ")) {
		t.Error("boundary preview did not reconcile the retained page")
	}
	if s.historyM.Content("pane-1", 0) != stored || s.historyAcknowledgements != 0 || s.historyPending["pane-1"] == nil || s.historyPending["pane-1"].sequence != sequence {
		t.Error("boundary preview changed history or pending acknowledgement")
	}
}

func TestLayoutHintDoesNotRequestHistoryRetry(t *testing.T) {
	s, _, _ := captureServer(t, false)
	raw := "Review your answers"
	if !question.LayoutHint(raw) || question.Classify(raw, "claude").Interaction != nil {
		t.Fatal("fixture is not hint-positive and parse-negative")
	}
	for range 2 {
		result, err := s.executeHistoryRead(context.Background(), captureRequest(s, false), func(context.Context, coordinator.PaneReadRequest) (herdr.PaneRead, error) {
			return herdr.PaneRead{Content: []byte(raw)}, nil
		})
		if err != nil || result.Capture.Admitted || result.Response["history_retry"] == true {
			t.Errorf("layout rejection requested unchanged history retry: %+v error=%v", result, err)
		}
	}
}
