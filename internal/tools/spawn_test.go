package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/platform"
	"foci/internal/provider"
)

// mockBootstrap implements SystemBlocksProvider for tests.
type mockBootstrap struct {
	blocks []provider.SystemBlock
}

func (m *mockBootstrap) SystemBlocks() []provider.SystemBlock {
	return m.blocks
}

// mockModelServer returns a test server that captures requests and returns canned responses.
func mockModelServer(handler func(req *provider.MessageRequest) *provider.MessageResponse) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req provider.MessageRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := handler(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

// mockSessionBrancher captures branch creation calls.
type mockSessionBrancher struct {
	parentKey string
	branchKey string
	opts      BranchOptions
	err       error
}

func (m *mockSessionBrancher) CreateBranch(parentKey string, opts BranchOptions) (string, error) {
	m.parentKey = parentKey
	m.opts = opts
	// Generate a deterministic branch key from parentKey for test stability.
	m.branchKey = parentKey + "/b1000000000"
	return m.branchKey, m.err
}

func (m *mockSessionBrancher) SessionPath(key string) (string, error) {
	return "/tmp/mock-session-" + key + ".jsonl", nil
}

// mockSpawnAgent captures HandleMessage calls.
type mockSpawnAgent struct {
	sessionKey string
	message    string
	response   string
	err        error
}

func (m *mockSpawnAgent) HandleMessage(ctx context.Context, sessionKey string, texts []string, _ []platform.Attachment) error {
	m.sessionKey = sessionKey
	if len(texts) > 0 {
		m.message = texts[0]
	}
	if m.err != nil {
		return m.err
	}
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: m.response})
	return nil
}

func okResponse(text string) func(req *provider.MessageRequest) *provider.MessageResponse {
	return func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: provider.TextContent(text), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	}
}

// concurrentAgent tracks concurrent HandleMessage calls.
type concurrentAgent struct {
	concurrentCount *int32
	maxConcurrent   *int32
}

func (a *concurrentAgent) HandleMessage(ctx context.Context, sessionKey string, texts []string, _ []platform.Attachment) error {
	cur := atomic.AddInt32(a.concurrentCount, 1)
	for {
		old := atomic.LoadInt32(a.maxConcurrent)
		if cur <= old || atomic.CompareAndSwapInt32(a.maxConcurrent, old, cur) {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	atomic.AddInt32(a.concurrentCount, -1)
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: "ok"})
	return nil
}

// channelSpawnAgent signals when HandleMessage is called.
type channelSpawnAgent struct {
	response string
	err      error
	called   chan struct{}
	mu       sync.Mutex
}

func (a *channelSpawnAgent) HandleMessage(ctx context.Context, sessionKey string, texts []string, _ []platform.Attachment) error {
	a.mu.Lock()
	if a.called != nil {
		select {
		case a.called <- struct{}{}:
		default:
		}
	}
	a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: a.response})
	return nil
}

func extractTempDir(result string) string {
	marker := "Files created in "
	idx := strings.Index(result, marker)
	if idx == -1 {
		return ""
	}
	start := idx + len(marker)
	end := strings.Index(result[start:], "/:")
	if end == -1 {
		return ""
	}
	return result[start : start+end]
}

func TestSpawnInheritSetsNoCompact(t *testing.T) {
	// Proves that spawnInherit calls SetNoCompact with the branch key after
	// creating the branch, preventing branch sessions from compacting
	// independently and sending notifications to the main chat.
	t.Parallel()

	var noCompactKey string
	var noCompactValue bool
	brancher := &mockSessionBrancher{}
	agent := &mockSpawnAgent{response: "done"}

	deps := SpawnDeps{
		Sessions:   brancher,
		MaxInherit: 2,
		SetNoCompact: func(sk string, v bool) {
			noCompactKey = sk
			noCompactValue = v
		},
	}
	sem := make(chan struct{}, 2)

	ctx := WithSessionKey(context.Background(), "test/c123/456")

	result, err := spawnInherit(ctx, deps, func() SpawnAgent { return agent }, sem, "test prompt", 5*time.Second)
	if err != nil {
		t.Fatalf("spawnInherit: %v", err)
	}
	if result.Text == "" {
		t.Fatal("expected non-empty result")
	}
	if noCompactKey == "" {
		t.Fatal("SetNoCompact was not called")
	}
	if !strings.Contains(noCompactKey, "/b") {
		t.Errorf("SetNoCompact key = %q, want branch key containing '/b'", noCompactKey)
	}
	if !noCompactValue {
		t.Error("SetNoCompact value = false, want true")
	}
}

func TestSpawnGuardResult(t *testing.T) {
	// Proves that small results pass through unchanged while large results are written to a temp file
	// and replaced with a "Result too large" guard message pointing to the file.
	t.Parallel()
	small := strings.Repeat("x", 100)
	if got := spawnGuardResult("test", small, spawnMaxResultChars); got != small {
		t.Errorf("small result should pass through, got %q", got[:50])
	}

	// Large result — written to temp file
	large := strings.Repeat("y", spawnMaxResultChars+1)
	got := spawnGuardResult("grep", large, spawnMaxResultChars)
	if strings.Contains(got, "yyy") {
		t.Errorf("large result should be replaced with guard message, got %d chars", len(got))
	}
	if !strings.Contains(got, "Result too large") {
		t.Errorf("expected 'Result too large', got %q", got)
	}
	if !strings.Contains(got, "spawn-result-grep-") {
		t.Errorf("expected temp file path in guard, got %q", got)
	}
	if !strings.Contains(got, "read tool") {
		t.Errorf("expected 'read tool' hint, got %q", got)
	}
}
