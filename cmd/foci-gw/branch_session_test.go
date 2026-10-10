package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/defersend"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/timeutil"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// delegatedReply is the canned text noBranchBackend completes every turn with.
const delegatedReply = "delegated reply"

// registerRoot adds key to the session index as an active independent root
// and re-pins the harness's DEFAULT resolution on testSessionKey. The pin
// matters: the default rung's fallback orders roots by
// last_user_activity_at DESC NULLS LAST (index.go), so a freshly upserted
// row would otherwise win and silently change which session "no selector"
// means for every later request in the test.
func registerRoot(t *testing.T, d httpHandlerDeps, key string) {
	t.Helper()
	d.sessionIndex.Upsert(session.SessionIndexEntry{
		SessionKey: key, FilePath: "x", SessionType: session.SessionTypeIndependent, Status: session.SessionStatusActive,
	})
	d.sessionIndex.TouchUserActivity(testSessionKey, time.Now())
}

// seedMarker writes one user marker message into key's session file and
// registers the key (registerRoot), so a branch of key reads the marker
// from history while the default session keeps its own, different marker.
func seedMarker(t *testing.T, d httpHandlerDeps, key, marker string) {
	t.Helper()
	if err := d.sessions.TestAppend(key, provider.Message{Role: "user", Content: provider.TextContent(marker)}); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	registerRoot(t, d, key)
}

// historyTexts returns every "role:text" of the last provider request, so a
// test can prove WHICH session's history a branch read.
func historyTexts(t *testing.T, mock *mockClient) string {
	t.Helper()
	calls := mock.snapshot()
	if len(calls) != 1 {
		t.Fatalf("backend calls = %d, want exactly one branch turn", len(calls))
	}
	return strings.Join(calls[0].allTexts, "\n")
}

// TestBranch_UnknownSelectorCreatesEmptyNamedParent pins Dick's 2026-10-09
// ruling (requirement 3): a /branch selector that matches no key, named
// session or alias is a valid NAME — the resolver's create rung mints
// <agent>/i<name> and the branch is made from that new, empty session. The
// caller gets a 200 and an answer with no inherited context; nothing errors.
func TestBranch_UnknownSelectorCreatesEmptyNamedParent(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"q","session":"nosuch"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an unknown name creates a session); body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != mockReply {
		t.Errorf("response = %q, want %q", resp["response"], mockReply)
	}
	if want := testAgentID + "/inosuch/b"; !strings.HasPrefix(resp["session"], want) {
		t.Errorf("session = %q, want a branch of the created key (prefix %q)", resp["session"], want)
	}
	if resp["resolved_via"] != "branch" {
		t.Errorf("resolved_via = %q, want branch", resp["resolved_via"])
	}
	if calls := mock.snapshot(); len(calls) != 1 {
		t.Errorf("backend calls = %d, want 1 (the branch turn ran on the empty session)", len(calls))
	}
}

// TestBranch_NamedSessionHistory pins requirement 4 for an API agent: a
// branch made from a named session reads THAT session's history — not the
// default session's — and the receipt names a branch of the named key.
func TestBranch_NamedSessionHistory(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	seedMarker(t, d, testSessionKey, "default marker")
	seedMarker(t, d, testAgentID+"/iside", "named marker")
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"q","session":"side"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if want := testAgentID + "/iside/b"; !strings.HasPrefix(resp["session"], want) {
		t.Fatalf("session = %q, want a branch of the named key (prefix %q)", resp["session"], want)
	}
	if resp["resolved_via"] != "branch" {
		t.Errorf("resolved_via = %q, want branch", resp["resolved_via"])
	}
	history := historyTexts(t, mock)
	if !strings.Contains(history, "named marker") {
		t.Errorf("the branch did not read the named session's history: provider saw %q", history)
	}
	if strings.Contains(history, "default marker") {
		t.Errorf("the branch read the DEFAULT session's history: provider saw %q", history)
	}
}

// TestBranch_AliasSessionHistory pins requirement 4's alias case: a branch
// made from a chat-alias selector reads that alias's session history, and
// the receipt names a branch of the alias's chat key.
func TestBranch_AliasSessionHistory(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	if err := d.sessionIndex.SetChatAliasUnique(testAgentID, "app", 77, "Fabro"); err != nil {
		t.Fatal(err)
	}
	seedMarker(t, d, testSessionKey, "default marker")
	seedMarker(t, d, testAgentID+"/c77", "alias marker")
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"q","session":"Fabro"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if want := testAgentID + "/c77/b"; !strings.HasPrefix(resp["session"], want) {
		t.Fatalf("session = %q, want a branch of the alias's chat key (prefix %q)", resp["session"], want)
	}
	if resp["resolved_via"] != "branch" {
		t.Errorf("resolved_via = %q, want branch", resp["resolved_via"])
	}
	history := historyTexts(t, mock)
	if !strings.Contains(history, "alias marker") {
		t.Errorf("the branch did not read the alias session's history: provider saw %q", history)
	}
	if strings.Contains(history, "default marker") {
		t.Errorf("the branch read the DEFAULT session's history: provider saw %q", history)
	}
}

// TestBranch_SyncSilentNamedParentNoChatDelivery pins requirement 6 on the
// gateway: a sync, silent /branch with a named-session selector returns the
// reply in the HTTP body with a branch receipt, and no connection gets any
// send — not the named session's chat, nor the agent's primary.
func TestBranch_SyncSilentNamedParentNoChatDelivery(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	seedMarker(t, d, testSessionKey, "default marker")
	seedMarker(t, d, testAgentID+"/iside", "named marker")
	conn := newRecordingConn(testAgentID + "/iside")
	// Primary answers ANY agent here, so a delivery that fell back to the
	// agent's primary chat would still be recorded and caught.
	d.connMgr = recordingConnMgr{sessionKey: testAgentID + "/iside", conn: conn}
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"q","session":"side","silent":true,"async":false}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != mockReply {
		t.Errorf("response = %q, want %q in the body", resp["response"], mockReply)
	}
	if want := testAgentID + "/iside/b"; !strings.HasPrefix(resp["session"], want) {
		t.Errorf("session = %q, want a branch of the named key (prefix %q)", resp["session"], want)
	}
	if resp["resolved_via"] != "branch" {
		t.Errorf("resolved_via = %q, want branch", resp["resolved_via"])
	}
	if _, texts := conn.snapshot(); len(texts) != 0 {
		t.Errorf("sync --silent delivered to a chat: %q, want no send on any connection", texts)
	}
}

// noBranchBackend is the #2284 cannot-branch double: a delegator.Delegator
// that deliberately does NOT implement delegator.BackendBrancher, so a
// manager built with it reports BackendCanBranch() == false and /branch
// takes the send fallback. Its turns complete synchronously —
// ImmediateInject fires inj.Turn.OnTurnComplete inline (the mockBackendDM
// seam, internal/agent/delegated_manager_test.go) — so the fallback turn
// the pre-#2284 code runs finishes cleanly: no hang, no goroutine to join.
type noBranchBackend struct {
	summaryModelBackend // the ~17 no-op Delegator methods

	injMu   sync.Mutex
	injects []string
}

var _ delegator.Delegator = (*noBranchBackend)(nil)

func (b *noBranchBackend) ImmediateInject(_ context.Context, inj delegator.Inject) error {
	b.injMu.Lock()
	b.injects = append(b.injects, inj.Text)
	b.injMu.Unlock()
	if inj.Turn != nil && inj.Turn.OnTurnComplete != nil {
		inj.Turn.OnTurnComplete(&delegator.TurnResult{Text: delegatedReply})
	}
	return nil
}

// injectSnapshot returns a copy of the injected texts, in order.
func (b *noBranchBackend) injectSnapshot() []string {
	b.injMu.Lock()
	defer b.injMu.Unlock()
	return append([]string(nil), b.injects...)
}

// delegatedBranchHarness swaps the harness's API agent for a delegated one
// whose backend cannot branch: a real DelegatedManager over noBranchBackend.
// The /branch path (runBranchTurn) consults only inst.ag, so the API
// harness's inst.cmds/inst.cc wiring going stale is harmless. StartInbox is
// required by the sync path (EnqueueInjectWait — see httpTestSetup).
func delegatedBranchHarness(t *testing.T, opts httpTestOpts) (httpHandlerDeps, *noBranchBackend) {
	t.Helper()
	d, _ := httpTestSetup(t, opts)
	be := &noBranchBackend{}
	mgr := &agent.DelegatedManager{
		AgentID:    testAgentID,
		StartOpts:  delegator.StartOptions{AgentID: testAgentID, WorkDir: t.TempDir()},
		NewBackend: func() (delegator.Delegator, error) { return be, nil },
	}
	t.Cleanup(mgr.Close)
	ag := &agent.Agent{
		AgentID:          testAgentID,
		Sessions:         d.sessions,
		Tools:            tools.NewRegistry(),
		Bootstrap:        workspace.NewBootstrap(t.TempDir(), nil),
		Model:            "test-model",
		DelegatedManager: mgr,
		SessionIndex:     d.sessionIndex,
	}
	d.agents[testAgentID].ag = ag
	ag.StartInbox(d.ctx)
	return d, be
}

// TestBranch_CannotBranchMainFallsThrough pins R1's option-B ruling: when
// the backend cannot branch and the resolved parent IS the agent's default
// session, /branch keeps today's behaviour exactly — the request degrades
// to a send on the parent (the caller's default-resolution receipt), the
// backend sees exactly one turn, and no branch session is created. "Main"
// is every shape the EMPTY selector can resolve to: the plainly resolved
// default, a CreateDefault-minted key (#1859 — an agent whose every
// conversation is archived gets one minted rather than a 412), and — given
// explicitly — the default key itself.
func TestBranch_CannotBranchMainFallsThrough(t *testing.T) {
	// The plainly resolved default session: no selector at all.
	t.Run("resolved default", func(t *testing.T) {
		d, be := delegatedBranchHarness(t, httpTestOpts{})
		mux := newTestMux(d)

		w := postJSON(mux, "/branch", `{"text":"q"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (the main-session fall-through stays); body: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["response"] != delegatedReply {
			t.Errorf("response = %q, want the delegated turn's %q", resp["response"], delegatedReply)
		}
		if resp["session"] != testSessionKey {
			t.Errorf("session = %q, want the parent %q (the fallback runs the turn on the parent)", resp["session"], testSessionKey)
		}
		if resp["resolved_via"] != "default" {
			t.Errorf("resolved_via = %q, want default (the caller's resolution receipt)", resp["resolved_via"])
		}
		if inj := be.injectSnapshot(); len(inj) != 1 {
			t.Errorf("backend injects = %q, want exactly one turn on the parent", inj)
		}
		entries, err := d.sessions.ScanAllSessions()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.SessionKey, testSessionKey+"/b") {
				t.Errorf("branch session %q created — a backend that cannot branch must not mint one", e.SessionKey)
			}
		}
	})

	// A main key minted by the CreateDefault hook (#1859): no visible
	// default session, so the empty selector resolves through the create
	// hook. That key is still main — the fall-through must survive it, not
	// be refused by the plain default-key comparison (review finding: the
	// minted key is invisible to defaultSessionKey).
	t.Run("CreateDefault-minted default", func(t *testing.T) {
		d, be := delegatedBranchHarness(t, httpTestOpts{noSession: true})
		d.createDefault = func(agentID string) (string, error) {
			return testAgentID + "/c99", nil
		}
		mux := newTestMux(d)

		w := postJSON(mux, "/branch", `{"text":"q"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a CreateDefault-minted main key keeps the fall-through); body: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["session"] != testAgentID+"/c99" {
			t.Errorf("session = %q, want the minted main key %q", resp["session"], testAgentID+"/c99")
		}
		if resp["response"] != delegatedReply {
			t.Errorf("response = %q, want the delegated turn's %q", resp["response"], delegatedReply)
		}
		if inj := be.injectSnapshot(); len(inj) != 1 {
			t.Errorf("backend injects = %q, want exactly one turn on the minted main session", inj)
		}
	})

	// The default key given EXPLICITLY as the selector (RungExact): the
	// resolved parent equals the default key, so the fall-through stays.
	t.Run("explicit default-key selector", func(t *testing.T) {
		d, be := delegatedBranchHarness(t, httpTestOpts{})
		mux := newTestMux(d)

		w := postJSON(mux, "/branch", `{"text":"q","session":"i0"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (the default key by name keeps the fall-through); body: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["session"] != testSessionKey {
			t.Errorf("session = %q, want the parent %q", resp["session"], testSessionKey)
		}
		if resp["resolved_via"] != "exact" {
			t.Errorf("resolved_via = %q, want exact (the selector was the full key)", resp["resolved_via"])
		}
		if inj := be.injectSnapshot(); len(inj) != 1 {
			t.Errorf("backend injects = %q, want exactly one turn on the parent", inj)
		}
	})
}

// TestBranch_CannotBranchNamedParentRefused is the R5 red test: a backend
// that cannot branch must NEVER run the turn on a named (non-default)
// parent — the fallback would interrupt the very session the caller asked
// to leave alone. The request is refused with a 422 naming the agent and
// the session and saying nothing ran; the backend is never started, nothing
// is injected, and the parent's session file and index row are unchanged.
func TestBranch_CannotBranchNamedParentRefused(t *testing.T) {
	t.Run("named parent", func(t *testing.T) {
		d, be := delegatedBranchHarness(t, httpTestOpts{})
		// Seed the parent with a message and an index row, so "unchanged"
		// below compares real state, not two empties.
		seedMarker(t, d, testAgentID+"/iside", "parent marker")
		before, err := d.sessions.Load(testAgentID + "/iside")
		if err != nil {
			t.Fatalf("load parent: %v", err)
		}
		if len(before) == 0 {
			t.Fatal("seeding failed: parent has no messages before the request")
		}
		rowBefore, err := d.sessionIndex.Get(testAgentID + "/iside")
		if err != nil {
			t.Fatalf("index row before: %v", err)
		}
		mux := newTestMux(d)

		w := postJSON(mux, "/branch", `{"text":"q","session":"side"}`)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("R5: status = %d, want 422 (a named parent must not fall through to a turn); body: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, want := range []string{"cannot branch", testAgentID, testAgentID + "/iside", "nothing ran"} {
			if !strings.Contains(body, want) {
				t.Errorf("R5: refusal body %q does not say %q", body, want)
			}
		}
		if starts := len(be.started); starts != 0 {
			t.Errorf("R5: backend started %d time(s), want 0 — nothing may run", starts)
		}
		if inj := be.injectSnapshot(); len(inj) != 0 {
			t.Errorf("R5: backend injects = %q, want none — the parent session got no turn", inj)
		}
		after, err := d.sessions.Load(testAgentID + "/iside")
		if err != nil {
			t.Fatalf("reload parent: %v", err)
		}
		if len(after) != len(before) {
			t.Errorf("R5: parent session changed: %d message(s) before, %d after — nothing may touch it", len(before), len(after))
		}
		rowAfter, err := d.sessionIndex.Get(testAgentID + "/iside")
		if err != nil {
			t.Fatalf("index row after: %v", err)
		}
		// Compare as instants: time.Time's == is zone-sensitive, and the
		// zero timestamps come back from SQLite in a fixed -0001 zone.
		if rowAfter.Status != rowBefore.Status ||
			rowAfter.SessionType != rowBefore.SessionType ||
			!rowAfter.LastActivityAt.Equal(rowBefore.LastActivityAt) ||
			!rowAfter.LastUserActivityAt.Equal(rowBefore.LastUserActivityAt) {
			t.Errorf("R5: parent index row changed: %+v before, %+v after — a refused request stamps no activity", rowBefore, rowAfter)
		}
	})

	// With no default session at all, every parent is a named one: an
	// unknown selector creates the empty key, and the refusal still holds
	// (the empty-selector 412 path never runs — a selector was given).
	t.Run("no default session", func(t *testing.T) {
		d, be := delegatedBranchHarness(t, httpTestOpts{noSession: true})
		mux := newTestMux(d)

		w := postJSON(mux, "/branch", `{"text":"q","session":"side"}`)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("R5: status = %d, want 422 (no default exists, so any parent is named); body: %s", w.Code, w.Body.String())
		}
		if starts := len(be.started); starts != 0 {
			t.Errorf("R5: backend started %d time(s), want 0 — nothing may run", starts)
		}
		if inj := be.injectSnapshot(); len(inj) != 0 {
			t.Errorf("R5: backend injects = %q, want none", inj)
		}
	})
}

// TestBranch_CannotBranchRefusedBeforeDispatch pins WHERE in runBranchTurn
// the R5 refusal sits: before every dispatch shape and before the model
// override, so neither an async request (which would answer 202 and enqueue
// a turn on the named parent) nor a model override (which a delegated agent
// rejects with its own 400) ever runs. A delegated agent cannot store a
// per-session model override at all — applyModelOverride rejects delegated
// agents outright — so the ordering is the observable: the 422 says the
// backend cannot branch, not that the model was refused.
func TestBranch_CannotBranchRefusedBeforeDispatch(t *testing.T) {
	t.Run("async never enqueues", func(t *testing.T) {
		d, be := delegatedBranchHarness(t, httpTestOpts{})
		seedMarker(t, d, testAgentID+"/iside", "parent marker")
		mux := newTestMux(d)

		w := postJSON(mux, "/branch", `{"text":"q","session":"side","async":true}`)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("R5: status = %d, want 422 (async must not turn the refusal into a 202 + a turn on the named parent); body: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "queued") {
			t.Errorf("R5: async refusal answered 422 with a queued receipt: %s", w.Body.String())
		}
		if inj := be.injectSnapshot(); len(inj) != 0 {
			t.Errorf("R5: backend injects = %q, want none — nothing was ever enqueued", inj)
		}
		entries, err := d.sessions.ScanAllSessions()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.SessionKey, testAgentID+"/iside/b") {
				t.Errorf("R5: branch session %q minted for an async refusal", e.SessionKey)
			}
		}
	})

	t.Run("model override never applied", func(t *testing.T) {
		d, be := delegatedBranchHarness(t, httpTestOpts{})
		seedMarker(t, d, testAgentID+"/iside", "parent marker")
		mux := newTestMux(d)

		w := postJSON(mux, "/branch", `{"text":"q","session":"side","model":"test-model"}`)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("R5: status = %d, want 422 (the refusal precedes the model override); body: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "cannot branch") {
			t.Errorf("R5: refusal body %q must name the cannot-branch cause, not a model rejection", body)
		}
		if strings.Contains(body, "model") {
			t.Errorf("R5: refusal body %q mentions the model — the override was applied before the refusal", body)
		}
		if inj := be.injectSnapshot(); len(inj) != 0 {
			t.Errorf("R5: backend injects = %q, want none", inj)
		}
	})
}

// TestSweep_BranchNamedParentRefusedWhenBackendCannotBranch pins the R5
// refusal on the deferred sweep's entry point: a wait-deferred /branch whose
// stored parent is a named session is refused at delivery time by the same
// shared runBranchTurn — the refusal WARN is logged synchronously during the
// sweep, no turn is ever enqueued, no branch session is minted, and the
// record is dropped after that one attempt (never retried forever).
func TestSweep_BranchNamedParentRefusedWhenBackendCannotBranch(t *testing.T) {
	d, be := delegatedBranchHarness(t, httpTestOpts{})
	registerRoot(t, d, testAgentID+"/iside")
	store := withDeferStore(t, &d)

	var mu sync.Mutex
	refused := 0
	// Drain any warnings buffered before this point first: SetWarnHook
	// replays the startup buffer into the new hook, and earlier tests'
	// refusals must not count here.
	log.SetWarnHook(func(log.Level, string, string) {})
	log.SetWarnHook(func(level log.Level, component, msg string) {
		if component == "branch" && strings.Contains(msg, "refused branch on named parent") {
			mu.Lock()
			refused++
			mu.Unlock()
		}
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	now := timeutil.Now()
	// wait_cold holds (the named session never ran a turn) → deliverable on
	// the first sweep.
	_, _ = store.Enqueue(defersend.Record{
		Kind: defersend.KindBranch, AgentID: testAgentID, SessionKey: testAgentID + "/iside",
		Text: "deferred branch", WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	sweepFor(d, store).sweep()

	mu.Lock()
	defer mu.Unlock()
	if refused != 1 {
		t.Fatalf("R5: refusal WARN logged %d time(s) during the sweep, want exactly 1 — the deferred branch must be refused by the shared runBranchTurn", refused)
	}
	// The refusal returns before any dispatch, so nothing was ever enqueued:
	// an immediate check is race-free (no envelope exists to run later).
	if inj := be.injectSnapshot(); len(inj) != 0 {
		t.Errorf("R5: backend injects = %q, want none — the sweep must not run a turn on the named parent", inj)
	}
	entries, err := d.sessions.ScanAllSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.SessionKey, testAgentID+"/iside/b") {
			t.Errorf("R5: branch session %q minted by the sweep", e.SessionKey)
		}
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("R5: store not drained after the refused delivery: %d record(s) — a refusal must not retry forever", len(all))
	}
}
