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

	"foci/internal/provider"
)

// testModelAliases returns standard model aliases for tests
func testModelAliases() map[string]string {
	return map[string]string{
		"opus":   "anthropic/claude-opus-4-6",
		"sonnet": "anthropic/claude-sonnet-4-6",
		"haiku":  "anthropic/claude-haiku-4-5",
		"flash":  "google/gemini-2.5-flash",
	}
}

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

func (m *mockSessionBrancher) CreateBranch(parentKey, branchKey string, opts BranchOptions) error {
	m.parentKey = parentKey
	m.branchKey = branchKey
	m.opts = opts
	return m.err
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

func (m *mockSpawnAgent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	m.sessionKey = sessionKey
	m.message = userMessage
	return m.response, m.err
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

func (a *concurrentAgent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	cur := atomic.AddInt32(a.concurrentCount, 1)
	for {
		old := atomic.LoadInt32(a.maxConcurrent)
		if cur <= old || atomic.CompareAndSwapInt32(a.maxConcurrent, old, cur) {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	atomic.AddInt32(a.concurrentCount, -1)
	return "ok", nil
}

// channelSpawnAgent signals when HandleMessage is called.
type channelSpawnAgent struct {
	response string
	err      error
	called   chan struct{}
	mu       sync.Mutex
}

func (a *channelSpawnAgent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	a.mu.Lock()
	if a.called != nil {
		select {
		case a.called <- struct{}{}:
		default:
		}
	}
	a.mu.Unlock()
	return a.response, a.err
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

func TestSpawnGuardResult(t *testing.T) {
	// Small result — returned as-is
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
