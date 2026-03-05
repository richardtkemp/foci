package agent

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"foci/internal/compaction"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// compactionMockServer creates a mock that returns a canned summary for
// compaction requests (detected by "provide continuity" in the last message)
// and normal end_turn responses otherwise. Turn highTokenTurn gets
// InputTokens=170000 to exceed the 160k threshold (0.8 * 200k).
func compactionMockServer(turnCount *atomic.Int32, highTokenTurn int32) *httptest.Server {
	return mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		lastMsg := req.Messages[len(req.Messages)-1]
		if strings.Contains(provider.TextOf(lastMsg.Content), "provide continuity") {
			return &provider.MessageResponse{
				ID:         "msg_summary",
				Type:       "message",
				Role:       "assistant",
				Content:    provider.TextContent("This is the compacted summary of the conversation."),
				StopReason: "end_turn",
				Usage:      provider.Usage{InputTokens: 500, OutputTokens: 100},
			}
		}

		n := turnCount.Add(1)
		inputTokens := 1000
		if n == highTokenTurn {
			inputTokens = 170_000
		}
		return &provider.MessageResponse{
			ID:         fmt.Sprintf("msg_%d", n),
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent(fmt.Sprintf("Response %d", n)),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: inputTokens, OutputTokens: 50},
		}
	})
}

// compactionTestEnv holds common test infrastructure for compaction tests.
type compactionTestEnv struct {
	ag        *Agent
	store     *session.Store
	compactor *compaction.Compactor
	server    *httptest.Server
}

// newCompactionTestEnv creates a test environment with a mock server, client,
// session store, bootstrap, and compactor. The turnCount and highTokenTurn
// parameters control which turn triggers high token usage.
func newCompactionTestEnv(t *testing.T, turnCount *atomic.Int32, highTokenTurn int32) *compactionTestEnv {
	t.Helper()
	server := compactionMockServer(turnCount, highTokenTurn)
	t.Cleanup(server.Close)

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	compactor := compaction.NewCompactor(store, "claude-haiku-4-5", 0.8)

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Compactor: compactor,
		Model:     "claude-haiku-4-5",
	}

	return &compactionTestEnv{
		ag:        ag,
		store:     store,
		compactor: compactor,
		server:    server,
	}
}

// runTurns runs HandleMessage for turns numbered from..to (inclusive) and
// verifies each response matches "Response N".
func (e *compactionTestEnv) runTurns(t *testing.T, sessionKey string, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		resp, err := e.ag.HandleMessage(context.Background(), sessionKey, fmt.Sprintf("Turn %d", i))
		if err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
		if resp != fmt.Sprintf("Response %d", i) {
			t.Errorf("Turn %d: response = %q", i, resp)
		}
	}
}
