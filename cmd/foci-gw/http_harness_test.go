package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

const (
	testAgentID    = "test-agent"
	testSessionKey = "test-agent/i0"
	mockReply      = "mock reply"
)

// mockCall records one SendMessage invocation for assertions.
type mockCall struct {
	text    string // last user message text in the request
	trigger string // agent trigger label from ctx ("user", "wake", "webhook", ...)
}

// mockClient is a minimal provider.Client for HTTP endpoint tests: it returns
// a canned response and records every call in arrival order. Two optional
// channels turn it into a controllable backend — entered (non-nil) receives
// the user text as each call starts, and proceed (non-nil) blocks each call
// until the test sends a token. Together they let a test observe exactly when
// a turn reaches the backend and hold it open (serialisation tests).
type mockClient struct {
	mu    sync.Mutex
	calls []mockCall

	entered chan string
	proceed chan struct{}
}

func (m *mockClient) SendMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	var text string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			text = provider.TextOf(req.Messages[i].Content)
			break
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, mockCall{text: text, trigger: agent.TriggerFromContext(ctx)})
	m.mu.Unlock()
	if m.entered != nil {
		m.entered <- text
	}
	if m.proceed != nil {
		<-m.proceed
	}
	return &provider.MessageResponse{
		ID:         "msg_test",
		Type:       "message",
		Role:       "assistant",
		Content:    provider.TextContent(mockReply),
		StopReason: "end_turn",
		Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (m *mockClient) CountTokens(_ context.Context, _ *provider.MessageRequest) (int, error) {
	return 100, nil
}

func (m *mockClient) IsCachingAvailable() bool { return false }

// RetryBaseDelay satisfies the provider.retryableClient interface (structural
// typing) so that provider.Send uses fast retries in tests.
func (m *mockClient) RetryBaseDelay() time.Duration { return time.Millisecond }

// snapshot returns a copy of the recorded calls.
func (m *mockClient) snapshot() []mockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mockCall(nil), m.calls...)
}

// lastText returns the user text of the most recent call ("" if none).
func (m *mockClient) lastText() string {
	calls := m.snapshot()
	if len(calls) == 0 {
		return ""
	}
	return calls[len(calls)-1].text
}

// httpTestOpts configures httpTestSetup. The zero value gives a single
// "test-agent" backed by mockClient, with a registered default session
// (testSessionKey) and an empty command registry.
type httpTestOpts struct {
	promptDir string             // prompt search dir (webhook prompt files)
	webhooks  map[string]string  // hook ID → prompt path
	commands  []*command.Command // slash commands registered on the agent
	noSession bool               // skip session registration (no-session error paths)
}

// httpTestSetup builds httpHandlerDeps around a single mockClient-backed
// agent — the shared harness for every HTTP endpoint test (/send, /command,
// /wake, /webhook, and the endpoint-registration tests, which override deps
// fields before calling newTestMux).
func httpTestSetup(t *testing.T, opts httpTestOpts) (httpHandlerDeps, *mockClient) {
	t.Helper()
	mock := &mockClient{}
	sessDir := filepath.Join(t.TempDir(), "sessions")
	os.MkdirAll(sessDir, 0755)
	sessions := session.NewStore(sessDir)

	ag := &agent.Agent{
		Client:    mock,
		Sessions:  sessions,
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), nil),
		Model:     "test-model",
	}

	// A real session index backs the route.Resolver's default resolution.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	var cm platform.ConnectionManager = stubConnMgr{}
	if !opts.noSession {
		cm = stubConnMgr{agentID: testAgentID, sessionKey: testSessionKey}
		idx.Upsert(session.SessionIndexEntry{SessionKey: testSessionKey, FilePath: "x", SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
	}

	cmds := command.NewRegistry()
	for _, c := range opts.commands {
		cmds.Register(c)
	}

	inst := &agentInstance{
		id:               testAgentID,
		ag:               ag,
		cmds:             cmds,
		promptSearchDirs: []string{opts.promptDir},
		webhooks:         opts.webhooks,
	}
	inst.cc = command.CommandContext{
		Agent:        ag,
		Sessions:     sessions,
		SessionIndex: idx,
	}

	ag.SessionIndex = idx

	// The sync paths route through runAgentQueued → EnqueueInjectWait, which
	// blocks until the session's inbox worker runs the injection — workers only
	// exist once StartInbox has run (production: main.go / platform setup). A
	// cancellable ctx winds the workers down at test end.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ag.StartInbox(ctx)

	d := httpHandlerDeps{
		agents:       map[string]*agentInstance{testAgentID: inst},
		agentOrder:   []string{testAgentID},
		cfg:          &config.Config{},
		sessions:     sessions,
		sessionIndex: idx,
		connMgr:      cm,
		ctx:          ctx,
	}
	return d, mock
}

// newTestMux registers all HTTP handlers on a fresh mux for the given deps.
func newTestMux(d httpHandlerDeps) *http.ServeMux {
	mux := http.NewServeMux()
	registerHTTPHandlers(mux, d)
	return mux
}
