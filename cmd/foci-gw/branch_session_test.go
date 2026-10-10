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

// delegatedBranchHarnessWith is delegatedBranchHarness parameterised on the
// backend double and its capability Spec: the wiring is identical except the
// manager's NewBackend and Spec. A double that implements
// delegator.BackendBrancher should carry a Spec declaring CapBranch (and one
// that does not, a Spec declaring nothing), so the declaration always
// matches the method set — the ethos of Spec declarations (#2154).
func delegatedBranchHarnessWith(t *testing.T, opts httpTestOpts, be delegator.Delegator, spec delegator.Spec) httpHandlerDeps {
	t.Helper()
	d, _ := httpTestSetup(t, opts)
	mgr := &agent.DelegatedManager{
		AgentID:    testAgentID,
		StartOpts:  delegator.StartOptions{AgentID: testAgentID, WorkDir: t.TempDir()},
		NewBackend: func() (delegator.Delegator, error) { return be, nil },
		Spec:       spec,
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
	return d
}

// delegatedBranchHarness swaps the harness's API agent for a delegated one
// whose backend cannot branch: a real DelegatedManager over noBranchBackend
// (its Spec declares nothing, matching the double's missing
// delegator.BackendBrancher method set). The /branch path (runBranchTurn)
// consults only inst.ag, so the API harness's inst.cmds/inst.cc wiring
// going stale is harmless. StartInbox is required by the sync path
// (EnqueueInjectWait — see httpTestSetup).
func delegatedBranchHarness(t *testing.T, opts httpTestOpts) (httpHandlerDeps, *noBranchBackend) {
	t.Helper()
	be := &noBranchBackend{}
	return delegatedBranchHarnessWith(t, opts, be, delegator.Spec{}), be
}

// branchCapableBackend is noBranchBackend's branching twin: the same
// synchronous ImmediateInject double plus an honest delegator.BackendBrancher
// method set (no-op fork/cleanup, mirroring internal/agent/branch_test.go's
// brancherBackend), to pair with a Spec that declares CapBranch.
type branchCapableBackend struct{ noBranchBackend }

var _ delegator.BackendBrancher = (*branchCapableBackend)(nil)

func (b *branchCapableBackend) ForkSession(_ context.Context, _ delegator.ForkRequest) (delegator.ForkResult, error) {
	return delegator.ForkResult{SessionID: "forked"}, nil
}

func (b *branchCapableBackend) CleanupSession(_ context.Context, _ delegator.CleanupRequest) error {
	return nil
}

// capBranchSpec is branchCapableBackend's Spec: it declares exactly
// CapBranch, which is what BackendCanBranch asks.
var capBranchSpec = delegator.Spec{Caps: map[delegator.Capability]delegator.Support{delegator.CapBranch: delegator.Yes()}}

// delegatedBranchingHarness is delegatedBranchHarness over a backend that
// CAN branch (Spec declares CapBranch), so /branch takes the real-fork
// path instead of the send fallback.
func delegatedBranchingHarness(t *testing.T, opts httpTestOpts) (httpHandlerDeps, *branchCapableBackend) {
	t.Helper()
	be := &branchCapableBackend{}
	return delegatedBranchHarnessWith(t, opts, be, capBranchSpec), be
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

// warmNamedParent seeds the named parent session and makes it WARM, so a
// wait_cold duration does not hold now: a wait-carrying /branch against it
// is deferral-shaped on the pre-#2301 code.
func warmNamedParent(t *testing.T, d httpHandlerDeps) {
	t.Helper()
	seedMarker(t, d, testAgentID+"/iside", "parent marker")
	d.sessionIndex.TouchCacheTouch(testAgentID+"/iside", time.Now())
}

// warnRecorder drains the warn buffer and then records every branch WARN
// line for the rest of the test (the SetWarnHook replay pattern from
// TestSweep_BranchNamedParentRefusedWhenBackendCannotBranch).
func warnRecorder(t *testing.T) func() []string {
	t.Helper()
	log.SetWarnHook(func(log.Level, string, string) {})
	var mu sync.Mutex
	var lines []string
	log.SetWarnHook(func(_ log.Level, component, msg string) {
		if component == "branch" {
			mu.Lock()
			lines = append(lines, msg)
			mu.Unlock()
		}
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// TestBranch_DeferredCannotBranchNamedParentRefusedAtEnqueue is the #2301 red
// test: a /branch carrying a wait condition that names a NON-default parent
// on a delegated backend that cannot branch is refused at ENQUEUE with the
// same 422 — same shared refusal — the request gets without the wait. On the
// pre-#2301 code the request was stored and answered 202 "deferred"; the
// refusal surfaced only as a WARN at delivery, and the caller never learned
// nothing ran.
func TestBranch_DeferredCannotBranchNamedParentRefusedAtEnqueue(t *testing.T) {
	// namedWarm seeds a WARM named parent, so wait_cold:"1h" does not hold
	// now and the base code defers.
	namedWarm := func(t *testing.T) (*noBranchBackend, *defersend.Store, *http.ServeMux) {
		d, be := delegatedBranchHarness(t, httpTestOpts{})
		warmNamedParent(t, d)
		store := withDeferStore(t, &d)
		return be, store, newTestMux(d)
	}

	t.Run("422, nothing stored, nothing ran", func(t *testing.T) {
		be, store, mux := namedWarm(t)

		w := postJSON(mux, "/branch", `{"text":"later","session":"side","wait_cold":"1h"}`)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("#2301: status = %d, want 422 at enqueue (base: 202 deferred, refused only at delivery); body: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, want := range []string{"cannot branch", testAgentID, testAgentID + "/iside", "nothing ran"} {
			if !strings.Contains(body, want) {
				t.Errorf("#2301: refusal body %q does not say %q", body, want)
			}
		}
		if all, _ := store.All(); len(all) != 0 {
			t.Errorf("#2301: defer store holds %d record(s), want 0 — a refused request stores nothing", len(all))
		}
		if starts := len(be.started); starts != 0 {
			t.Errorf("#2301: backend started %d time(s), want 0", starts)
		}
		if inj := be.injectSnapshot(); len(inj) != 0 {
			t.Errorf("#2301: backend injects = %q, want none — nothing ran", inj)
		}
	})

	t.Run("sync caller gets the same 422", func(t *testing.T) {
		be, store, mux := namedWarm(t)

		w := postJSON(mux, "/branch", `{"text":"later","session":"side","wait_cold":"1h","async":false}`)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("#2301: status = %d, want the same 422 for a sync caller; body: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "deferred") {
			t.Errorf("#2301: sync refusal answered with a deferred receipt: %s", w.Body.String())
		}
		if all, _ := store.All(); len(all) != 0 {
			t.Errorf("#2301: defer store holds %d record(s), want 0", len(all))
		}
		if inj := be.injectSnapshot(); len(inj) != 0 {
			t.Errorf("#2301: backend injects = %q, want none", inj)
		}
	})

	t.Run("same message as the immediate refusal", func(t *testing.T) {
		_, _, mux := namedWarm(t)

		waited := postJSON(mux, "/branch", `{"text":"later","session":"side","wait_cold":"1h"}`)
		immediate := postJSON(mux, "/branch", `{"text":"later","session":"side"}`)

		if waited.Code != http.StatusUnprocessableEntity || immediate.Code != http.StatusUnprocessableEntity {
			t.Fatalf("#2301: waited=%d immediate=%d, want both 422", waited.Code, immediate.Code)
		}
		if waited.Body.String() != immediate.Body.String() {
			t.Errorf("#2301: the enqueue refusal and the immediate refusal disagree (R2: one message):\nwaited:    %s\nimmediate: %s",
				waited.Body.String(), immediate.Body.String())
		}
	})

	t.Run("met condition still refuses at enqueue", func(t *testing.T) {
		// The parent is COLD, so wait_cold HOLDS — the deferral below would
		// be skipped. The enqueue gate fires on wait PRESENCE (R1's test),
		// so the refusal is the enqueue one and the caller still gets the
		// same 422 the run path would give.
		d, _ := delegatedBranchHarness(t, httpTestOpts{})
		seedMarker(t, d, testAgentID+"/iside", "parent marker") // cold: never touched
		store := withDeferStore(t, &d)
		warns := warnRecorder(t)
		mux := newTestMux(d)

		w := postJSON(mux, "/branch", `{"text":"later","session":"side","wait_cold":"1m"}`)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("#2301: status = %d, want 422 (a met condition is no escape: delivery could only end in the refusal); body: %s", w.Code, w.Body.String())
		}
		refusals := 0
		for _, msg := range warns() {
			if strings.Contains(msg, "refused branch on named parent") {
				refusals++
				if !strings.Contains(msg, "at enqueue") {
					t.Errorf("#2301: a holding condition was refused by the run-time gate, not the enqueue one: %q", msg)
				}
			}
		}
		if refusals != 1 {
			t.Errorf("#2301: refusal WARNs = %d, want exactly 1", refusals)
		}
		if all, _ := store.All(); len(all) != 0 {
			t.Errorf("#2301: defer store holds %d record(s), want 0", len(all))
		}
	})

	t.Run("logged once at WARN marked at enqueue", func(t *testing.T) {
		be, store, mux := namedWarm(t)
		warns := warnRecorder(t)

		w := postJSON(mux, "/branch", `{"text":"later","session":"side","wait_cold":"1h"}`)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("#2301: status = %d, want 422; body: %s", w.Code, w.Body.String())
		}
		var enqueueMarked, unmarked int
		for _, msg := range warns() {
			switch {
			case strings.Contains(msg, "refused branch on named parent at enqueue"):
				enqueueMarked++
			case strings.Contains(msg, "refused branch on named parent"):
				unmarked++
			}
		}
		if enqueueMarked != 1 {
			t.Errorf("#2301: enqueue-marked refusal WARNs = %d, want exactly 1 (lines: %q)", enqueueMarked, warns())
		}
		if unmarked != 0 {
			t.Errorf("#2301: run-time refusal WARNs = %d, want 0 — nothing reached runBranchTurn (lines: %q)", unmarked, warns())
		}
		if all, _ := store.All(); len(all) != 0 {
			t.Errorf("#2301: defer store holds %d record(s), want 0", len(all))
		}
		if inj := be.injectSnapshot(); len(inj) != 0 {
			t.Errorf("#2301: backend injects = %q, want none", inj)
		}
	})
}

// TestBranch_DeferredDefaultParentStillDefersWhenCannotBranch pins #2301's
// "everything else unchanged": on a backend that cannot branch, a
// wait-carrying branch of the agent's DEFAULT session still defers — its
// delivery ends in the allowed send fallback, so the enqueue refusal must
// not swallow it.
func TestBranch_DeferredDefaultParentStillDefersWhenCannotBranch(t *testing.T) {
	d, be := delegatedBranchHarness(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now()) // warm default → wait_cold unmet
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"later","wait_cold":"1h"}`)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (the default parent still defers); body: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "deferred" {
		t.Errorf("status field = %v, want deferred; body: %s", resp["status"], w.Body.String())
	}
	all, _ := store.All()
	if len(all) != 1 {
		t.Fatalf("queued = %d, want 1", len(all))
	}
	if all[0].Kind != defersend.KindBranch || all[0].SessionKey != testSessionKey {
		t.Errorf("record = %+v, want one branch on the default session %q", all[0], testSessionKey)
	}
	if inj := be.injectSnapshot(); len(inj) != 0 {
		t.Errorf("backend injects = %q, want none at enqueue", inj)
	}
}

// TestBranch_DeferredNamedParentStillDefersOnBranchingBackend pins #2301's
// "everything else unchanged": a backend that CAN branch still defers a
// wait-carrying branch of a named parent — the enqueue refusal is only for a
// backend that cannot branch.
func TestBranch_DeferredNamedParentStillDefersOnBranchingBackend(t *testing.T) {
	d, be := delegatedBranchingHarness(t, httpTestOpts{})
	warmNamedParent(t, d) // wait_cold unmet
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"later","session":"side","wait_cold":"1h"}`)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (a branching backend still defers); body: %s", w.Code, w.Body.String())
	}
	all, _ := store.All()
	if len(all) != 1 {
		t.Fatalf("queued = %d, want 1", len(all))
	}
	if all[0].Kind != defersend.KindBranch || all[0].SessionKey != testAgentID+"/iside" {
		t.Errorf("record = %+v, want one branch on the named parent", all[0])
	}
	if inj := be.injectSnapshot(); len(inj) != 0 {
		t.Errorf("backend injects = %q, want none at enqueue", inj)
	}
}

// TestBranch_DeferredNamedParentStillDefersForAPIAgent pins #2301's
// "everything else unchanged": an API agent (no DelegatedManager — the
// cannot-branch predicate does not apply) still defers a wait-carrying
// branch of a named parent with 202.
func TestBranch_DeferredNamedParentStillDefersForAPIAgent(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	warmNamedParent(t, d) // wait_cold unmet
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"later","session":"side","wait_cold":"1h"}`)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (an API agent still defers); body: %s", w.Code, w.Body.String())
	}
	all, _ := store.All()
	if len(all) != 1 {
		t.Fatalf("queued = %d, want 1", len(all))
	}
	if all[0].Kind != defersend.KindBranch || all[0].SessionKey != testAgentID+"/iside" {
		t.Errorf("record = %+v, want one branch on the named parent", all[0])
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s) at enqueue, want 0", len(calls))
	}
}

// TestBranch_NamedParentNoWaitStillRefused characterises the arms of R1 the
// enqueue gate must NOT change: with no wait field at all, or with
// wait_none, a named parent on a cannot-branch backend is refused by
// runBranchTurn's #2284 gate exactly as before — 422, nothing stored,
// nothing runs.
func TestBranch_NamedParentNoWaitStillRefused(t *testing.T) {
	cases := []struct{ name, body string }{
		{"no wait field", `{"text":"q","session":"side"}`},
		// The parent is warm and the wait would defer — wait_none must skip
		// the defer machinery into the same refusal, not around it.
		{"wait_none", `{"text":"q","session":"side","wait_cold":"1h","wait_none":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, be := delegatedBranchHarness(t, httpTestOpts{})
			warmNamedParent(t, d)
			store := withDeferStore(t, &d)
			mux := newTestMux(d)

			w := postJSON(mux, "/branch", tc.body)

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 as before (#2284); body: %s", w.Code, w.Body.String())
			}
			if all, _ := store.All(); len(all) != 0 {
				t.Errorf("defer store holds %d record(s), want 0", len(all))
			}
			if inj := be.injectSnapshot(); len(inj) != 0 {
				t.Errorf("backend injects = %q, want none", inj)
			}
		})
	}
}

// TestBranch_CannotBranchDefaultParentWaitNoneRunsNow characterises R1/R4:
// wait_none on the DEFAULT parent of a cannot-branch backend skips the wait
// gate (and the enqueue refusal — the parent is the default) and runs the
// send fallback NOW: the deferred machinery is not involved.
func TestBranch_CannotBranchDefaultParentWaitNoneRunsNow(t *testing.T) {
	d, be := delegatedBranchHarness(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now()) // warm: the wait_cold would otherwise defer
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"now","wait_cold":"1h","wait_none":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (wait_none runs the default-session fallback now); body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != delegatedReply {
		t.Errorf("response = %q, want the delegated turn's %q", resp["response"], delegatedReply)
	}
	if resp["session"] != testSessionKey || resp["resolved_via"] != "default" {
		t.Errorf("receipt = %q via %q, want the parent %q via default", resp["session"], resp["resolved_via"], testSessionKey)
	}
	if inj := be.injectSnapshot(); len(inj) != 1 {
		t.Errorf("backend injects = %q, want exactly one turn on the parent", inj)
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("defer store holds %d record(s), want 0", len(all))
	}
}
