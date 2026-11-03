package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func paneScrollPresenceCases() []struct {
	name string
	json string
	want string
} {
	return []struct {
		name string
		json string
		want string
	}{
		{name: "missing scroll", json: `{}`, want: `null`},
		{name: "null scroll", json: `{"scroll":null}`, want: `null`},
		{name: "empty scroll", json: `{"scroll":{}}`, want: `{}`},
		{name: "missing max field", json: `{"scroll":{"offset_from_bottom":0}}`, want: `{}`},
		{name: "null max field", json: `{"scroll":{"max_offset_from_bottom":null}}`, want: `{}`},
		{name: "explicit zero", json: `{"scroll":{"max_offset_from_bottom":0}}`, want: `{"max_offset_from_bottom":0}`},
		{name: "positive value", json: `{"scroll":{"max_offset_from_bottom":24}}`, want: `{"max_offset_from_bottom":24}`},
	}
}

func TestPaneScrollMetricPresence(t *testing.T) {
	for _, tt := range paneScrollPresenceCases() {
		t.Run(tt.name, func(t *testing.T) {
			var pane Pane
			if err := json.Unmarshal([]byte(tt.json), &pane); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(pane.Scroll)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tt.want {
				t.Fatalf("scroll = %s, want %s", encoded, tt.want)
			}
		})
	}
}

func TestGetInventoryParsesAgentActivitySequence(t *testing.T) {
	socketPath, done := startUnaryTestSocket(t, "agent.list", map[string]any{
		"type": "agent_list",
		"agents": []any{
			map[string]any{
				"pane_id":          "pane-1",
				"agent":            "codex",
				"agent_status":     "idle",
				"state_change_seq": 794,
				"agent_session":    map[string]any{"value": "session-1", "kind": "id"},
			},
		},
	})
	client := NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath)

	inventory, err := client.GetInventory(context.Background())
	if err != nil {
		t.Fatalf("GetInventory() error = %v", err)
	}
	if len(inventory.Panes) != 1 {
		t.Fatalf("GetInventory() panes = %d, want 1", len(inventory.Panes))
	}
	pane := inventory.Panes[0]
	if pane.StateChangeSeq != 794 || pane.Session != "session-1" {
		t.Fatalf("GetInventory() pane = %#v", pane)
	}
	if socketErr := <-done; socketErr != nil {
		t.Fatal(socketErr)
	}
}

func TestGetInventoryUsesJSONWithoutCLIFallback(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\nprintf '%s\\n' 'protocol_mismatch' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write Herdr script: %v", err)
	}
	socketPath, done := startUnaryTestSocket(t, "agent.list", map[string]any{
		"type":   "agent_list",
		"agents": []any{map[string]any{"pane_id": "pane-json", "agent": "codex", "agent_status": "idle"}},
	})
	client := NewClient(bin, socketPath)

	inventory, err := client.GetInventory(context.Background())
	if err != nil {
		t.Fatalf("GetInventory() error = %v", err)
	}
	if len(inventory.Panes) != 1 || inventory.Panes[0].ID != "pane-json" {
		t.Fatalf("GetInventory() = %#v, want JSON inventory", inventory)
	}
	if socketErr := <-done; socketErr != nil {
		t.Fatal(socketErr)
	}
}
