package codex

import (
	"context"
	"encoding/json"
	"foci/internal/delegator"
	"sync"
	"testing"
)

// TestEphemeralForkAdoptsLiveThread proves no release/resume separates the fork from its first turn.
func TestEphemeralForkAdoptsLiveThread(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	parent := setupMockBackend(t, func(method string, params json.RawMessage, _ int64) (json.RawMessage, error) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		if method == "thread/fork" {
			var p threadForkParams
			if err := json.Unmarshal(params, &p); err != nil {
				t.Error(err)
			}
			if !p.Ephemeral || !p.ExcludeTurns {
				t.Errorf("not a paginated-safe ephemeral fork: %+v", p)
			}
			return json.RawMessage(`{"approvalsReviewer":"auto_review","model":"test-model","thread":{"id":"ephemeral-child"}}`), nil
		}
		return json.RawMessage(`{"status":"unsubscribed"}`), nil
	})
	parent.threadID = "thread-1"
	parent.agentID = t.Name()
	sharedPool.Lock()
	sharedPool.servers[parent.agentID] = parent
	sharedPool.refs[parent.agentID] = 1
	sharedPool.Unlock()
	t.Cleanup(func() {
		sharedPool.Lock()
		delete(sharedPool.servers, parent.agentID)
		delete(sharedPool.refs, parent.agentID)
		sharedPool.Unlock()
	})
	res, err := parent.ForkSession(context.Background(), delegator.ForkRequest{ParentSessionID: "thread-1", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	// A parent can close before its leaf opens: the pending fork owns a server reference.
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if !parent.IsRunning() {
		t.Fatal("closing parent killed pending ephemeral fork")
	}
	child := &Backend{lg: parent.lg, shared: parent, agentID: parent.agentID, readyCh: make(chan struct{}), startOpts: delegator.StartOptions{SessionKey: "test/child", Ephemeral: true}}
	sharedPool.Lock()
	sharedPool.refs[parent.agentID]++
	sharedPool.Unlock()
	if err := child.adoptEphemeralFork(res.SessionID); err != nil {
		t.Fatal(err)
	}
	if child.SessionID() != res.SessionID || child.threadForSession("test/child") != res.SessionID {
		t.Fatal("child not bound")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, method := range methods {
		if method == "thread/resume" || method == "thread/start" {
			t.Fatalf("fork was reloaded with %s", method)
		}
	}
	if len(methods) != 2 || methods[0] != "thread/fork" || methods[1] != "thread/unsubscribe" {
		t.Fatalf("unexpected methods %v (only the closed parent's unsubscribe is allowed)", methods)
	}
}

// TestEphemeralStartAndLostFork separates fresh utility batches from missing fork history.
func TestEphemeralStartAndLostFork(t *testing.T) {
	b := setupMockBackend(t, func(method string, params json.RawMessage, _ int64) (json.RawMessage, error) {
		if method == "thread/start" {
			var p threadStartParams
			if err := json.Unmarshal(params, &p); err != nil {
				t.Error(err)
			}
			if !p.Ephemeral {
				t.Error("utility batch writes persistent history")
			}
		}
		return json.RawMessage(`{"approvalsReviewer":"auto_review","thread":{"id":"new-batch"}}`), nil
	})
	b.readyCh = make(chan struct{})
	b.startOpts.Ephemeral = true
	if _, err := b.startThread(); err != nil {
		t.Fatal(err)
	}
	if err := b.adoptEphemeralFork("missing"); err == nil {
		t.Fatal("missing live fork succeeded")
	}
}
