package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/memory"
	"foci/internal/platform"
)

// stubConnMgr implements platform.ConnectionManager for tests.
// Set agentID and sessionKey to make AllForAgent return a stub connection.
type stubConnMgr struct {
	agentID    string
	sessionKey string
}

func (s stubConnMgr) Primary(string) platform.Connection                    { return nil }
func (s stubConnMgr) AllForAgent(agentID string) []platform.Connection {
	if s.agentID != "" && agentID == s.agentID && s.sessionKey != "" {
		return []platform.Connection{&stubConn{sessionKey: s.sessionKey}}
	}
	return nil
}
func (s stubConnMgr) ForSession(string) platform.Connection                 { return nil }
func (s stubConnMgr) ForSessionOrPrimary(string, string) platform.Connection { return nil }
func (s stubConnMgr) AcquireFacet(string) (platform.Connection, bool)   { return nil, false }
func (s stubConnMgr) HasFacet(string) bool                              { return false }
func (s stubConnMgr) StartAll(context.Context)                              {}
func (s stubConnMgr) Wait()                                                 {}

// stubConn is a minimal Connection that returns a fixed session key.
type stubConn struct{ sessionKey string }

func (c *stubConn) SessionKey() string                              { return c.sessionKey }
func (c *stubConn) PlatformName() string                            { return "test" }
func (c *stubConn) DefaultSessionKey() string                       { return c.sessionKey }
func (c *stubConn) SessionKeyForChat(int64) string                  { return c.sessionKey }
func (c *stubConn) SetSessionKey(string)                            {}
func (c *stubConn) SetSessionKeyDirect(string)                      {}
func (c *stubConn) SetChatID(int64)                                 {}
func (c *stubConn) ChatID() int64                                   { return 0 }
func (c *stubConn) Username() string                                { return "test" }
func (c *stubConn) UpdateChatSessionKey(int64, string)              {}
func (c *stubConn) SendText(string) error                           { return nil }
func (c *stubConn) SendDocument(string, string) error              { return nil }
func (c *stubConn) SendVoice(string) error                          { return nil }
func (c *stubConn) SendVideo(string, string) error                  { return nil }
func (c *stubConn) SendPhoto(string, string) error                  { return nil }
func (c *stubConn) SendAudio(string, string) error                  { return nil }
func (c *stubConn) SendAnimation(string, string) error              { return nil }
func (c *stubConn) SendVoiceData([]byte) error                      { return nil }
func (c *stubConn) SendTextToChat(int64, string) error              { return nil }
func (c *stubConn) SendDocumentToChat(int64, string, string) error  { return nil }
func (c *stubConn) SendVoiceToChat(int64, string) error             { return nil }
func (c *stubConn) SendVideoToChat(int64, string, string) error     { return nil }
func (c *stubConn) SendPhotoToChat(int64, string, string) error     { return nil }
func (c *stubConn) SendAudioToChat(int64, string, string) error     { return nil }
func (c *stubConn) SendAnimationToChat(int64, string, string) error { return nil }
func (c *stubConn) SendVoiceDataToChat(int64, []byte) error         { return nil }
func (c *stubConn) SendInjectedMessage(string, string) error        { return nil }
func (c *stubConn) SendToSession(string, string) error              { return nil }
func (c *stubConn) SendNotification(string)                         {}
func (c *stubConn) SendNotificationDirect(string) string             { return "" }
func (c *stubConn) SetTyping(bool)                                   {}

func TestNewSessionNotifyFnParsesSlashKeys(t *testing.T) {
	// The resolver must receive the correct agent ID extracted from
	// slash-separated session keys like "clutch/c5970082313/1772794601".
	// Before the fix, colon-splitting failed on this format.
	t.Parallel()

	var mu sync.Mutex
	var resolvedAgentID string
	resolverCalled := make(chan struct{}, 1)

	resolver := func(agentID string) *agentInstance {
		mu.Lock()
		resolvedAgentID = agentID
		mu.Unlock()
		resolverCalled <- struct{}{}
		return nil // stop processing
	}

	fn := newSessionNotifyFn(resolver, context.Background(), stubConnMgr{})
	fn("clutch/c5970082313/1772794601", "test message")

	select {
	case <-resolverCalled:
		mu.Lock()
		got := resolvedAgentID
		mu.Unlock()
		if got != "clutch" {
			t.Errorf("agent ID = %q, want %q", got, "clutch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolver was not called — session key parsing likely failed")
	}
}

func TestNewSessionNotifyFnParsesBranchKeys(t *testing.T) {
	// Branch keys have a 4th segment; agent ID is the first segment.
	t.Parallel()

	var mu sync.Mutex
	var resolvedAgentID string
	resolverCalled := make(chan struct{}, 1)

	resolver := func(agentID string) *agentInstance {
		mu.Lock()
		resolvedAgentID = agentID
		mu.Unlock()
		resolverCalled <- struct{}{}
		return nil
	}

	fn := newSessionNotifyFn(resolver, context.Background(), stubConnMgr{})
	fn("fotini/c8792716180/1741826250/b1741826300", "branch message")

	select {
	case <-resolverCalled:
		mu.Lock()
		got := resolvedAgentID
		mu.Unlock()
		if got != "fotini" {
			t.Errorf("agent ID = %q, want %q", got, "fotini")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolver was not called — branch key parsing failed")
	}
}

func TestNewSessionNotifyFnRejectsGarbage(t *testing.T) {
	// Invalid keys should not call the resolver.
	t.Parallel()

	resolverCalled := make(chan struct{}, 1)
	resolver := func(agentID string) *agentInstance {
		resolverCalled <- struct{}{}
		return nil
	}

	fn := newSessionNotifyFn(resolver, context.Background(), stubConnMgr{})
	fn("not-a-valid-key", "bad message")

	select {
	case <-resolverCalled:
		t.Fatal("resolver should not be called for an invalid session key")
	case <-time.After(200 * time.Millisecond):
		// Expected: resolver not called, error logged
	}
}

func TestNewSessionNotifyFnParsesIndependentKeys(t *testing.T) {
	// Independent session keys use 'i' type prefix.
	t.Parallel()

	var mu sync.Mutex
	var resolvedAgentID string
	resolverCalled := make(chan struct{}, 1)

	resolver := func(agentID string) *agentInstance {
		mu.Lock()
		resolvedAgentID = agentID
		mu.Unlock()
		resolverCalled <- struct{}{}
		return nil
	}

	fn := newSessionNotifyFn(resolver, context.Background(), stubConnMgr{})
	fn("myagent/i1709596800/1709596800", "independent message")

	select {
	case <-resolverCalled:
		mu.Lock()
		got := resolvedAgentID
		mu.Unlock()
		if got != "myagent" {
			t.Errorf("agent ID = %q, want %q", got, "myagent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolver was not called")
	}
}

// TestNewAsyncNotifier_CrossAgentTarget_UsesResolver verifies that when the
// async notifier handles a session key owned by a different agent, it
// resolves the target's Agent via agentResolverFn rather than dispatching
// on the caller's Agent. This is the regression test for the cross-agent
// send_to_session bug: the caller's Agent has the wrong workdir / backend
// for the target's session, and dispatching there silently leaks a
// cross-workdir cc_resume_id that wedges the target's next turn.
func TestNewAsyncNotifier_CrossAgentTarget_UsesResolver(t *testing.T) {
	t.Parallel()

	resolverCalls := make(chan string, 4)
	resolver := func(aid string) *agentInstance {
		resolverCalls <- aid
		// Return nil so the notifier short-circuits with "unknown target
		// agent" — keeps the test from needing a fully-wired Agent.
		return nil
	}

	notifier := newAsyncNotifier(
		func() *agent.Agent { return nil }, // caller agent — unused on cross-agent path
		"caller",                            // caller's agentID
		resolver,
		context.Background(),
		stubConnMgr{},
	)
	// reply_to=caller path: targetSession owned by "target", replyToSession owned by "caller".
	notifier.InjectToAgent("target/c42/1000000000", "msg", "caller/c1/1000000001", "test")

	select {
	case got := <-resolverCalls:
		if got != "target" {
			t.Errorf("resolver called with %q, want %q", got, "target")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolver was not called — cross-agent target was dispatched on the caller's Agent (bug regression)")
	}
}

// TestNewAsyncNotifier_SameAgentTarget_SkipsResolver verifies the same-agent
// fast path: when the target session belongs to the caller's agent, the
// resolver should NOT be consulted (it's a same-process dispatch, getAgent()
// already returns the right Agent).
func TestNewAsyncNotifier_SameAgentTarget_SkipsResolver(t *testing.T) {
	t.Parallel()

	resolverCalls := make(chan string, 4)
	resolver := func(aid string) *agentInstance {
		resolverCalls <- aid
		return nil
	}

	notifier := newAsyncNotifier(
		func() *agent.Agent { return nil },
		"caller",
		resolver,
		context.Background(),
		stubConnMgr{},
	)
	// Same-agent target — resolver path should not fire.
	notifier.InjectToAgent("caller/c1/1000000001", "msg", "caller/c2/1000000002", "test")

	select {
	case got := <-resolverCalls:
		t.Errorf("resolver should not be called for same-agent target; got call with %q", got)
	case <-time.After(200 * time.Millisecond):
		// Expected: resolver not called.
	}
}

func TestBuildWakeSchedulerNilStore(t *testing.T) {
	// Without a reminderStore the scheduler is disabled — buildWakeScheduler
	// must return nil so callers can detect "reminders unsupported" and skip
	// remind-tool registration.
	t.Parallel()
	fn := buildWakeScheduler(func() *agent.Agent { return nil }, nil, "test", context.Background(), stubConnMgr{})
	if fn != nil {
		t.Errorf("buildWakeScheduler with nil store = non-nil, want nil")
	}
}

func TestBuildWakeSchedulerReturnsCallback(t *testing.T) {
	// With a real reminderStore, buildWakeScheduler returns a callable
	// schedule function. The function must accept a wake without error so
	// tools.NewRemindTool can use it from any transport (API or delegated).
	t.Parallel()
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := buildWakeScheduler(func() *agent.Agent { return &agent.Agent{} }, rs, "test", ctx, stubConnMgr{})
	if fn == nil {
		t.Fatal("buildWakeScheduler with valid store = nil, want non-nil")
	}
	// Schedule a wake far in the future so it never fires during the test.
	if err := fn(1, 24*time.Hour, "noop", ""); err != nil {
		t.Errorf("schedule fn returned error: %v", err)
	}
}

func TestBuildWakeSchedulerRestoresPending(t *testing.T) {
	// On agent startup, buildWakeScheduler must restore any pending wakes
	// previously stored in the DB so reminders survive restarts. We pre-seed
	// the store with one due wake and verify buildWakeScheduler returns a
	// non-nil scheduler — the restoration loop runs synchronously inside the
	// builder, so a successful return implies pending rows were processed
	// without panic.
	t.Parallel()
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	if _, err := rs.AddWake("test", "test/main", "morning routine", "24h"); err != nil {
		t.Fatalf("seed AddWake: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := buildWakeScheduler(func() *agent.Agent { return &agent.Agent{} }, rs, "test", ctx, stubConnMgr{})
	if fn == nil {
		t.Fatal("buildWakeScheduler returned nil despite valid store")
	}

	pending, err := rs.PendingWakes("test")
	if err != nil {
		t.Fatalf("PendingWakes: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("PendingWakes returned %d rows, want 1 (restoration should not consume DB rows)", len(pending))
	}
}
