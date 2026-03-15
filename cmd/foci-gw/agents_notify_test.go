package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
)

// stubConnMgr implements platform.ConnectionManager for tests.
type stubConnMgr struct{}

func (s stubConnMgr) Primary(string) platform.Connection                    { return nil }
func (s stubConnMgr) AllForAgent(string) []platform.Connection              { return nil }
func (s stubConnMgr) ForSession(string) platform.Connection                 { return nil }
func (s stubConnMgr) ForSessionOrPrimary(string, string) platform.Connection { return nil }
func (s stubConnMgr) AcquireFacet(string) (platform.Connection, bool)   { return nil, false }
func (s stubConnMgr) HasFacet(string) bool                              { return false }
func (s stubConnMgr) StartAll(context.Context)                              {}
func (s stubConnMgr) Wait()                                                 {}

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
