package main

import (
	"context"
	"sync"
	"testing"
	"time"

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
func (c *stubConn) SendDocument(string) error                       { return nil }
func (c *stubConn) SendVoice(string) error                          { return nil }
func (c *stubConn) SendVideo(string) error                          { return nil }
func (c *stubConn) SendPhoto(string) error                          { return nil }
func (c *stubConn) SendAudio(string) error                          { return nil }
func (c *stubConn) SendAnimation(string) error                      { return nil }
func (c *stubConn) SendVoiceData([]byte) error                      { return nil }
func (c *stubConn) SendTextToChat(int64, string) error              { return nil }
func (c *stubConn) SendDocumentToChat(int64, string) error          { return nil }
func (c *stubConn) SendVoiceToChat(int64, string) error             { return nil }
func (c *stubConn) SendVideoToChat(int64, string) error             { return nil }
func (c *stubConn) SendPhotoToChat(int64, string) error             { return nil }
func (c *stubConn) SendAudioToChat(int64, string) error             { return nil }
func (c *stubConn) SendAnimationToChat(int64, string) error         { return nil }
func (c *stubConn) SendVoiceDataToChat(int64, []byte) error         { return nil }
func (c *stubConn) SendInjectedMessage(string, string) error        { return nil }
func (c *stubConn) SendToSession(string, string) error              { return nil }
func (c *stubConn) SendNotification(string)                         {}
func (c *stubConn) SendNotificationDirect(string)                   {}
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
