package coordinator

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestPollerJoinsPaneScrollByTerminalIdentity(t *testing.T) {
	state := testState()
	socket := startPollerTestSocket(t, func(method string) (any, string) {
		switch method {
		case "agent.list":
			return json.RawMessage(`{"type":"agent_list","agents":[
				{"pane_id":"positive","terminal_id":"term-positive","agent":"claude","agent_status":"working"},
				{"pane_id":"zero","terminal_id":"term-zero","agent":"qoder","agent_status":"idle"}
			]}`), ""
		case "pane.list":
			return json.RawMessage(`{"type":"pane_list","panes":[
				{"pane_id":"zero","terminal_id":"term-zero","scroll":{"max_offset_from_bottom":0}},
				{"pane_id":"shell","terminal_id":"term-shell","scroll":{"max_offset_from_bottom":0}},
				{"pane_id":"positive","terminal_id":"term-positive","scroll":{"max_offset_from_bottom":42}}
			]}`), ""
		default:
			return pollerSuccessResult(method), ""
		}
	})
	client := herdr.NewClient("unused", socket)
	t.Cleanup(func() { _ = client.Close() })
	NewPoller(client, state, time.Second, testLogger()).poll(context.Background())
	if !state.InventoryReady() || state.AgentCount() != 2 {
		t.Fatalf("inventory ready=%v agents=%d", state.InventoryReady(), state.AgentCount())
	}
	for paneID, wantOffset := range map[string]int{"positive": 42, "zero": 0} {
		agent, ok := state.Agent(paneID)
		if !ok || !agent.ScrollMetricsKnown || agent.ScrollMaxOffset != wantOffset {
			t.Fatalf("agent %q = %+v, want known scroll %d", paneID, agent, wantOffset)
		}
		if got := agent.UsesNoScrollbackHistory(); got != (wantOffset == 0) {
			t.Fatalf("agent %q eligibility = %v", paneID, got)
		}
	}
}

func TestPollerScrollUnknownOnPaneListFailureOrIdentityMismatch(t *testing.T) {
	for _, tt := range []struct {
		name          string
		agentTerminal string
		pane          string
		paneError     string
	}{
		{name: "pane list failure", agentTerminal: "term-1", paneError: "upstream_down"},
		{name: "terminal mismatch", agentTerminal: "term-1", pane: `{"pane_id":"pane-1","terminal_id":"term-old","scroll":{"max_offset_from_bottom":0}}`},
		{name: "missing pane terminal", agentTerminal: "term-1", pane: `{"pane_id":"pane-1","scroll":{"max_offset_from_bottom":0}}`},
		{name: "missing agent terminal", pane: `{"pane_id":"pane-1","terminal_id":"term-1","scroll":{"max_offset_from_bottom":0}}`},
		{name: "both terminals missing", pane: `{"pane_id":"pane-1","scroll":{"max_offset_from_bottom":0}}`},
		{name: "pane mismatch", agentTerminal: "term-1", pane: `{"pane_id":"pane-other","terminal_id":"term-1","scroll":{"max_offset_from_bottom":0}}`},
		{name: "missing scroll", agentTerminal: "term-1", pane: `{"pane_id":"pane-1","terminal_id":"term-1"}`},
		{name: "null scroll", agentTerminal: "term-1", pane: `{"pane_id":"pane-1","terminal_id":"term-1","scroll":null}`},
		{name: "empty scroll", agentTerminal: "term-1", pane: `{"pane_id":"pane-1","terminal_id":"term-1","scroll":{}}`},
		{name: "missing max field", agentTerminal: "term-1", pane: `{"pane_id":"pane-1","terminal_id":"term-1","scroll":{"offset_from_bottom":0}}`},
		{name: "null max field", agentTerminal: "term-1", pane: `{"pane_id":"pane-1","terminal_id":"term-1","scroll":{"max_offset_from_bottom":null}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := testState()
			state.CommitInventory([]*AgentState{{
				PaneID: "pane-1", TerminalID: "term-1", Agent: "claude", Status: "idle",
				ScrollMetricsKnown: true, ScrollMaxOffset: 0,
			}}, 0)
			socket := startPollerTestSocket(t, func(method string) (any, string) {
				switch method {
				case "agent.list":
					return map[string]any{"type": "agent_list", "agents": []any{map[string]any{
						"pane_id": "pane-1", "terminal_id": tt.agentTerminal, "agent": "claude", "agent_status": "idle",
						"workspace_id": "workspace-1", "cwd": "/work/project", "scroll": map[string]any{"max_offset_from_bottom": 0},
					}}}, ""
				case "workspace.list":
					return json.RawMessage(`{"type":"workspace_list","workspaces":[{"workspace_id":"workspace-1"}]}`), ""
				case "pane.list":
					return json.RawMessage(`{"type":"pane_list","panes":[` + tt.pane + `]}`), tt.paneError
				default:
					return pollerSuccessResult(method), ""
				}
			})
			client := herdr.NewClient("unused", socket)
			t.Cleanup(func() { _ = client.Close() })
			NewPoller(client, state, time.Second, testLogger()).poll(context.Background())
			agent, ok := state.Agent("pane-1")
			if !state.InventoryReady() || !ok || agent.ScrollMetricsKnown || agent.ScrollMaxOffset != 0 || agent.UsesNoScrollbackHistory() {
				t.Fatalf("ready=%v agent=%+v, want unknown scroll", state.InventoryReady(), agent)
			}
			if tt.paneError != "" {
				workspaces := state.InventorySnapshot().Workspaces
				if len(workspaces) != 1 || workspaces[0].Cwd != "/work/project" {
					t.Fatalf("workspace hydration fallback = %+v", workspaces)
				}
			}
		})
	}
}

func TestEventTopologyPreservesCurrentSameTerminalScrollMetric(t *testing.T) {
	state := testState()
	cache := scrollMetricEventCache(t)
	methods := make(chan string, 3)
	socket := startPollerTestSocket(t, func(method string) (any, string) {
		methods <- method
		return json.RawMessage(`{"type":"agent_list","agents":[{"pane_id":"pane-1","terminal_id":"term-1","agent":"claude","agent_status":"idle"}]}`), ""
	})
	client := herdr.NewClient("unused", socket)
	t.Cleanup(func() { _ = client.Close() })
	poller := NewPoller(client, state, time.Second, testLogger())
	poller.commitEventTopology(context.Background(), cache.Snapshot(), state.RevisionCounter())
	agent, ok := state.Agent("pane-1")
	if !ok || agent.ScrollMetricsKnown {
		t.Fatalf("bootstrap agent = %+v, want unknown metrics", agent)
	}
	_, committed := state.CommitPoll([]*AgentState{{
		PaneID: "pane-1", TerminalID: "term-1", Agent: "claude", Status: "idle",
		ScrollMetricsKnown: true, ScrollMaxOffset: 27,
	}}, nil, state.BeginPoll())
	if !committed {
		t.Fatal("metric poll did not commit")
	}
	if !poller.applyTopologyEvent(context.Background(), cache, herdr.Event{
		Event: "tab.renamed", Data: json.RawMessage(`{"tab":{"tab_id":"tab-1","label":"Renamed"}}`),
	}) {
		t.Fatal("tab event failed")
	}
	agent, ok = state.Agent("pane-1")
	if !ok || !agent.ScrollMetricsKnown || agent.ScrollMaxOffset != 27 || agent.UsesNoScrollbackHistory() {
		t.Fatalf("agent after tab event = %+v, want known positive scroll", agent)
	}
	for range 2 {
		select {
		case method := <-methods:
			if method != "agent.list" {
				t.Fatalf("event refresh method = %q, want agent.list", method)
			}
		default:
			t.Fatal("missing event inventory refresh")
		}
	}
	select {
	case method := <-methods:
		t.Fatalf("unexpected event refresh method = %q", method)
	default:
	}
}

func TestEventTopologyMetricCarryForwardOccursAtCommit(t *testing.T) {
	for _, tt := range []struct {
		name   string
		known  bool
		offset int
	}{
		{name: "positive", known: true, offset: 31},
		{name: "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := testState()
			state.CommitInventory([]*AgentState{{
				PaneID: "pane-1", TerminalID: "term-1", Agent: "claude", Status: "idle", ScrollMetricsKnown: true,
			}}, 0)
			socket := startPollerTestSocket(t, func(string) (any, string) {
				return json.RawMessage(`{"type":"agent_list","agents":[{"pane_id":"pane-1","terminal_id":"term-1","agent":"claude","agent_status":"idle","scroll":{"max_offset_from_bottom":0}}]}`), ""
			})
			client := herdr.NewClient("unused", socket)
			t.Cleanup(func() { _ = client.Close() })
			poller := NewPoller(client, state, time.Second, testLogger())
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseEnrichment := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseEnrichment)
			poller.SetEnrich(func(context.Context, []*AgentState) {
				close(entered)
				<-release
			})
			topology := scrollMetricEventCache(t).Snapshot()
			go func() {
				poller.commitEventTopology(context.Background(), topology, state.RevisionCounter())
				close(done)
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("event enrichment did not pause")
			}
			_, committed := state.CommitPoll([]*AgentState{{
				PaneID: "pane-1", TerminalID: "term-1", Agent: "claude", Status: "idle",
				ScrollMetricsKnown: tt.known, ScrollMaxOffset: tt.offset,
			}}, nil, state.BeginPoll())
			if !committed {
				t.Fatal("newer metric poll did not commit")
			}
			releaseEnrichment()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("event commit did not finish")
			}
			agent, ok := state.Agent("pane-1")
			if !ok || agent.ScrollMetricsKnown != tt.known || agent.ScrollMaxOffset != tt.offset {
				t.Fatalf("event committed agent = %+v, want known=%v offset=%d", agent, tt.known, tt.offset)
			}
		})
	}
}

func scrollMetricEventCache(t *testing.T) *herdr.SessionCache {
	t.Helper()
	var snapshot herdr.SessionSnapshot
	if err := json.Unmarshal([]byte(`{
		"panes":[{"pane_id":"pane-1","terminal_id":"term-1","scroll":{"max_offset_from_bottom":0}}],
		"agents":[{"pane_id":"pane-1","terminal_id":"term-1","agent":"claude","agent_status":"idle"}]
	}`), &snapshot); err != nil {
		t.Fatal(err)
	}
	return herdr.NewSessionCache(snapshot)
}

func TestTopologyStaleRepollsAreBounded(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, 0)
	poller := NewPoller(nil, state, time.Second, testLogger())

	for retry := 0; retry < maxImmediateTopologyPolls; retry++ {
		poller.handleTopologyStale(state.InventoryStatus())
		select {
		case <-poller.wakeup:
		default:
			t.Fatalf("retry %d did not request an immediate repoll", retry+1)
		}
	}
	poller.handleTopologyStale(state.InventoryStatus())
	select {
	case <-poller.wakeup:
		t.Fatal("topology churn requested an unbounded immediate repoll")
	default:
	}
	status := state.InventoryStatus()
	if status["state"] != "error" || status["error_code"] != "topology_churn" {
		t.Fatalf("inventory status = %+v, want topology degradation", status)
	}
}

// While the event stream is healthy the poll is only a reconcile backstop, but
// when events are unavailable it is the sole freshness source and must honour
// the operator-configured interval again.
func TestPollerIntervalTracksEventStreamHealth(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, 0)
	poller := NewPoller(nil, state, time.Second, testLogger())

	if got := poller.currentInterval(); got != time.Second {
		t.Fatalf("interval with events down = %v, want the configured 1s", got)
	}

	poller.eventsActive.Store(true)
	if got := poller.currentInterval(); got != idlePollInterval {
		t.Fatalf("interval with events up = %v, want %v", got, idlePollInterval)
	}

	poller.eventsActive.Store(false)
	if got := poller.currentInterval(); got != time.Second {
		t.Fatalf("interval after events dropped = %v, want the configured 1s", got)
	}
}

func TestPollerIntervalClampsToReconcileCeiling(t *testing.T) {
	poller := NewPoller(nil, testState(), time.Hour, testLogger())
	if got := poller.currentInterval(); got != idlePollInterval {
		t.Fatalf("interval = %v, want it clamped to %v", got, idlePollInterval)
	}
}

func TestPollerInventoryChangePublishesCurrentState(t *testing.T) {
	state := testState()
	poller := NewPoller(nil, state, time.Second, testLogger())
	var statuses []string
	poller.SetOnInventoryChange(func() error {
		statuses = append(statuses, state.InventoryStatus()["state"].(string))
		return nil
	})
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, 0)
	poller.notifyInventoryChange()
	state.MarkInventoryFailure(fmt.Errorf("temporary failure"))
	poller.notifyInventoryChange()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, state.RevisionCounter())
	poller.notifyInventoryChange()
	poller.notifyInventoryChange()
	if got, want := fmt.Sprint(statuses), "[ready error ready ready]"; got != want {
		t.Fatalf("publication states = %s, want %s", got, want)
	}
}

func TestAgentsFromTopologyPreservesForegroundCwd(t *testing.T) {
	state := testState()
	poller := NewPoller(nil, state, time.Second, testLogger())
	pane := herdr.Pane{
		ID: "pane-1", Agent: "claude", Cwd: "/work/pane", ForegroundCwd: "/work/foreground",
	}
	agents := poller.agentsFromTopology([]herdr.Pane{pane}, nil)
	if len(agents) != 1 || agents[0].Cwd != pane.Cwd || agents[0].Project != "pane" || agents[0].ForegroundCwd != pane.ForegroundCwd {
		t.Fatalf("topology agent = %#v, want pane project/cwd=%q foreground=%q", agents, pane.Cwd, pane.ForegroundCwd)
	}
	state.CommitInventory(agents, state.RevisionCounter())
	stored, ok := state.Agent("pane-1")
	if !ok || stored.Cwd != pane.Cwd || stored.ForegroundCwd != pane.ForegroundCwd {
		t.Fatalf("stored agent = %#v, want both directories preserved", stored)
	}

	cleared := pane
	cleared.ForegroundCwd = ""
	state.CommitInventory(poller.agentsFromTopology([]herdr.Pane{cleared}, nil), state.RevisionCounter())
	stored, ok = state.Agent("pane-1")
	if !ok || stored.ForegroundCwd != "" {
		t.Fatalf("cleared foreground cwd = %#v, want empty", stored)
	}
}

func TestPollerEventCommitPublishesRecoveryAfterPausedEnrichment(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, 0)
	socket := startPollerTestSocket(t, func(string) (any, string) {
		return map[string]any{"type": "agent_list", "agents": []herdr.Pane{{ID: "pane-1", Agent: "codex", Status: "idle"}}}, ""
	})
	client := herdr.NewClient("unused", socket)
	t.Cleanup(func() { _ = client.Close() })
	poller := NewPoller(client, state, time.Second, testLogger())
	entered := make(chan struct{})
	release := make(chan struct{})
	poller.SetEnrich(func(context.Context, []*AgentState) {
		close(entered)
		<-release
	})
	var mu sync.Mutex
	var statuses []string
	poller.SetOnInventoryChange(func() error {
		status, _ := state.InventoryStatus()["state"].(string)
		mu.Lock()
		statuses = append(statuses, status)
		mu.Unlock()
		return nil
	})
	topology := herdr.TopologySnapshot{Panes: []herdr.Pane{{ID: "pane-1", Agent: "codex", Status: "idle"}}}
	eventDone := make(chan struct{})
	go func() {
		poller.commitEventTopology(context.Background(), topology, state.RevisionCounter())
		close(eventDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("event enrichment did not pause")
	}
	state.MarkInventoryFailure(fmt.Errorf("event recovery schedule failure"))
	poller.notifyInventoryChange()
	close(release)
	select {
	case <-eventDone:
	case <-time.After(time.Second):
		t.Fatal("paused event commit did not complete")
	}
	mu.Lock()
	got := append([]string(nil), statuses...)
	mu.Unlock()
	if fmt.Sprint(got) != "[error ready]" {
		t.Fatalf("event recovery publications = %v, want [error ready]", got)
	}
	if state.InventoryStatus()["state"] != "ready" {
		t.Fatalf("event recovery state = %#v, want ready", state.InventoryStatus())
	}
}

func TestPollerInventoryPublicationSerializesOvertakingChanges(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, 0)
	poller := NewPoller(nil, state, time.Second, testLogger())
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)
	secondDone := make(chan struct{})
	var mu sync.Mutex
	var statuses []string
	calls := 0
	poller.SetOnInventoryChange(func() error {
		status, _ := state.InventoryStatus()["state"].(string)
		mu.Lock()
		statuses = append(statuses, status)
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			close(entered)
			<-release
		} else if call == 2 {
			close(secondDone)
		}
		return nil
	})
	go poller.notifyInventoryChange()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first inventory publication did not enter")
	}
	state.MarkInventoryFailure(fmt.Errorf("overtaking failure"))
	go poller.notifyInventoryChange()
	select {
	case <-secondDone:
		t.Fatal("inventory publication overtook the first callback")
	case <-time.After(20 * time.Millisecond):
	}
	releaseFirst()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second inventory publication did not complete")
	}
	mu.Lock()
	got := append([]string(nil), statuses...)
	mu.Unlock()
	if fmt.Sprint(got) != "[ready error]" {
		t.Fatalf("serialized publication states = %v, want [ready error]", got)
	}
}

func TestInventorySnapshotIsCoherentAndOwned(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "blocked", Options: []string{"one"}}}, 0)
	state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "One", Worktree: &herdr.WorkspaceWorktree{CheckoutPath: "/work"}}})
	snapshot := state.InventorySnapshot()
	if len(snapshot.Agents) != 1 || len(snapshot.Agents[0].Options) != 1 {
		t.Fatalf("snapshot agents = %#v", snapshot.Agents)
	}
	snapshot.Status["state"] = "error"
	snapshot.Agents[0].Options[0] = "mutated"
	snapshot.Workspaces[0].Worktree.CheckoutPath = "/mutated"
	fresh := state.InventorySnapshot()
	if fresh.Status["state"] != "ready" || fresh.Agents[0].Options[0] != "one" || fresh.Workspaces[0].Worktree.CheckoutPath != "/work" {
		t.Fatalf("state snapshot was not independently owned: %#v", fresh)
	}
}

func TestHydrateWorkspaceCwdsKeepsShellOnlyWorkspaceLaunchable(t *testing.T) {
	workspaces := []herdr.Workspace{{ID: "w1", Label: "Shell only"}}
	hydrateWorkspaceCwds(workspaces, nil, []herdr.Pane{{
		ID: "p1", WorkspaceID: "w1", Cwd: "/home/user/project",
	}})
	if workspaces[0].Cwd != "/home/user/project" {
		t.Fatalf("workspace cwd = %q", workspaces[0].Cwd)
	}
}

func TestRunEventsRefreshesSnapshotAfterDroppedStream(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		defer listener.Close()
		subscriptions := 0
		snapshots := 0
		for range 7 {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverDone <- acceptErr
				return
			}
			func() {
				defer conn.Close()
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); decodeErr != nil {
					serverDone <- decodeErr
					return
				}
				switch request.Method {
				case "events.subscribe":
					subscriptions++
					_ = json.NewEncoder(conn).Encode(map[string]any{
						"id": request.ID, "result": map[string]any{"type": "subscription_started"},
					})
					if subscriptions == 1 {
						_ = json.NewEncoder(conn).Encode(map[string]any{
							"event": "workspace.created",
							"data": map[string]any{
								"workspace": map[string]any{"workspace_id": "w2", "label": "Buffered"},
							},
						})
					}
				case "agent.list":
					_ = json.NewEncoder(conn).Encode(map[string]any{
						"id": request.ID, "result": pollerSuccessResult(request.Method),
					})
				case "session.snapshot":
					snapshots++
					workspaces := []any{
						map[string]any{"workspace_id": "w1", "label": "Project"},
					}
					if snapshots == 2 {
						workspaces = append(workspaces,
							map[string]any{"workspace_id": "w3", "label": "Created Offline"},
						)
					}
					_ = json.NewEncoder(conn).Encode(map[string]any{
						"id": request.ID,
						"result": map[string]any{
							"type":     "session_snapshot",
							"snapshot": map[string]any{"workspaces": workspaces},
						},
					})
				default:
					serverDone <- fmt.Errorf("unexpected event method %q", request.Method)
				}
			}()
		}
		serverDone <- nil
	}()

	state := testState()
	client := herdr.NewClient("missing-herdr", socketPath)
	t.Cleanup(func() { _ = client.Close() })
	poller := NewPoller(client, state, time.Second, testLogger())
	reconnects := 0
	poller.eventReconnectWait = func(context.Context) bool {
		reconnects++
		return reconnects == 1
	}
	updates := make(chan InventorySnapshot, 8)
	poller.SetOnInventoryChange(func() error {
		updates <- state.InventorySnapshot()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		poller.RunEvents(ctx, herdr.NewEventClient(socketPath))
		close(runDone)
	}()

	var final InventorySnapshot
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for final.Workspaces == nil {
		select {
		case snapshot := <-updates:
			if len(snapshot.Workspaces) == 2 && snapshot.Workspaces[1].ID == "w3" {
				final = snapshot
			}
		case <-deadline.C:
			t.Fatal("event reconnect did not converge on the current snapshot")
		}
	}
	<-runDone
	if len(final.Workspaces) != 2 || final.Workspaces[0].ID != "w1" || final.Workspaces[1].ID != "w3" {
		t.Fatalf("final workspaces = %+v, want w1 and w3", final.Workspaces)
	}
	if reconnects != 2 {
		t.Fatalf("reconnect waits = %d, want 2", reconnects)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
