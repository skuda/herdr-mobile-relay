package coordinator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestPhysicalFlightSharedAcrossReaderLimits(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "p", TerminalID: "terminal", Agent: "claude", Status: "idle"}}, 0)
	d := NewDispatcher(nil, state, nil, testLogger())
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	opts := PaneReadOptions{Physical: true, SourceRevision: 1,
		Execute: func(ctx context.Context, request PaneReadRequest, _ func(context.Context, PaneReadRequest) (herdr.PaneRead, error)) (PaneReadResult, error) {
			if request.Lines != 1000 || !request.Physical {
				t.Errorf("capture shape = %+v", request)
			}
			calls.Add(1)
			close(entered)
			<-release
			return PaneReadResult{Read: herdr.PaneRead{Content: []byte(strings.Repeat("row\n", 119) + "row")},
				Response: map[string]any{"content": strings.Repeat("row\n", 119) + "row"}, Capture: PaneReadCapture{Sequence: 7, Admitted: true}}, nil
		}}
	type result struct {
		response map[string]any
		capture  PaneReadCapture
	}
	done := make(chan result, 3)
	read := func(lines int) {
		response, capture := d.HandleReadPaneWithOptions(context.Background(), map[string]any{"pane_id": "p", "lines": lines, "format": "ansi"}, opts)
		done <- result{response, capture}
	}
	go read(100)
	<-entered
	go read(500)
	go read(1000)
	deadline := time.Now().Add(time.Second)
	for {
		d.readMu.Lock()
		followers := 0
		for _, call := range d.reads {
			followers += call.followers
		}
		d.readMu.Unlock()
		if followers == 2 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("followers did not join physical flight")
		}
		runtime.Gosched()
	}
	close(release)
	lengths := map[int]int{}
	for range 3 {
		r := <-done
		if r.capture.Sequence != 7 || !r.capture.Admitted {
			t.Fatalf("shared capture = %+v", r.capture)
		}
		lengths[strings.Count(r.response["content"].(string), "\n")+1]++
		r.response["private"] = true
	}
	if calls.Load() != 1 || lengths[100] != 1 || lengths[120] != 2 {
		t.Fatalf("calls=%d lengths=%v", calls.Load(), lengths)
	}
}

func TestPaneResponseLimitsComposedAndRawRows(t *testing.T) {
	for _, physical := range []bool{false, true} {
		for _, admitted := range []bool{false, true} {
			for _, tt := range []struct {
				name          string
				content       string
				composed      string
				raw           string
				readTruncated bool
				composedClip  bool
				rawClip       bool
			}{
				{name: "trailing newline at limit", content: "one\ntwo\nthree\n", composed: "two\nthree\n", raw: "one\ntwo\nthree\n", composedClip: true},
				{name: "trailing newline over limit", content: "archive\none\ntwo\nthree\n", composed: "two\nthree\n", raw: "one\ntwo\nthree\n", composedClip: true, rawClip: true},
				{name: "below limit", content: "two\nthree\n", composed: "two\nthree\n", raw: "two\nthree\n"},
				{name: "no trailing newline", content: "one\ntwo\nthree", composed: "one\ntwo\nthree", raw: "one\ntwo\nthree"},
				{name: "upstream truncation", content: "two\nthree\n", composed: "two\nthree\n", raw: "two\nthree\n", readTruncated: true, composedClip: true, rawClip: true},
			} {
				t.Run(fmt.Sprintf("physical=%v/admitted=%v/%s", physical, admitted, tt.name), func(t *testing.T) {
					state := NewState(testLogger())
					state.CommitInventory([]*AgentState{{PaneID: "p", TerminalID: "terminal", Agent: "qoder", Status: "idle"}}, 0)
					d := NewDispatcher(nil, state, nil, testLogger())
					options := PaneReadOptions{Physical: physical,
						Execute: func(context.Context, PaneReadRequest, func(context.Context, PaneReadRequest) (herdr.PaneRead, error)) (PaneReadResult, error) {
							return PaneReadResult{Read: herdr.PaneRead{Content: []byte(tt.content), Truncated: tt.readTruncated},
								Response: map[string]any{"content": tt.content}, Capture: PaneReadCapture{Admitted: admitted}}, nil
						}}
					response, _ := d.HandleReadPaneWithOptions(context.Background(), map[string]any{"pane_id": "p", "lines": 3, "format": "ansi"}, options)
					want, clipped := tt.raw, tt.rawClip
					if admitted {
						want, clipped = tt.composed, tt.composedClip
					}
					if response["content"] != want || response["truncated"] != clipped {
						t.Fatalf("limited response content=%q truncated=%v, want content=%q truncated=%v", response["content"], response["truncated"], want, clipped)
					}
				})
			}
		}
	}
}

func TestSingleFlightFollowerCannotRelabelOlderSnapshot(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "p", TerminalID: "terminal", Agent: "claude", Status: "idle"}}, 0)
	d := NewDispatcher(nil, state, nil, testLogger())
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	opts := PaneReadOptions{Physical: true, SourceRevision: 1,
		Execute: func(context.Context, PaneReadRequest, func(context.Context, PaneReadRequest) (herdr.PaneRead, error)) (PaneReadResult, error) {
			calls.Add(1)
			close(entered)
			<-release
			return PaneReadResult{Read: herdr.PaneRead{Content: []byte("old raw page")}, Capture: PaneReadCapture{Sequence: 1, TerminalID: "terminal"}}, nil
		}}
	type result struct {
		response map[string]any
		capture  PaneReadCapture
	}
	done := make(chan result, 2)
	read := func() {
		response, capture := d.HandleReadPaneWithOptions(context.Background(), map[string]any{"pane_id": "p", "lines": 100, "format": "ansi"}, opts)
		done <- result{response, capture}
	}
	go read()
	<-entered
	go read()
	deadline := time.Now().Add(time.Second)
	for {
		d.readMu.Lock()
		joined := false
		for _, call := range d.reads {
			joined = call.followers == 1
		}
		d.readMu.Unlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("follower did not join")
		}
		runtime.Gosched()
	}
	close(release)
	for range 2 {
		result := <-done
		if result.capture.Sequence != 1 || result.capture.Admitted || result.response["content"] != "old raw page" {
			t.Fatalf("relabelled shared result = %+v", result)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("underlying calls=%d", calls.Load())
	}
}

func TestSingleFlightRejectedOrCancelledOwnerDoesNotTransferAdmission(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		state := NewState(testLogger())
		state.CommitInventory([]*AgentState{{PaneID: "p", TerminalID: "terminal", Agent: "claude", Status: "idle"}}, 0)
		d := NewDispatcher(nil, state, nil, testLogger())
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		opts := PaneReadOptions{Physical: true, Execute: func(ctx context.Context, _ PaneReadRequest, _ func(context.Context, PaneReadRequest) (herdr.PaneRead, error)) (PaneReadResult, error) {
			sequence := calls.Add(1)
			if sequence == 1 {
				close(entered)
				<-release
			}
			result := PaneReadResult{Read: herdr.PaneRead{Content: []byte("raw")}, Capture: PaneReadCapture{Sequence: uint64(sequence), Admitted: sequence > 1}}
			if cancelled && sequence == 1 {
				return result, context.Canceled
			}
			return result, nil
		}}
		done := make(chan PaneReadCapture, 2)
		read := func() {
			_, capture := d.HandleReadPaneWithOptions(context.Background(), map[string]any{"pane_id": "p", "lines": 100, "format": "ansi"}, opts)
			done <- capture
		}
		go read()
		<-entered
		go read()
		deadline := time.Now().Add(time.Second)
		for {
			d.readMu.Lock()
			joined := false
			for _, call := range d.reads {
				joined = call.followers == 1
			}
			d.readMu.Unlock()
			if joined {
				break
			}
			if time.Now().After(deadline) {
				close(release)
				t.Fatal("follower did not join")
			}
			runtime.Gosched()
		}
		close(release)
		for range 2 {
			if capture := <-done; capture.Admitted || capture.Sequence != 1 {
				t.Fatalf("rejected owner capture = %+v", capture)
			}
		}
		read()
		if capture := <-done; !capture.Admitted || capture.Sequence != 2 {
			t.Fatalf("fresh retry = %+v", capture)
		}
	}
}

func TestHandleReadPaneUsesRecentRowsForResizedPane(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	recentPath := filepath.Join(dir, "recent.ansi")
	historyPath := filepath.Join(dir, "history.ansi")
	recentLines := make([]string, 120)
	for index := range recentLines {
		recentLines[index] = fmt.Sprintf("recent row %03d", index)
	}
	historyLines := make([]string, 120)
	for index := range historyLines {
		historyLines[index] = fmt.Sprintf("history row %03d", index)
	}
	recent := strings.Join(recentLines, "\n") + "\n"
	historyContent := strings.Join(historyLines, "\n") + "\n"
	if err := os.WriteFile(recentPath, []byte(recent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyPath, []byte(historyContent), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"case \" $* \" in\n"+
		"  *\" --source recent \"*) cat \""+recentPath+"\" ;;\n"+
		"  *) cat \""+historyPath+"\" ;;\n"+
		"esac\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "omp", Status: "idle"}}, state.RevisionCounter())
	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "missing.sock")),
		state,
		nil,
		testLogger(),
	)

	response := dispatcher.HandleReadPane(context.Background(), map[string]any{
		"pane_id": "pane-1", "lines": float64(150), "format": "ansi",
		"terminal_columns": float64(20), "terminal_rows": float64(46),
	})
	if response["content"] != recent {
		t.Fatalf("pane content has %d lines, want the %d-line recent window",
			strings.Count(response["content"].(string), "\n"),
			len(recentLines),
		)
	}
	if response["viewport_only"] != true {
		t.Fatalf("viewport_only = %#v, want true", response["viewport_only"])
	}
	if response["viewport_rows"] != 46 {
		t.Fatalf("viewport_rows = %#v, want 46", response["viewport_rows"])
	}
	if response["truncated"] != false {
		t.Fatalf("pane truncation = %#v, want false", response["truncated"])
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(invocations), "--source recent --format ansi") {
		t.Fatalf("pane read did not request the recent resized window: %s", invocations)
	}
}

// Display reads serve physical rows so the Resize Session baseline shares row
// semantics with resized frames; only Claude keeps logical lines for its
// alternate-screen history merge. Both apply to ansi reads, the only format
// the app asks for.
func TestHandleReadPaneSourceFollowsAgent(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"echo content\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{
		{PaneID: "pane-omp", Agent: "omp", Status: "idle"},
		{PaneID: "pane-claude", Agent: "claude", Status: "idle"},
	}, state.RevisionCounter())
	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "missing.sock")),
		state,
		nil,
		testLogger(),
	)

	for _, paneID := range []string{"pane-omp", "pane-claude"} {
		response := dispatcher.HandleReadPane(context.Background(), map[string]any{
			"pane_id": paneID, "lines": float64(100), "format": "ansi",
		})
		if _, failed := response["error"]; failed {
			t.Fatalf("read %s failed: %#v", paneID, response)
		}
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(invocations), "pane-omp --lines 100 --source recent ") {
		t.Fatalf("omp read did not request physical rows: %s", invocations)
	}
	if !strings.Contains(string(invocations), "pane-claude --lines 100 --source recent-unwrapped ") {
		t.Fatalf("claude read did not request logical lines: %s", invocations)
	}
}

// A "recent"/"recent-unwrapped" read in text format above the pane's viewport
// height makes Herdr harvest scrollback through the agent's mouse-scroll
// interface, which visibly scrolls the operator's real pane. No display read
// may reach that path.
func TestTextFormatDisplayReadNeverHarvestsScrollback(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"echo content\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{
		{PaneID: "pane-omp", Agent: "omp", Status: "idle"},
		{PaneID: "pane-claude", Agent: "claude", Status: "idle"},
	}, state.RevisionCounter())
	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "missing.sock")),
		state,
		nil,
		testLogger(),
	)

	for _, paneID := range []string{"pane-omp", "pane-claude"} {
		for _, columns := range []float64{0, 59} {
			response := dispatcher.HandleReadPane(context.Background(), map[string]any{
				"pane_id": paneID, "lines": float64(400), "format": "text",
				"terminal_columns": columns,
			})
			if _, failed := response["error"]; failed {
				t.Fatalf("read %s failed: %#v", paneID, response)
			}
		}
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(invocations), "--source recent") {
		t.Fatalf("a text-format read asked for harvested scrollback: %s", invocations)
	}
	if strings.Count(string(invocations), "--source visible --format text") != 4 {
		t.Fatalf("text-format reads did not all use the visible screen: %s", invocations)
	}
}
