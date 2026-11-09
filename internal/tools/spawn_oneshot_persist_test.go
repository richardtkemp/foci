package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
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
	convs := &storeConversations{store: session.NewStore(t.TempDir())}
	h := newPersistHarness(nil, func(d *SpawnDeps) {
		d.Client = newTestAnthropicClient(server.URL, "test-token")
		d.Registry = reg
		d.FallbackModel, d.FallbackFormat = "anthropic/claude-haiku-4-5", "anthropic"
		d.MaxToolLoops = func() int { return 10 }
		d.Conversations = convs
	})
	h.convs = convs
	return h
}

// storeConversations is SpawnConversations over a real session store, as
// cmd/foci-gw's sessionBranchAdapter is (#2984).
type storeConversations struct{ store *session.Store }

func (c *storeConversations) NewSpawnConversation(parent string) (string, error) {
	return c.store.CreateBranchWithOptions(parent, session.BranchOptions{NoResetHook: true, BranchType: "spawn", Fresh: true})
}

func (c *storeConversations) AppendSpawnConversation(key string, msgs []provider.Message) error {
	return c.store.For(key).AppendAll(key, msgs)
}

func (c *storeConversations) LoadSpawnConversation(key string) ([]provider.Message, error) {
	return c.store.Load(key)
}

func (c *storeConversations) ResetSpawnConversation(key string) error {
	return c.store.For(key).Replace(key, nil)
}

// fileBytes is the raw content of the session file key.
func (c *storeConversations) fileBytes(t *testing.T, key string) []byte {
	t.Helper()
	path, err := c.store.SessionPath(key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read conversation file %s: %v", key, err)
	}
	return b
}

// conv is the conversation in the session file key ("" = none).
func (h *persistHarness) conv(t *testing.T, key string) []provider.Message {
	t.Helper()
	if key == "" {
		return nil
	}
	msgs, err := h.convs.LoadSpawnConversation(key)
	if err != nil {
		t.Fatalf("load conversation %s: %v", key, err)
	}
	return msgs
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
	var convAtSecondCall []provider.Message
	var convKey string
	calls := 0
	h = newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return toolUseResponse("m1")
		}
		convKey = h.tracker.conversationKey("test/imain", idWhileRunning)
		convAtSecondCall = h.conv(t, convKey)
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
	if convKey == "" || len(convAtSecondCall) != 3 || convAtSecondCall[1].Role != "assistant" {
		t.Fatalf("conversation file %q at its second model call = %d messages, want its task, the tool call and its result after 1 round", convKey, len(convAtSecondCall))
	}
	if rec, ok := h.store.record(t, idWhileRunning); ok {
		t.Errorf("record after it completed = %+v, want none", rec)
	}
	// The record goes; the conversation file stays, as a spawn branch's
	// transcript does, and ends on its answer (#2984).
	if got := h.conv(t, convKey); len(got) != 4 || got[3].Role != "assistant" || provider.TextOf(got[3].Content) != "done" {
		t.Errorf("conversation file after it completed = %+v, want its 3 and its answer", got)
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
	if c := h.conv(t, rec.OneShot.Conversation); len(c) != 3 {
		t.Fatalf("conversation file %q after the shutdown cut = %d messages, want its first round", rec.OneShot.Conversation, len(c))
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
	key := h.savedConversation(t, saved)
	h.store.put(t, spawnRecord{
		ID: "spawn-os4", Parent: "test/imain", Name: "stars", Prompt: "count the stars",
		Started: time.Now().Add(-time.Minute), Running: true,
		OneShot: &oneShotRecord{Mode: spawnModeCharacter, Self: "test/imain/b1", Conversation: key},
	})

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
	// The notice went to the model, not into the file; the answer did.
	if got := h.conv(t, key); len(got) != 4 || provider.TextOf(got[3].Content) != "resumed answer" ||
		bytes.Contains(h.convs.fileBytes(t, key), []byte("FOCI RESTARTED")) {
		t.Errorf("conversation file after the resumed run = %d messages (want the 3 saved and its answer, no restart notice)", len(got))
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

// savedConversation writes msgs to a new conversation file, as an earlier
// process's run would have, and returns its key.
func (h *persistHarness) savedConversation(t *testing.T, msgs []provider.Message) string {
	t.Helper()
	key, err := h.convs.NewSpawnConversation("test/imain")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.convs.AppendSpawnConversation(key, msgs); err != nil {
		t.Fatal(err)
	}
	return key
}

// #2984: each round of a one-shot appends only what it added to its session
// file; nothing written before is written again. Over a 50-round run the file
// only ever grows at its end, by about the same bytes each round, where the
// #2979 metadata row was rewritten whole each round (O(n^2) bytes in all).
func TestAPIOneShot_ConversationAppendOnly50Rounds(t *testing.T) {
	t.Parallel()
	const rounds = 50
	var h *persistHarness
	var mu sync.Mutex
	calls := 0
	var id, key string
	var snaps [][]byte // the file at each round's tool call, once it exists
	h = newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls > rounds {
			return textResponse("done")
		}
		return toolUseResponse(fmt.Sprintf("m%02d", calls))
	}, probeTool(func(context.Context) {
		mu.Lock()
		defer mu.Unlock()
		if id == "" {
			id = h.tracker.Running("test/imain")[0].ID
		}
		if key == "" {
			key = h.tracker.conversationKey("test/imain", id)
		}
		if key != "" {
			snaps = append(snaps, h.convs.fileBytes(t, key))
		}
	}))
	h.spawner.deps.MaxToolLoops = func() int { return rounds + 10 }

	params, _ := json.Marshal(map[string]string{"name": "os50", "prompt": "fifty rounds", "context": "character"})
	res, err := h.spawner.SpawnTool().Execute(h.ctx, params)
	if err != nil || res.Text != "done" {
		t.Fatalf("spawn = %q, %v", res.Text, err)
	}
	mu.Lock()
	defer mu.Unlock()
	final := h.convs.fileBytes(t, key)
	snaps = append(snaps, final)
	// Every round saw the file (made before the first model call, #2988),
	// plus the end.
	if len(snaps) != rounds+1 {
		t.Fatalf("saw the file %d times, want %d", len(snaps), rounds+1)
	}
	// Append-only: each state of the file is a prefix of the next.
	var deltas []int
	for i := 1; i < len(snaps); i++ {
		if !bytes.HasPrefix(snaps[i], snaps[i-1]) {
			t.Fatalf("round %d rewrote earlier bytes of the file (len %d -> %d)", i+1, len(snaps[i-1]), len(snaps[i]))
		}
		deltas = append(deltas, len(snaps[i])-len(snaps[i-1]))
	}
	// Each round writes its own two messages, so no round's bytes depend on
	// how many came before.
	for i, d := range deltas {
		if d <= 0 || d > 2*deltas[0] {
			t.Fatalf("round %d wrote %d bytes, round 2 wrote %d: want about the same", i+2, d, deltas[0])
		}
	}
	msgs := h.conv(t, key)
	if len(msgs) != 2+2*rounds {
		t.Fatalf("file holds %d messages, want %d (task, 50 rounds, answer)", len(msgs), 2+2*rounds)
	}
	// What the #2979 scheme wrote: the whole conversation, every round.
	var oldTotal, oldLast int
	for r := 1; r <= rounds; r++ {
		b, _ := json.Marshal(struct {
			Loops    int                `json:"loops"`
			Messages []provider.Message `json:"messages"`
		}{r, msgs[:1+2*r]})
		oldTotal += len(b)
		oldLast = len(b)
	}
	t.Logf("%d rounds: append-only wrote %d bytes in all (last save, round 50 + answer: %d); whole-conversation rows would have written %d (round 50: %d)",
		rounds, len(final), deltas[len(deltas)-1], oldTotal, oldLast)
}

// #2984: a resumed run appends its new rounds after the saved conversation,
// and the restart notice it was given is not written to the file.
func TestAPIOneShot_ResumedRunAppends(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := 0
	h := newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return toolUseResponse("m2")
		}
		return textResponse("resumed answer")
	}, probeTool(func(context.Context) {}))

	saved := []provider.Message{
		{Role: "user", Content: provider.TextContent("count the stars")},
		{Role: "assistant", Content: toolUseResponse("m1").Content},
		{Role: "user", Content: []provider.ContentBlock{provider.ToolResultBlock("tu_m1", "probed", false)}},
	}
	key := h.savedConversation(t, saved)
	before := h.convs.fileBytes(t, key)
	h.store.put(t, spawnRecord{
		ID: "spawn-os6", Parent: "test/imain", Name: "stars", Prompt: "count the stars",
		Started: time.Now().Add(-time.Minute), Running: true,
		OneShot: &oneShotRecord{Mode: spawnModeCharacter, Self: "test/imain/b1", Conversation: key},
	})
	h.spawner.RestoreSpawns()
	d := h.nextDelivery(t)
	if !strings.Contains(d.msg, "resumed answer") {
		t.Fatalf("delivered %q, want its answer", d.msg)
	}
	after := h.convs.fileBytes(t, key)
	if !bytes.HasPrefix(after, before) {
		t.Fatal("the resumed run rewrote the saved conversation")
	}
	if bytes.Contains(after, []byte("FOCI RESTARTED")) {
		t.Error("the restart notice was written to the file")
	}
	got := h.conv(t, key)
	if len(got) != 6 || got[3].Role != "assistant" || got[4].Role != "user" || provider.TextOf(got[5].Content) != "resumed answer" {
		t.Fatalf("file holds %d messages, want the 3 saved, the resumed round's 2 and its answer", len(got))
	}
	d.done(true)
}

// #2984 (Dick 2026-11-08: write the final answer too): a one-shot that
// answers with no tool call still leaves its task and answer in a file.
func TestAPIOneShot_AnswerWithoutToolsWritten(t *testing.T) {
	t.Parallel()
	var h *persistHarness
	var key string
	h = newOneShotHarness(t, func(req *provider.MessageRequest) *provider.MessageResponse {
		return textResponse("straight answer")
	}, nil)
	params, _ := json.Marshal(map[string]string{"name": "os7", "prompt": "just answer", "context": "explore"})
	res, err := h.spawner.SpawnTool().Execute(h.ctx, params)
	if err != nil || res.Text != "straight answer" {
		t.Fatalf("spawn = %q, %v", res.Text, err)
	}
	keys := h.convKeys(t)
	if len(keys) != 1 {
		t.Fatalf("conversation files = %v, want one", keys)
	}
	key = keys[0]
	got := h.conv(t, key)
	if len(got) != 2 || provider.TextOf(got[0].Content) != "just answer" || got[1].Role != "assistant" ||
		provider.TextOf(got[1].Content) != "straight answer" {
		t.Fatalf("conversation file = %+v, want its task and its answer", got)
	}
}

// convKeys lists the spawn conversation files under the harness parent.
func (h *persistHarness) convKeys(t *testing.T) []string {
	t.Helper()
	path, err := h.convs.store.SessionPath("test/imain")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range ents {
		if name := e.Name(); strings.HasPrefix(name, "b") && strings.HasSuffix(name, ".jsonl") {
			keys = append(keys, "test/imain/"+strings.TrimSuffix(name, ".jsonl"))
		}
	}
	return keys
}
