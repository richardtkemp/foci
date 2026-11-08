package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/provider"
)

// #2979: an API agent's direct one-shot spawn survives a foci restart.

// oneShotHarness is a persistHarness whose agent runs on the API: its spawns'
// one-shot loop calls server.
func newOneShotHarness(t *testing.T, handler func(req *provider.MessageRequest) *provider.MessageResponse, tool *Tool) *persistHarness {
	t.Helper()
	server := mockModelServer(handler)
	t.Cleanup(server.Close)
	reg := NewRegistry()
	if tool != nil {
		reg.Register(tool)
	}
	return newPersistHarness(nil, func(d *SpawnDeps) {
		d.Client = newTestAnthropicClient(server.URL, "test-token")
		d.Registry = reg
		d.FallbackModel, d.FallbackFormat = "anthropic/claude-haiku-4-5", "anthropic"
		d.MaxToolLoops = func() int { return 10 }
	})
}

func (m *memSpawnStore) conv(t *testing.T, id string) (oneShotConv, bool) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.rows["test"][spawnConvPrefix+id]
	if !ok {
		return oneShotConv{}, false
	}
	var c oneShotConv
	if err := json.Unmarshal([]byte(v), &c); err != nil {
		t.Fatalf("conversation %s: %v", id, err)
	}
	return c, true
}

func toolUseResponse(id string) *provider.MessageResponse {
	return &provider.MessageResponse{
		ID: id, Type: "message", Role: "assistant",
		Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "tu_" + id, Name: "probe_tool", Input: json.RawMessage(`{}`)},
		},
		StopReason: "tool_use",
		Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

func textResponse(text string) *provider.MessageResponse {
	return &provider.MessageResponse{
		ID: "final", Type: "message", Role: "assistant",
		Content:    provider.TextContent(text),
		StopReason: "end_turn",
		Usage:      provider.Usage{InputTokens: 20, OutputTokens: 5},
	}
}

func probeTool(run func(ctx context.Context)) *Tool {
	return &Tool{
		Name:       "probe_tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			run(ctx)
			return TextResult("probed"), nil
		},
	}
}

// While it runs, the spawn's record and (after its first round of tools) its
// conversation are persisted; a run that completes forgets both.
func TestAPIOneShot_PersistedWhileRunningForgottenWhenDone(t *testing.T) {
	t.Parallel()
	var h *persistHarness
	var mu sync.Mutex
	var idWhileRunning string
	var recWhileRunning spawnRecord
	var recOK bool
	var convAtSecondCall oneShotConv
	var convOK bool
	calls := 0
	h = newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return toolUseResponse("m1")
		}
		convAtSecondCall, convOK = h.store.conv(t, idWhileRunning)
		return textResponse("done")
	}, probeTool(func(context.Context) {
		r := h.tracker.Running("test/imain")
		if len(r) != 1 {
			t.Errorf("Running = %+v, want the spawn", r)
			return
		}
		mu.Lock()
		idWhileRunning = r[0].ID
		recWhileRunning, recOK = h.store.record(t, r[0].ID)
		mu.Unlock()
	}))

	params, _ := json.Marshal(map[string]string{"name": "os1", "prompt": "look around", "context": "character"})
	res, err := h.spawner.SpawnTool().Execute(h.ctx, params)
	if err != nil || res.Text != "done" {
		t.Fatalf("spawn = %q, %v", res.Text, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !recOK || !recWhileRunning.Running || recWhileRunning.OneShot == nil ||
		recWhileRunning.OneShot.Mode != spawnModeCharacter || recWhileRunning.OneShot.Self == "" ||
		recWhileRunning.Prompt != "look around" || recWhileRunning.Parent != "test/imain" {
		t.Fatalf("record while running = %+v (ok %v), want a running character one-shot with its task", recWhileRunning, recOK)
	}
	if !convOK || convAtSecondCall.Loops != 1 || len(convAtSecondCall.Messages) != 3 {
		t.Fatalf("conversation at its second model call = %+v (ok %v), want its task, the tool call and its result after 1 round", convAtSecondCall, convOK)
	}
	if rec, ok := h.store.record(t, idWhileRunning); ok {
		t.Errorf("record after it completed = %+v, want none", rec)
	}
	if _, ok := h.store.conv(t, idWhileRunning); ok {
		t.Error("conversation kept after it completed")
	}
}

// A run the shutdown cuts short keeps its record (running) and its
// conversation, for the next process.
func TestAPIOneShot_ShutdownCutKeepsRecord(t *testing.T) {
	t.Parallel()
	var h *persistHarness
	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	var id string
	h = newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		return toolUseResponse("m1")
	}, probeTool(func(context.Context) {
		if r := h.tracker.Running("test/imain"); len(r) == 1 {
			id = r[0].ID
		}
		// foci shuts down, and the parent's call is cut with it.
		h.tracker.BeginShutdown()
		cancelCall()
	}))

	params, _ := json.Marshal(map[string]string{"name": "os2", "prompt": "long task", "context": "character"})
	if _, err := h.spawner.SpawnTool().Execute(WithSessionKey(callCtx, "test/imain"), params); err == nil {
		t.Fatal("a cut spawn returned no error")
	}
	rec, ok := h.store.record(t, id)
	if !ok || !rec.Running || rec.OneShot == nil || rec.OneShot.Mode != spawnModeCharacter {
		t.Fatalf("record after the shutdown cut = %+v (ok %v), want it kept, running", rec, ok)
	}
	if c, ok := h.store.conv(t, id); !ok || len(c.Messages) != 3 {
		t.Fatalf("conversation after the shutdown cut = %+v (ok %v), want its first round", c, ok)
	}
}

// A stop is not a shutdown: the record goes with the run.
func TestAPIOneShot_StopForgetsRecord(t *testing.T) {
	t.Parallel()
	var h *persistHarness
	var id string
	h = newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		return toolUseResponse("m1")
	}, probeTool(func(context.Context) {
		if r := h.tracker.Running("test/imain"); len(r) == 1 {
			id = r[0].ID
		}
		h.tracker.Stop("test/imain")
	}))
	params, _ := json.Marshal(map[string]string{"name": "os3", "prompt": "t", "context": "character"})
	if _, err := h.spawner.SpawnTool().Execute(h.ctx, params); err == nil {
		t.Fatal("a stopped spawn returned no error")
	}
	if rec, ok := h.store.record(t, id); ok {
		t.Errorf("record after a stop = %+v, want none", rec)
	}
	if _, ok := h.store.conv(t, id); ok {
		t.Error("conversation kept after a stop")
	}
}

// After a restart, a persisted one-shot runs again from its saved
// conversation, with a restart notice, and its answer reaches the parent as
// a [SPAWN RESULT].
func TestAPIOneShot_ResumedAfterRestart(t *testing.T) {
	t.Parallel()
	got := make(chan []provider.Message, 1)
	h := newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		select {
		case got <- req.Messages:
		default:
		}
		return textResponse("resumed answer")
	}, probeTool(func(context.Context) {}))

	saved := []provider.Message{
		{Role: "user", Content: provider.TextContent("count the stars")},
		{Role: "assistant", Content: toolUseResponse("m1").Content},
		{Role: "user", Content: []provider.ContentBlock{provider.ToolResultBlock("tu_m1", "probed", false)}},
	}
	h.store.put(t, spawnRecord{
		ID: "spawn-os4", Parent: "test/imain", Name: "stars", Prompt: "count the stars",
		Started: time.Now().Add(-time.Minute), Running: true,
		OneShot: &oneShotRecord{Mode: spawnModeCharacter, Self: "test/imain/b1"},
	})
	b, _ := json.Marshal(oneShotConv{Loops: 1, Messages: saved})
	_ = h.store.SetAgentMetadata("test", spawnConvPrefix+"spawn-os4", string(b))

	h.spawner.RestoreSpawns()

	var msgs []provider.Message
	select {
	case msgs = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the resumed spawn made no model call")
	}
	if len(msgs) != 3 || msgs[1].Role != "assistant" {
		t.Fatalf("resumed with %d messages, want the 3 saved", len(msgs))
	}
	last := msgs[2].Content
	if len(last) != 2 || last[0].Type != "tool_result" || !strings.Contains(last[1].Text, "FOCI RESTARTED") {
		t.Errorf("last message = %+v, want the tool result then the restart notice", last)
	}

	d := h.nextDelivery(t)
	if d.session != "test/imain" || !strings.Contains(d.msg, "[SPAWN RESULT] stars (spawn spawn-os4) completed") ||
		!strings.Contains(d.msg, "resumed answer") {
		t.Fatalf("delivered to %s: %q, want its completed [SPAWN RESULT]", d.session, d.msg)
	}
	if _, ok := h.store.conv(t, "spawn-os4"); ok {
		t.Error("conversation kept after the resumed run ended")
	}
	if rec, ok := h.store.record(t, "spawn-os4"); !ok || rec.Running || rec.Resumes != 1 {
		t.Errorf("record before the parent took the result = %+v (ok %v), want the result kept, resumed once", rec, ok)
	}
	d.done(true)
	if rec, ok := h.store.record(t, "spawn-os4"); ok {
		t.Errorf("record after the parent took the result = %+v, want none", rec)
	}
	select {
	case n := <-h.notices:
		if !strings.Contains(n, "Resumed 1") || !strings.Contains(n, "spawn-os4") {
			t.Errorf("notice = %q, want it resumed", n)
		}
	case <-time.After(5 * time.Second):
		t.Error("no restart notice to the parent's chat")
	}
}

// A raw one-shot cannot resume (its sandbox is gone): its parent is told it
// was lost, with its task.
func TestAPIOneShot_RawReportedLost(t *testing.T) {
	t.Parallel()
	h := newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		t.Error("a lost spawn made a model call")
		return textResponse("x")
	}, nil)
	h.store.put(t, spawnRecord{
		ID: "spawn-os5", Parent: "test/imain", Name: "rawjob", Prompt: "write a file",
		Started: time.Now(), Running: true, OneShot: &oneShotRecord{Mode: spawnModeRaw},
	})
	h.spawner.RestoreSpawns()
	d := h.nextDelivery(t)
	if !strings.Contains(d.msg, "lost in a foci restart") || !strings.Contains(d.msg, spawnRawLostReason) ||
		!strings.Contains(d.msg, "write a file") {
		t.Fatalf("delivered %q, want its lost report with the reason and task", d.msg)
	}
}
