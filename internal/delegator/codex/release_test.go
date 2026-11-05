package codex

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// unsubscribedThreads reads the stub app-server's capture file and returns
// the threadId of every thread/unsubscribe request, in order.
func unsubscribedThreads(t *testing.T, capture string) []string {
	t.Helper()
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				ThreadID string `json:"threadId"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &msg) == nil && msg.Method == "thread/unsubscribe" {
			out = append(out, msg.Params.ThreadID)
		}
	}
	return out
}

func startFacade(t *testing.T, stub, agent, key, resume string) *Backend {
	t.Helper()
	be, _ := newFromConfig(map[string]any{"binary": stub})
	b := be.(*Backend)
	if err := b.Start(context.Background(), delegator.StartOptions{
		AgentID: agent, SessionKey: key, WorkDir: t.TempDir(), ResumeSessionID: resume,
	}); err != nil {
		t.Fatalf("start %s: %v", key, err)
	}
	return b
}

// TestCloseReleasesThreadOnSharedAppServer is the 2026-11-05 fd-leak
// regression: a session that closes while sibling sessions keep the agent's
// app-server alive must hand its thread back (thread/unsubscribe), or codex
// keeps it loaded with its rollout and writer-lock files open until the
// app-server exits. That holds for an attached facade and for the owner's own
// session alike. The last close needs no unsubscribe: the app-server exits.
func TestCloseReleasesThreadOnSharedAppServer(t *testing.T) {
	stub, capture := stubAppServer(t)
	const agent = "release-agent"
	owner := startFacade(t, stub, agent, "session/a", "")
	peer := startFacade(t, stub, agent, "session/b", "")
	third := startFacade(t, stub, agent, "session/c", "")
	ownerThread, peerThread := owner.SessionID(), peer.SessionID()

	_ = peer.Close()
	if got := unsubscribedThreads(t, capture); len(got) != 1 || got[0] != peerThread {
		t.Fatalf("after closing one attached session, unsubscribed = %q, want [%s]", got, peerThread)
	}
	if f := owner.facadeForThread(peerThread); f != nil {
		t.Errorf("released thread %s is still routed to a facade", peerThread)
	}

	_ = owner.Close() // the owner's own session; the app-server stays up for third
	if !third.IsRunning() {
		t.Fatal("app-server stopped while a session still used it")
	}
	if got := unsubscribedThreads(t, capture); len(got) != 2 || got[1] != ownerThread {
		t.Fatalf("after closing the owner's session, unsubscribed = %q, want [%s %s]", got, peerThread, ownerThread)
	}
	if third.SessionID() == "" || third.facadeForThread(third.SessionID()) != third {
		t.Fatal("the remaining session lost its thread mapping")
	}
	_ = third.Close()
}

// TestCloseLeavesThreadResumedByAnotherSession: subscriptions belong to the
// one shared connection, not to a facade. If a later session has resumed the
// same thread before the old facade finishes closing, unsubscribing would
// unload it from under the live session (and unregistering would strip its
// routing), so the old facade must leave it alone.
func TestCloseLeavesThreadResumedByAnotherSession(t *testing.T) {
	stub, capture := stubAppServer(t)
	const agent = "takeover-agent"
	keep := startFacade(t, stub, agent, "session/keep", "")
	defer keep.Close()
	old := startFacade(t, stub, agent, "session/x", "")
	thread := old.SessionID()
	fresh := startFacade(t, stub, agent, "session/x", thread) // resumes the same thread
	defer fresh.Close()

	_ = old.Close()
	for _, id := range unsubscribedThreads(t, capture) {
		if id == thread {
			t.Fatalf("closing the old facade unsubscribed thread %s, which a live session had resumed", thread)
		}
	}
	if f := fresh.facadeForThread(thread); f != fresh {
		t.Fatalf("thread %s routes to %p after the old facade closed, want the live facade %p", thread, f, fresh)
	}
	if got := fresh.SessionID(); got != thread {
		t.Fatalf("live session's thread = %q, want %q", got, thread)
	}
}

// TestResumeThreadRetriesWhileCodexUnloads: codex unloads a released thread
// 60s after thread/unsubscribe, and a resume landing in that instant is
// refused with "is closing; retry thread/resume after the thread is closed".
// Falling back to a fresh thread there would lose the session's history, so
// resumeThread waits it out.
func TestResumeThreadRetriesWhileCodexUnloads(t *testing.T) {
	oldEvery := resumeClosingRetryEvery
	resumeClosingRetryEvery = time.Millisecond
	t.Cleanup(func() { resumeClosingRetryEvery = oldEvery })

	var mu sync.Mutex
	calls := 0
	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		if method != "thread/resume" {
			return nil, nil
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			return nil, rpcErrorReply{code: -32600, message: "thread th_c is closing; retry thread/resume after the thread is closed"}
		}
		return json.RawMessage(`{"approvalsReviewer":"auto_review","thread":{"id":"th_c"}}`), nil
	})
	b.workDir = "/tmp"
	if err := b.resumeThread("th_c"); err != nil {
		t.Fatalf("resumeThread: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("thread/resume sent %d times, want 3 (two refusals, then success)", calls)
	}
}

// TestResumeThreadDoesNotRetryOtherErrors: only the closing refusal is
// retried; "no rollout found" and the like fail at once, so the manager's
// fresh-thread fallback is not delayed.
func TestResumeThreadDoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		calls++
		return nil, rpcErrorReply{code: -32600, message: "no rollout found for thread id th_n"}
	})
	if err := b.resumeThread("th_n"); err == nil {
		t.Fatal("resumeThread succeeded on a missing rollout")
	}
	if calls != 1 {
		t.Errorf("thread/resume sent %d times, want 1", calls)
	}
}

// TestForkSessionReleasesTheFork: thread/fork loads the fork and subscribes
// foci's connection to it, but its branch session (a depth leaf) may resume it
// hours later or never. ForkSession releases it at once; the branch's resume
// reloads it from its rollout.
func TestForkSessionReleasesTheFork(t *testing.T) {
	var seq methodSequence
	var unsubscribed string
	b := setupMockBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		seq.record(method)
		switch method {
		case "thread/fork":
			return json.RawMessage(`{"approvalsReviewer":"auto_review","thread":{"id":"fork-1"}}`), nil
		case "thread/unsubscribe":
			var p threadUnsubscribeParams
			_ = json.Unmarshal(params, &p)
			unsubscribed = p.ThreadID
			return json.RawMessage(`{"status":"unsubscribed"}`), nil
		}
		return nil, nil
	})
	if _, err := b.ForkSession(context.Background(), delegator.ForkRequest{ParentSessionID: "parent"}); err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if got := seq.snapshot(); len(got) != 2 || got[0] != "thread/fork" || got[1] != "thread/unsubscribe" {
		t.Fatalf("methods = %q, want [thread/fork thread/unsubscribe]", got)
	}
	if unsubscribed != "fork-1" {
		t.Errorf("unsubscribed %q, want the fork fork-1", unsubscribed)
	}
	if !b.isPendingFork("fork-1") {
		t.Error("the released fork is no longer known as foci's pending fork")
	}
}

// TestThreadClosedForReleasedThreadIsNotLost: ~60s after a release codex
// sends thread/closed for a thread foci has already unmapped. That is the
// release completing, not a lost line, so it must not raise the lost-line WARN.
func TestThreadClosedForReleasedThreadIsNotLost(t *testing.T) {
	owner, _, _, _, _ := newSharedPair(t)
	stopUnclaimedTimer(t, owner)
	warns := captureWarns(t)

	start := time.Now()
	owner.dispatch([]byte(`{"method":"thread/closed","params":{"threadId":"released-thread"}}`))
	owner.flushUnclaimed(start.Add(unclaimedGrace+time.Second), false)
	if got := lostWarns(warns()); len(got) != 0 {
		t.Fatalf("thread/closed for a released thread was reported lost: %q", got)
	}
}

// TestReleasedThreadLinesAreNotLost: a thread released while its turn still
// runs keeps running to the end, and codex keeps sending its status changes
// to the (unsubscribed) connection until it unloads it — live 0.166.0, two
// thread/status/changed then thread/closed. foci unmapped the thread on
// release, so those lines must not raise the lost-line WARN. Once
// thread/closed arrives the thread is forgotten, and a later stray line for it
// is reported again (which also shows the WARN check is live).
func TestReleasedThreadLinesAreNotLost(t *testing.T) {
	b := setupMockBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		if method == "thread/unsubscribe" {
			return json.RawMessage(`{"status":"unsubscribed"}`), nil
		}
		return nil, nil
	})
	stopUnclaimedTimer(t, b)
	warns := captureWarns(t)

	b.releaseThread("thread-1")
	if b.facadeForThread("thread-1") != nil {
		t.Fatal("released thread is still mapped")
	}
	start := time.Now()
	status := []byte(`{"method":"thread/status/changed","params":{"threadId":"thread-1","status":{"type":"idle"}}}`)
	b.dispatch(status)
	b.dispatch([]byte(`{"method":"thread/closed","params":{"threadId":"thread-1"}}`))
	b.flushUnclaimed(start.Add(unclaimedGrace+time.Second), false)
	if got := lostWarns(warns()); len(got) != 0 {
		t.Fatalf("lines of a released thread were reported lost: %q", got)
	}
	if b.isReleased("thread-1") {
		t.Fatal("thread/closed did not clear the released entry")
	}

	start = time.Now()
	b.dispatch(status)
	b.flushUnclaimed(start.Add(unclaimedGrace+time.Second), false)
	if got := lostWarns(warns()); len(got) != 1 {
		t.Fatalf("a line after thread/closed: lost WARNs = %q, want one", got)
	}
}
