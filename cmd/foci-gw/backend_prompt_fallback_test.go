package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"foci/internal/agent"
	"foci/internal/platform"
	"foci/internal/session"
)

// fakeAppHub is an app-shaped connection manager for the #2608 tests: every
// session resolves to a connection bound to that session (as the real hub's
// BotForSession does), but only sessions with a conversation binding can show
// buttons; the others fail exactly as appConn does with errNoBinding. With
// noConns set, no session resolves to any connection.
type fakeAppHub struct {
	stubConnMgr
	noConns bool

	mu      sync.Mutex
	bound   map[string]bool
	shownIn []string
	texts   []string
}

func (h *fakeAppHub) ForSessionOrPrimary(sessionKey, _ string) platform.Connection {
	if h.noConns {
		return nil
	}
	return &fakeAppConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: sessionKey}}, hub: h}
}

type fakeAppConn struct {
	buttonStubConn
	hub *fakeAppHub
}

func (c *fakeAppConn) PlatformName() string { return "app" }

func (c *fakeAppConn) SendTextWithButtons(text string, _ []platform.ButtonChoice, _ string) (string, error) {
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	if !c.hub.bound[c.sessionKey] {
		return "", errors.New("app: no live socket for conversation")
	}
	c.hub.shownIn = append(c.hub.shownIn, c.sessionKey)
	c.hub.texts = append(c.hub.texts, text)
	return "m-1", nil
}

// failRecorder records the failPrompt calls of a prompt test.
type failRecorder struct {
	mu      sync.Mutex
	calls   int
	sk, req string
	reason  string
}

func (r *failRecorder) fail(sk, req, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.sk, r.req, r.reason = sk, req, reason
	return nil
}

// fallbackPrompt posts one approval prompt from sessionKey through the real
// gateway closure against hub, with defaultKey as the agent's default session.
func fallbackPrompt(t *testing.T, hub *fakeAppHub, idx *session.SessionIndex, defaultKey, sessionKey, reqID string) *failRecorder {
	t.Helper()
	rec := &failRecorder{}
	approval, _ := backendPromptFuncs(backendPromptDeps{
		ag:             &agent.Agent{DelegatedManager: &agent.DelegatedManager{}},
		connMgr:        hub,
		sessionIdx:     idx,
		agentID:        "main",
		defaultSession: func() string { return defaultKey },
		failPrompt:     rec.fail,
	})
	approval(sessionKey, reqID, "Allow Bash?", "Bash", "", promptChoices())
	t.Cleanup(func() { platform.CancelInteractiveMessage(reqID, "done") })
	return rec
}

// TestBackendPrompt_BranchFallsBackToParent proves #2608 ruling 2: a fork's
// prompt that its own (non-existent) app conversation cannot show goes to the
// parent session's chat, labelled with the asking session.
func TestBackendPrompt_BranchFallsBackToParent(t *testing.T) {
	t.Parallel()
	hub := &fakeAppHub{bound: map[string]bool{"main/c1": true, "main/c9": true}}
	rec := fallbackPrompt(t, hub, nil, "main/c9", "main/c1/b100", "req-2608-parent")

	if rec.calls != 0 {
		t.Fatalf("prompt refused (%q), want it shown in the parent's chat", rec.reason)
	}
	if len(hub.shownIn) != 1 || hub.shownIn[0] != "main/c1" {
		t.Fatalf("prompt shown in %v, want [main/c1] (the parent, before the default)", hub.shownIn)
	}
	if !strings.HasPrefix(hub.texts[0], "↪️ **From session main/c1/b100**") || !strings.Contains(hub.texts[0], "Allow Bash?") {
		t.Errorf("prompt text = %q, want the asking session named first, then the body", hub.texts[0])
	}
}

// TestBackendPrompt_LogicalParentFromIndex: a branch of a branch is offered
// to its recorded parent branch before the root.
func TestBackendPrompt_LogicalParentFromIndex(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	idx.Upsert(session.SessionIndexEntry{SessionKey: "main/c1/b200", ParentSessionKey: "main/c1/b100", Status: session.SessionStatusActive})

	hub := &fakeAppHub{bound: map[string]bool{"main/c1/b100": true, "main/c1": true}}
	rec := fallbackPrompt(t, hub, idx, "", "main/c1/b200", "req-2608-logical")

	if rec.calls != 0 || len(hub.shownIn) != 1 || hub.shownIn[0] != "main/c1/b100" {
		t.Fatalf("shown in %v (refusals %d), want [main/c1/b100]", hub.shownIn, rec.calls)
	}
}

// TestBackendPrompt_FallsBackToDefaultSession: no parent chat can show it, so
// the agent's default session gets it.
func TestBackendPrompt_FallsBackToDefaultSession(t *testing.T) {
	t.Parallel()
	hub := &fakeAppHub{bound: map[string]bool{"main/c9": true}}
	rec := fallbackPrompt(t, hub, nil, "main/c9", "main/c1/b100", "req-2608-default")

	if rec.calls != 0 || len(hub.shownIn) != 1 || hub.shownIn[0] != "main/c9" {
		t.Fatalf("shown in %v (refusals %d), want [main/c9]", hub.shownIn, rec.calls)
	}
}

// TestBackendPrompt_OwnChatFirst: a session that can show its own prompt gets
// it unchanged, with no redirect line.
func TestBackendPrompt_OwnChatFirst(t *testing.T) {
	t.Parallel()
	hub := &fakeAppHub{bound: map[string]bool{"main/c1/b100": true, "main/c1": true}}
	rec := fallbackPrompt(t, hub, nil, "main/c9", "main/c1/b100", "req-2608-own")

	if rec.calls != 0 || len(hub.shownIn) != 1 || hub.shownIn[0] != "main/c1/b100" {
		t.Fatalf("shown in %v (refusals %d), want [main/c1/b100]", hub.shownIn, rec.calls)
	}
	if strings.Contains(hub.texts[0], "↪️") {
		t.Errorf("prompt text = %q, want no redirect line in the session's own chat", hub.texts[0])
	}
}

// TestBackendPrompt_UndeliverableIsRefused proves #2608 ruling 1: a prompt no
// chat can show is answered as a failure naming the reason, never left
// waiting; nothing stays registered for it.
func TestBackendPrompt_UndeliverableIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		hub  *fakeAppHub
		want string
	}{
		{"send fails everywhere", &fakeAppHub{}, "no live socket"},
		{"no connection at all", &fakeAppHub{noConns: true}, "no connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqID := "req-2608-fail-" + strings.ReplaceAll(tc.name, " ", "-")
			rec := fallbackPrompt(t, tc.hub, nil, "main/c9", "main/c1/b100", reqID)

			if rec.calls != 1 {
				t.Fatalf("refusals = %d, want 1", rec.calls)
			}
			if rec.sk != "main/c1/b100" || rec.req != reqID {
				t.Errorf("refused sk=%q req=%q, want the asking session and request", rec.sk, rec.req)
			}
			for _, part := range []string{tc.want, "main/c1/b100", "main/c1", "main/c9"} {
				if !strings.Contains(rec.reason, part) {
					t.Errorf("reason = %q, want it to contain %q", rec.reason, part)
				}
			}
			if _, _, ok := platform.HandleInteractiveCallback(reqID + ":0"); ok {
				t.Errorf("a prompt entry is still registered for %s, want none", reqID)
			}
		})
	}
}

// TestBackendPrompt_WalksDepthChainToLiveAncestor proves #2627: a prompt from
// a spawn of a nested depth leaf (spawn → L3 → L2 → L1 → root) is offered to
// every ancestor in turn, skipping the ones that are not live (done leaves),
// before the root. The old lookup tried only the immediate parent (L3), then
// the root.
func TestBackendPrompt_WalksDepthChainToLiveAncestor(t *testing.T) {
	t.Parallel()
	const (
		spawn = "main/c1/b400"
		l3    = "main/c1/b300"
		l2    = "main/c1/b200"
		l1    = "main/c1/b100"
		root  = "main/c1"
	)
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	for child, parent := range map[string]string{spawn: l3, l3: l2, l2: l1, l1: root} {
		idx.Upsert(session.SessionIndexEntry{SessionKey: child, ParentSessionKey: parent, Status: session.SessionStatusActive})
	}

	for i, tc := range []struct {
		name string
		done map[string]bool // ancestors that are not live
		want string
	}{
		{"intermediate leaf live", map[string]bool{l3: true}, l2},
		{"intermediate leaf done, next live", map[string]bool{l3: true, l2: true}, l1},
		{"every leaf done", map[string]bool{l3: true, l2: true, l1: true}, root},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every chat can show buttons, so only liveness decides; the
			// spawn's own branch has no conversation.
			hub := &fakeAppHub{bound: map[string]bool{l3: true, l2: true, l1: true, root: true}}
			rec := &failRecorder{}
			reqID := fmt.Sprintf("req-2627-%d", i)
			approval, _ := backendPromptFuncs(backendPromptDeps{
				ag:             &agent.Agent{DelegatedManager: &agent.DelegatedManager{}},
				connMgr:        hub,
				sessionIdx:     idx,
				agentID:        "main",
				defaultSession: func() string { return "main/c9" },
				failPrompt:     rec.fail,
				live:           func(k string) bool { return !tc.done[k] },
			})
			approval(spawn, reqID, "Allow Bash?", "Bash", "", promptChoices())
			t.Cleanup(func() { platform.CancelInteractiveMessage(reqID, "done") })

			if rec.calls != 0 || len(hub.shownIn) != 1 || hub.shownIn[0] != tc.want {
				t.Fatalf("shown in %v (refusals %d), want [%s]", hub.shownIn, rec.calls, tc.want)
			}
		})
	}
}

// TestPromptTargets_FullAncestorChain: every ancestor is listed in order,
// then the root, then the default session.
func TestPromptTargets_FullAncestorChain(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	for child, parent := range map[string]string{"main/c1/b3": "main/c1/b2", "main/c1/b2": "main/c1/b1", "main/c1/b1": "main/c1"} {
		idx.Upsert(session.SessionIndexEntry{SessionKey: child, ParentSessionKey: parent, Status: session.SessionStatusActive})
	}
	got := promptTargets(backendPromptDeps{sessionIdx: idx, defaultSession: func() string { return "main/c9" }}, "main/c1/b3")
	want := []string{"main/c1/b3", "main/c1/b2", "main/c1/b1", "main/c1", "main/c9"}
	if !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

// #2662: a cross-agent spawn's session offers a prompt to no chat of the
// agent it runs as — not its root, not the agent's default session.
func TestPromptTargets_CrossAgentSpawnReachesNoChat(t *testing.T) {
	t.Parallel()
	key := session.CrossAgentSpawnRoot("helen") + "/b5"
	got := promptTargets(backendPromptDeps{defaultSession: func() string { return "helen/c9" }}, key)
	if !slices.Equal(got, []string{key}) {
		t.Fatalf("targets = %v, want only the spawn's own session", got)
	}
}
