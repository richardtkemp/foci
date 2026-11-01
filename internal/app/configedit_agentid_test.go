package app

import (
	"os"
	"strings"
	"testing"

	"foci/internal/fap"
)

// TestConfigPut_AgentIDIsReadOnly proves the config editor refuses an edit of
// the agent id (#1281) — as agent.id and as agents.id, put and unset — with a
// message naming the exact rename command, and writes nothing. Before the
// refusal the editor accepted the put and rewrote only the id line, which
// orphaned the agent's sessions, databases and bots.
func TestConfigPut_AgentIDIsReadOnly(t *testing.T) {
	h, path := newConfigEditHub(t)
	c := fakeClient()
	c.features = map[string]struct{}{featureConfigEdit: {}}
	h.clients[c] = struct{}{}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, section := range []string{"agent", "agents"} {
		h.handleConfigPut(c, fap.ConfigPut{Scope: "clutch", Section: section, Key: "id", Value: "renamed"})
		schema := lastConfigSchema(t, c)
		msg, _ := schema["error"].(string)
		if !strings.Contains(msg, "read-only") || !strings.Contains(msg, "foci rename-agent clutch <new-id>") {
			t.Errorf("put %s.id: error = %q, want the read-only refusal naming the rename command", section, msg)
		}
		h.handleConfigUnset(c, fap.ConfigUnset{Scope: "clutch", Section: section, Key: "id"})
		if msg, _ := lastConfigSchema(t, c)["error"].(string); !strings.Contains(msg, "foci rename-agent clutch") {
			t.Errorf("unset %s.id: error = %q", section, msg)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("a refused id edit changed foci.toml:\n%s", after)
	}
}
