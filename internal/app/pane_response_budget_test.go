package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panedelta"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func budgetFrame(content string) map[string]any {
	return map[string]any{"type": "pane_content", "pane_id": "p", "content": content, "format": "ansi",
		"target": map[string]any{"pane_id": "p", "terminal_id": "terminal-1"}, "truncated": false,
		"interaction": nil, "question_layout": false, "options": []string{"choice"}}
}

func TestPaneResponseBudgetCountsEscapedANSIAndMetadata(t *testing.T) {
	row := strings.Repeat("\x1b[38;2;123;123;123mX\x1b[0m", 20)
	content := strings.Repeat(row+"\n", 9999) + row
	response := budgetFrame(content)
	encoded, _ := json.Marshal(response)
	if len(encoded) <= PaneResponseBudgetBytes {
		t.Fatal("ANSI fixture does not exceed response budget")
	}
	fitPaneContentResponse(response)
	fitted := response["content"].(string)
	if !paneResponseFits(response) || response["truncated"] != true || !strings.HasSuffix(content, fitted) {
		t.Fatalf("fitted bytes=%d truncated=%v", len(fitted), response["truncated"])
	}
	response["content"] = row + "\n" + fitted
	if paneResponseFits(response) {
		t.Fatal("clipping did not retain longest fitting suffix")
	}
}

func TestPaneResponseBudgetBoundaryAndFittingLegacyContent(t *testing.T) {
	response := budgetFrame("legacy content")
	fitPaneContentResponse(response)
	if response["content"] != "legacy content" || response["truncated"] != false {
		t.Fatalf("fitting legacy changed: %+v", response)
	}
	response = budgetFrame("")
	candidate := map[string]any{}
	for key, value := range response {
		candidate[key] = value
	}
	candidate["content_fingerprint"], candidate["ack_required"] = "0123456789abcdef", true
	encoded, _ := json.Marshal(candidate)
	response["content"] = strings.Repeat("X", PaneResponseBudgetBytes-len(encoded))
	if !paneResponseFits(response) {
		t.Fatal("exact budget did not fit")
	}
	response["content"] = response["content"].(string) + "X"
	if paneResponseFits(response) {
		t.Fatal("one byte above budget fitted")
	}
}

func TestOversizedNewestRowProducesEmptyTruncatedSuccess(t *testing.T) {
	for _, watch := range []bool{false, true} {
		response := fitPaneContentResponse(budgetFrame(strings.Repeat("X", PaneResponseBudgetBytes+1)))
		if response["content"] != "" || response["truncated"] != true || response["error"] != nil || !paneResponseFits(response) {
			t.Fatalf("empty clipped response: %+v", response)
		}
		if _, ok := response["options"]; ok {
			t.Fatal("empty content retained transcript metadata")
		}
		if watch {
			frame := &paneWatchFrame{contentFingerprint: paneFingerprint(""), frameFingerprint: paneFrameFingerprint(response)}
			update := paneWatchUpdate(response, nil, frame)
			if update["ack_required"] != true || update["error"] != nil {
				t.Fatalf("empty watch response: %+v", update)
			}
		}
	}
}

func TestANSIHeavyFullFrameThroughHubSendAndReconnect(t *testing.T) {
	for _, source := range []string{history.SourceRecent, history.SourceLegacy} {
		t.Run(source, func(t *testing.T) {
			s, _, _ := captureServer(t, source == history.SourceRecent)
			row := strings.Repeat("\x1b[38;2;123;123;123mX\x1b[0m", 20)
			for page := range 10 {
				body := make([]string, 994)
				for index := range body {
					body[index] = fmt.Sprintf("%d-%d %s", page, index, row)
				}
				result := executeCapture(t, s, capturePage(body...), source == history.SourceRecent)
				if !result.Capture.Admitted {
					t.Fatalf("page %d was not admitted", page)
				}
			}
			stored := s.historyM.Content("pane-1", 0)
			response := budgetFrame(stored)
			encoded, err := json.Marshal(response)
			if err != nil || len(encoded) <= PaneResponseBudgetBytes {
				t.Fatalf("composed fixture bytes=%d error=%v", len(encoded), err)
			}
			fitPaneContentResponse(response)
			accepted := make(chan bool, 2)
			s.hub.SetOnConnect(func(client *transport.ClientConn) { accepted <- s.hub.Send(client, response) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			defer s.hub.Shutdown(context.Background())
			for range 2 {
				frames := &leaseTestFrames{name: transport.TransportWebRTC, in: make(chan []byte, 1), out: make(chan []byte, 1), done: make(chan struct{})}
				done := make(chan struct{})
				go func() { s.hub.Serve(ctx, frames); close(done) }()
				select {
				case ok := <-accepted:
					if !ok {
						t.Fatal("Hub rejected fitting frame")
					}
				case <-ctx.Done():
					t.Fatal("Hub did not connect")
				}
				select {
				case data := <-frames.out:
					var delivered map[string]any
					if err := json.Unmarshal(data, &delivered); err != nil {
						t.Fatal(err)
					}
					if len(data) > PaneResponseBudgetBytes || delivered["content"] != response["content"] || delivered["truncated"] != true {
						t.Fatal("Hub changed fitted frame")
					}
					if !strings.HasSuffix(stored, delivered["content"].(string)) {
						t.Fatal("delivery omitted newest suffix")
					}
				case <-ctx.Done():
					t.Fatal("Hub did not deliver frame")
				}
				frames.CloseNow()
				<-done
			}
			if s.historyM.Content("pane-1", 0) != stored {
				t.Fatal("byte fitting changed retained state")
			}
		})
	}
}

func TestPaneWatchANSIHeavyInitialAndResyncFullFrameFits(t *testing.T) {
	row := strings.Repeat("\x1b[38;2;123;123;123mX\x1b[0m", 20)
	response := fitPaneContentResponse(budgetFrame(strings.Repeat(row+"\n", 9999) + row))
	content := response["content"].(string)
	response["content_fingerprint"] = paneFingerprint(content)
	current := &paneWatchFrame{content: content, contentFingerprint: paneFingerprint(content), frameFingerprint: paneFrameFingerprint(response)}
	for _, previous := range []*paneWatchFrame{nil, {content: "unrelated", contentFingerprint: "different", frameFingerprint: "previous"}} {
		copy := make(map[string]any)
		for key, value := range response {
			copy[key] = value
		}
		update := paneWatchUpdate(copy, previous, current)
		if update == nil || !paneResponseFits(update) || update["content_fingerprint"] != current.contentFingerprint {
			t.Fatal("watch full frame did not fit")
		}
	}
}

func TestOversizedEncodedDeltaFallsBackToFittedFullFrame(t *testing.T) {
	response := budgetFrame("small")
	segments := []panedelta.Segment{{Text: strings.Repeat("\x1b[0m", PaneResponseBudgetBytes/4)}}
	update := fittedPaneDelta(response, "old", segments)
	if update["type"] != "pane_content" || update["ack_required"] != true || !paneResponseFits(update) {
		t.Fatalf("oversized delta did not use full frame: type=%v", update["type"])
	}
}
