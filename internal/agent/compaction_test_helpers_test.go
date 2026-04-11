package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"foci/internal/compaction"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// compactionTestClient creates a test client that returns a canned summary for
// compaction requests (detected by "continue seamlessly" in the last message)
// and normal end_turn responses otherwise. Turn highTokenTurn gets
// InputTokens=170000 to exceed the 160k threshold (0.8 * 200k).
func compactionTestClient(turnCount *atomic.Int32, highTokenTurn int32) *testClient {
	return newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		lastMsg := req.Messages[len(req.Messages)-1]
		if strings.Contains(provider.TextOf(lastMsg.Content), "continue seamlessly") {
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
	ag             *Agent
	store          *session.Store
	compactor      *compaction.Compactor
	lastRotatedKey string // set by SessionKeyRotatedFunc callback
}

// newCompactionTestEnv creates a test environment with a mock client,
// session store, bootstrap, and compactor. The turnCount and highTokenTurn
// parameters control which turn triggers high token usage.
func newCompactionTestEnv(t *testing.T, turnCount *atomic.Int32, highTokenTurn int32) *compactionTestEnv {
	t.Helper()
	client := compactionTestClient(turnCount, highTokenTurn)

	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	compactor := compaction.NewCompactor(store, 0.8)

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Compactor: compactor,
		Model:     "claude-haiku-4-5",
	}

	env := &compactionTestEnv{
		ag:        ag,
		store:     store,
		compactor: compactor,
	}
	ag.SessionKeyRotatedFunc.Add(func(oldKey, newKey string) {
		env.lastRotatedKey = newKey
	})
	return env
}

// activeKey returns the current session key, accounting for rotation.
func (e *compactionTestEnv) activeKey(original string) string {
	if e.lastRotatedKey != "" {
		return e.lastRotatedKey
	}
	return original
}

// runTurns runs HandleMessage for turns numbered from..to (inclusive) and
// verifies each response matches "Response N".
func (e *compactionTestEnv) runTurns(t *testing.T, sessionKey string, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		resp, err := e.ag.hmTest(context.Background(), sessionKey, fmt.Sprintf("Turn %d", i))
		if err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
		if resp != fmt.Sprintf("Response %d", i) {
			t.Errorf("Turn %d: response = %q", i, resp)
		}
	}
}
