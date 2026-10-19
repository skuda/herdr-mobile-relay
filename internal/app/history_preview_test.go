package app

import (
	"context"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
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
	frame := &paneWatchFrame{classificationAgent: "claude", historyRetry: result.Response["history_retry"] == true}
	if !paneWatchNeedsFrameRead("same", "same", frame, "claude") {
		t.Fatal("transient rejection did not request unchanged-probe retry")
	}
	advance(panesize.HistorySettleWindow)
	result = executeCapture(t, s, capturePage("current A", "current B"), true)
	frame.historyRetry = result.Response["history_retry"] == true
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
		executeCapture(t, s, capturePage("archive A", "archive B"), physical)
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
