package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/provider"
)

// recordingBatchRunner records each batch request, checks what it can only
// see while the batch runs (the raw temp dir, the deadline), and answers.
type recordingBatchRunner struct {
	mu        sync.Mutex
	reqs      []delegator.BatchRequest
	deadlines []time.Duration // time left on ctx when the batch ran
	write     string          // a file name a raw batch writes into its WorkDir
}

func (r *recordingBatchRunner) RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	if dl, ok := ctx.Deadline(); ok {
		r.deadlines = append(r.deadlines, time.Until(dl))
	}
	if r.write != "" && req.WorkDir != "" {
		if err := os.WriteFile(filepath.Join(req.WorkDir, r.write), []byte("x"), 0o600); err != nil {
			return "", err
		}
	}
	return "answer for " + req.Purpose, nil
}

// trapClient fails the test on any use: a delegated spawn must never reach an
// API client (directly, or as the fallback a nil group resolver returns).
type trapClient struct{ t *testing.T }

func (c trapClient) SendMessage(context.Context, *provider.MessageRequest) (*provider.MessageResponse, error) {
	c.t.Error("a delegated spawn sent an API request")
	return nil, context.Canceled
}
func (c trapClient) CountTokens(context.Context, *provider.MessageRequest) (int, error) {
	c.t.Error("a delegated spawn counted tokens through an API client")
	return 0, nil
}
func (c trapClient) IsCachingAvailable() bool { return false }

// allBatchCaps declares every capability a delegated spawn mode needs.
var allBatchCaps = map[delegator.Capability]delegator.Support{
	delegator.CapBranch:        delegator.Yes(),
	delegator.CapIsolatedBatch: delegator.Yes(),
	delegator.CapReadOnlyBatch: delegator.Yes(),
}

// newDelegatedSpawner builds a delegated agent's spawner over runner.
func newDelegatedSpawner(runner BatchRunner, caps map[delegator.Capability]delegator.Support) *Spawner {
	return NewSpawner(SpawnDeps{
		Sessions:   &mockSessionBrancher{},
		AgentID:    "test",
		MaxInherit: 2,
		Delegated: &DelegatedSpawn{
			Spec:   delegator.Spec{Caps: caps},
			Runner: func() BatchRunner { return runner },
		},
	}, func() SpawnAgent { return nil })
}

// TestDelegatedSpawnModes proves the modes a delegated spawn offers follow
// its backend's declarations: raw needs an isolated batch, clone a fork,
// explore a read-only batch; character is always there.
func TestDelegatedSpawnModes(t *testing.T) {
	t.Parallel()
	no := delegator.No("test")
	cases := []struct {
		name string
		caps map[delegator.Capability]delegator.Support
		want []string
	}{
		{"everything", allBatchCaps, []string{"raw", "character", "clone", "explore"}},
		{"no isolation", map[delegator.Capability]delegator.Support{
			delegator.CapBranch: delegator.Yes(), delegator.CapIsolatedBatch: no, delegator.CapReadOnlyBatch: delegator.Yes(),
		}, []string{"character", "clone", "explore"}},
		{"nothing", map[delegator.Capability]delegator.Support{
			delegator.CapBranch: no, delegator.CapIsolatedBatch: no, delegator.CapReadOnlyBatch: no,
		}, []string{"character"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DelegatedSpawnModes(delegator.Spec{Caps: c.caps})
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("modes = %v, want %v", got, c.want)
			}
			s := newDelegatedSpawner(&recordingBatchRunner{}, c.caps)
			var schema struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(s.SpawnTool().Parameters, &schema); err != nil {
				t.Fatalf("schema: %v", err)
			}
			if got := schema.Properties["context"].Enum; !reflect.DeepEqual(got, c.want) {
				t.Errorf("schema context enum = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDelegatedSpawn_UnofferedModeRefused proves a mode the backend cannot
// run safely is refused, not run some other way: a raw spawn on a backend
// with no isolated batch never reaches the runner.
func TestDelegatedSpawn_UnofferedModeRefused(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, map[delegator.Capability]delegator.Support{
		delegator.CapBranch: delegator.Yes(), delegator.CapIsolatedBatch: delegator.No("test"), delegator.CapReadOnlyBatch: delegator.Yes(),
	})
	_, err := s.SpawnTool().Execute(WithSessionKey(context.Background(), "test/c1"), json.RawMessage(`{"prompt":"p","context":"raw"}`))
	if err == nil || !strings.Contains(err.Error(), "invalid context") {
		t.Fatalf("raw spawn on a backend without isolation: err = %v, want invalid context", err)
	}
	if len(runner.reqs) != 0 {
		t.Fatalf("runner got %d batches for a refused mode", len(runner.reqs))
	}
}

// TestDelegatedSpawn_OneShotsRunAsBatches proves each one-shot mode of a
// delegated spawn becomes one batch on the agent's backend with the right
// access, prompt source, purpose, owner, model (verbatim) and deadline, and
// that its answer is the tool result; a raw spawn runs in a fresh temp dir
// whose files are listed.
func TestDelegatedSpawn_OneShotsRunAsBatches(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{write: "out.txt"}
	s := newDelegatedSpawner(runner, allBatchCaps)
	ctx := WithSessionKey(context.Background(), "test/c1")
	tool := s.SpawnTool()

	res, err := tool.Execute(ctx, json.RawMessage(`{"prompt":"draft it","context":"raw","model":"zai/glm-4.6","timeout":30}`))
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	if !strings.HasPrefix(res.Text, "answer for spawn_raw") || !strings.Contains(res.Text, "out.txt") {
		t.Errorf("raw result = %q, want the answer and the created file", res.Text)
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"prompt":"as me","context":"character"}`)); err != nil {
		t.Fatalf("character: %v", err)
	}
	res, err = tool.Execute(ctx, json.RawMessage(`{"prompt":"where is X","context":"explore"}`))
	if err != nil || res.Text != "answer for spawn_explore" {
		t.Fatalf("explore = %q, %v", res.Text, err)
	}

	if len(runner.reqs) != 3 {
		t.Fatalf("runner got %d batches, want 3", len(runner.reqs))
	}
	raw, char, explore := runner.reqs[0], runner.reqs[1], runner.reqs[2]

	// #2643 req 6: the caller's prompt is a raw spawn's only prompt — it
	// replaces the backend's default system prompt, which an empty one keeps.
	if raw.Access != delegator.BatchAccessIsolated || raw.Purpose != delegator.BatchPurposeSpawnRaw || raw.AgentPrompt || raw.SystemPrompt != "draft it" || raw.Prompt != spawnRawUserMessage {
		t.Errorf("raw batch = %+v, want isolated, spawn_raw, the caller's prompt as its system prompt and only %q as the user message", raw, spawnRawUserMessage)
	}
	if raw.WorkDir == "" || !strings.Contains(raw.WorkDir, "foci-spawn-") {
		t.Errorf("raw batch WorkDir = %q, want a fresh spawn temp dir", raw.WorkDir)
	}
	if raw.Model != "zai/glm-4.6" {
		t.Errorf("raw batch model = %q, want the caller's string verbatim", raw.Model)
	}
	if char.Access != delegator.BatchAccessParent || !char.AgentPrompt || char.SystemPrompt != "" || char.Purpose != delegator.BatchPurposeSpawnCharacter {
		t.Errorf("character batch = %+v, want parent access with the agent's prompt", char)
	}
	if char.Model != "" {
		t.Errorf("character batch model = %q, want empty (the agent's own)", char.Model)
	}
	if explore.Access != delegator.BatchAccessReadOnly || explore.SystemPrompt != exploreRolePrompt || explore.Purpose != delegator.BatchPurposeSpawnExplore {
		t.Errorf("explore batch = %+v, want read-only with the explorer prompt", explore)
	}
	for _, r := range runner.reqs {
		if r.OwnerSessionKey != "test/c1" || r.Prompt == "" {
			t.Errorf("batch %s: owner %q prompt %q, want test/c1 and the caller's prompt", r.Purpose, r.OwnerSessionKey, r.Prompt)
		}
	}
	if d := runner.deadlines[0]; d <= 0 || d > 30*time.Second {
		t.Errorf("raw batch ran with %s left, want the 30s timeout", d)
	}
}

// TestDelegatedSpawn_NeverTouchesModelGroups proves no delegated one-shot
// reaches API client code: a spawner whose API fields are traps (set after
// construction, past NewSpawner's guard) runs every one-shot mode without
// tripping one.
func TestDelegatedSpawn_NeverTouchesModelGroups(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Client = trapClient{t}
	s.deps.FallbackFunc = func(string) (string, string, string, bool) {
		t.Error("a delegated spawn asked for a model fallback")
		return "", "", "", false
	}
	s.deps.ProviderRoutingFor = func(string) *provider.ProviderRouting {
		t.Error("a delegated spawn asked for provider routing")
		return nil
	}
	ctx := WithSessionKey(context.Background(), "test/c1")
	for _, mode := range []string{"raw", "character", "explore"} {
		params, _ := json.Marshal(map[string]string{"prompt": "p", "context": mode, "model": "cheap"})
		if _, err := s.SpawnTool().Execute(ctx, params); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	if len(runner.reqs) != 3 {
		t.Fatalf("runner got %d batches, want 3", len(runner.reqs))
	}
}

// TestNewSpawner_DelegatedRejectsAPIFields proves the constructor refuses a
// delegated agent's deps that carry an API one-shot field, so the gateway
// cannot wire model-group code into a delegated spawn by mistake.
func TestNewSpawner_DelegatedRejectsAPIFields(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("NewSpawner accepted a delegated spawn with an API client")
		}
	}()
	NewSpawner(SpawnDeps{
		Client:    trapClient{t},
		Delegated: &DelegatedSpawn{Runner: func() BatchRunner { return nil }},
	}, func() SpawnAgent { return nil })
}
