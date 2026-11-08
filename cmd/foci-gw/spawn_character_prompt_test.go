package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"foci/internal/agent"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// captureClient is a provider.Client that records the system blocks of each
// request and answers every one with a final text reply.
type captureClient struct {
	mu     sync.Mutex
	system [][]provider.SystemBlock
}

func (c *captureClient) SendMessage(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	c.mu.Lock()
	c.system = append(c.system, append([]provider.SystemBlock(nil), req.System...))
	c.mu.Unlock()
	return &provider.MessageResponse{
		ID: "msg_test", Type: "message", Role: "assistant",
		Content: provider.TextContent("done"), StopReason: "end_turn",
	}, nil
}

func (c *captureClient) CountTokens(context.Context, *provider.MessageRequest) (int, error) {
	return 0, nil
}

func (c *captureClient) IsCachingAvailable() bool { return false }

// TestAPICharacterSpawnGetsMainSessionPrompt is #2981: an API agent's
// character spawn runs on the same system prompt as one of its main sessions
// (environment block, character files, skills list), not on the character
// files alone. Before the fix the spawn got Bootstrap.SystemBlocks() only, so
// a local-model agent's spawn reported "no skills in its system prompt".
func TestAPICharacterSpawnGetsMainSessionPrompt(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "character"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "character", "SOUL.md"), []byte("I am the soul file."), 0o644); err != nil {
		t.Fatal(err)
	}
	bs := workspace.NewBootstrap(ws, nil)

	var envKeys []string
	var envSpawn []bool
	var envMu sync.Mutex
	ag := &agent.Agent{
		AgentID:   "spawnprompt",
		Bootstrap: bs,
		EnvironmentBlockFunc: func(sessionKey string, spawn bool) string {
			envMu.Lock()
			envKeys = append(envKeys, sessionKey)
			envSpawn = append(envSpawn, spawn)
			envMu.Unlock()
			return "# Environment for the test agent"
		},
		ExtraSystemBlocks: []provider.SystemBlock{{Type: "text", Text: "# Available Skills\n- a-skill"}},
	}

	client := &captureClient{}
	p := minimalSetupParams(t, "spawnprompt")
	p.sessions = session.NewStore(t.TempDir())
	out := &toolOutputs{}
	registerTools(&toolDeps{
		p:             p,
		path:          pathAPI,
		registry:      tools.NewRegistry(),
		connMgr:       stubConnMgr{},
		agLazy:        func() *agent.Agent { return ag },
		wakeFn:        stubWakeFn,
		out:           out,
		client:        client,
		bootstrap:     bs,
		resolvedModel: "anthropic/claude-haiku-4-5",
		defaultFormat: "anthropic",
	})
	if out.spawner == nil {
		t.Fatal("no spawner registered")
	}

	caller := "spawnprompt/c1/i1"
	ctx := tools.WithSessionKey(context.Background(), caller)
	params, _ := json.Marshal(map[string]string{
		"name": "promptcheck", "prompt": "Say done.", "context": "character",
	})
	res, err := out.spawner.SpawnTool().Execute(ctx, params)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if res.Text != "done" {
		t.Fatalf("spawn result = %q, want done", res.Text)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.system) != 1 {
		t.Fatalf("model calls = %d, want 1", len(client.system))
	}
	var texts []string
	for _, b := range client.system[0] {
		texts = append(texts, b.Text)
	}
	got := strings.Join(texts, "\n---\n")
	// The main-session order: environment, character files, skills.
	want := []string{"# Environment for the test agent", "I am the soul file.", "# Available Skills"}
	last := -1
	for _, w := range want {
		i := strings.Index(got, w)
		if i < 0 {
			t.Fatalf("spawn system prompt lacks %q:\n%s", w, got)
		}
		if i < last {
			t.Errorf("spawn system prompt has %q out of the main-session order:\n%s", w, got)
		}
		last = i
	}
	// The environment block is built for the spawn's own session, a batch
	// key under the caller, as a delegated character batch's is.
	envMu.Lock()
	defer envMu.Unlock()
	if len(envKeys) == 0 || !strings.HasPrefix(envKeys[len(envKeys)-1], "spawnprompt/c1/b") {
		t.Errorf("environment block built for %v, want the spawn's batch key under spawnprompt/c1", envKeys)
	}
	// #2986: and it is built as a spawn's, whose reply goes to its spawner.
	if len(envSpawn) == 0 || !envSpawn[len(envSpawn)-1] {
		t.Errorf("environment block built with spawn=%v, want a spawn's", envSpawn)
	}
}
