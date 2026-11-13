package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
)

// Spawn contexts have no deadline of their own, keep their identity markers,
// and remain explicitly cancellable. Only direct runs inherit caller expiry.
func TestSpawnContext_NoRunDeadline(t *testing.T) {
	t.Parallel()
	for _, detached := range []bool{false, true} {
		ctx, cancel := buildSpawnContext(context.Background(), "test/imain/b1", detached)
		if dl, ok := ctx.Deadline(); ok {
			t.Errorf("detached=%v: unexpected deadline %v", detached, dl)
		}
		if SessionKeyFromContext(ctx) != "test/imain/b1" {
			t.Error("missing the branch's session key")
		}
		cancel()
		if ctx.Err() != context.Canceled {
			t.Errorf("cancel: %v", ctx.Err())
		}
	}
	parent, stop := context.WithTimeout(context.Background(), time.Hour)
	defer stop()
	direct, endDirect := buildSpawnContext(parent, "test/imain/b1", false)
	defer endDirect()
	background, endBackground := buildSpawnContext(parent, "test/imain/b2", true)
	defer endBackground()
	if _, ok := direct.Deadline(); !ok {
		t.Error("direct spawn lost caller deadline")
	}
	if _, ok := background.Deadline(); ok {
		t.Error("background spawn inherited caller deadline")
	}
	stop()
	if direct.Err() != context.Canceled || background.Err() != nil {
		t.Errorf("caller cancellation: direct=%v background=%v", direct.Err(), background.Err())
	}
}

// Shell flags and help are generated from these schemas: removing timeout
// must remove it from both spawn and spawn_send, not merely ignore the flag.
func TestSpawnShell_NoTimeoutFlag(t *testing.T) {
	t.Parallel()
	s := NewSpawner(SpawnDeps{}, nil)
	for _, tool := range []*Tool{s.SpawnTool(), s.SendTool()} {
		var schema struct{ Properties map[string]json.RawMessage }
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		if _, ok := schema.Properties["timeout"]; ok {
			t.Errorf("%s advertises timeout", tool.Name)
		}
		if strings.Contains(generateShellFunc(tool), "--timeout") {
			t.Errorf("%s shell still accepts or documents --timeout", tool.Name)
		}
	}
}

// Old persisted timeout fields are ignored;
// newly encoded records omit them, so restored runs cannot recreate a timer.
func TestSpawnPersist_IgnoresLegacyTimeout(t *testing.T) {
	t.Parallel()
	h := newPersistHarness(newCtxSpawnAgent(), nil)
	rec := runningRecord("spawn-legacy")
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.TrimSuffix(string(b), "}") + `,"timeout":1}`
	if err := h.store.SetAgentMetadata("test", spawnMetaPrefix+rec.ID, legacy); err != nil {
		t.Fatal(err)
	}
	records := h.tracker.loadRecords("test")
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	encoded, err := json.Marshal(records[0])
	if err != nil || strings.Contains(string(encoded), `"timeout"`) {
		t.Fatalf("new record = %s, %v", encoded, err)
	}
	agent := newCtxSpawnAgent()
	h.spawner.agentFn = func() SpawnAgent { return agent }
	h.spawner.RestoreSpawns()
	h.nextDelivery(t)
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.keys) != 1 || len(agent.deadlines) != 0 {
		t.Fatalf("restored contexts: keys=%v deadlines=%v", agent.keys, agent.deadlines)
	}
}

// spawnNoDeadlineClient checks the context before the real client adds its
// per-request protection; a spawn must not put its own deadline around it.
type spawnNoDeadlineClient struct {
	provider.Client
	t *testing.T
}

func (c spawnNoDeadlineClient) SendMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	if dl, ok := ctx.Deadline(); ok {
		c.t.Errorf("API spawn added run deadline %v", dl)
	}
	return c.Client.SendMessage(ctx, req)
}
