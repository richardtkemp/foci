package slack

import (
	"context"
	"sync"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
)

// testLoggerFn is the shared lazy logger for store-level tests.
var testLoggerFn = func() *log.ComponentLogger { return log.NewComponentLogger("slack-test") }

// TestSessionKeys pins the deterministic session mapping (#2459 req 4): one
// session per DM user, one per channel thread, distinct across users,
// threads, channels and agents, and keyed by the agent so two agents on two
// channels never share.
func TestSessionKeys(t *testing.T) {
	dm1 := Target{Kind: TargetDM, Channel: "D100", User: "U1"}
	dm2 := Target{Kind: TargetDM, Channel: "D101", User: "U2"}
	thread1 := Target{Kind: TargetThread, Channel: "C200", ThreadTS: "1700000000.000200"}
	thread1Later := Target{Kind: TargetThread, Channel: "C200", ThreadTS: "1700000000.000200"} // same thread, later mention
	thread2 := Target{Kind: TargetThread, Channel: "C200", ThreadTS: "1700000000.000900"}
	channel2 := Target{Kind: TargetThread, Channel: "C201", ThreadTS: "1700000000.000200"}

	sk := func(agent string, t Target) string { return sessionKeyFor(agent, t) }

	if sk("helen", dm1) != sk("helen", dm1) || sk("helen", thread1) != sk("helen", thread1Later) {
		t.Error("the same DM / the same thread must always derive the same key")
	}
	if sk("helen", dm1) == sk("helen", dm2) {
		t.Error("two users' DMs share a session")
	}
	if sk("helen", thread1) == sk("helen", thread2) {
		t.Error("two threads in one channel share a session")
	}
	if sk("helen", thread1) == sk("helen", channel2) {
		t.Error("the same ts in two channels shares a session")
	}
	if sk("helen", dm1) == sk("helen", thread1) {
		t.Error("a DM and a thread share a session")
	}
	if sk("helen", thread1) == sk("bob", thread1) {
		t.Error("two agents on the same conversation share a session")
	}
	// Two agents on two channels: keys live under each agent.
	if sk("helen", channel2) == sk("bob", thread1) {
		t.Error("agent a's channel-2 session collides with agent b's channel-1 session")
	}
	if got := session.AgentIDFromKey(sk("helen", dm1)); got != "helen" {
		t.Errorf("key %q carries agent %q", sk("helen", dm1), got)
	}
	// The turn text of a channel mention is the mention message only.
	msg, reason := classifyEvent(mentionEvent("U1", "C200", "<@UBOTID> only this", "1700000000.000200", ""), "UBOTID")
	if reason != "" || msg.Text != "only this" {
		t.Errorf("mention turn text = %q (reason %s); want the mention message minus the bot mention", msg.Text, reason)
	}
}

// TestSessionKeys_SurviveRestart proves the mapping and outbound routing
// survive a foci restart: a second manager over the SAME session index
// derives the same keys — from the persisted targets AND from a replayed
// inbound mention — and a send to the session lands in the right
// conversation with no inbound message at all.
func TestSessionKeys_SurviveRestart(t *testing.T) {
	h := newHarness(t, "helen", map[string]string{"C200": "helen"})
	h.allow("helen", true, "U1")

	dm := dmEvent("U1", "D100", "hello", "1")
	h.deliver(dm)
	thread := mentionEvent("U1", "C200", "<@UBOTID> fix", "1700000000.000200", "")
	h.deliver(thread)
	envs := h.waitEnvelopes(h.mgr.core("helen"), 2)
	dmKey, threadKey := envs[0].SessionKey, envs[1].SessionKey

	// Restart: a fresh manager over the same index (the old one goes away).
	h.build()
	h.allow("helen", true, "U1")

	if got := h.mgr.BotForSession(dmKey); got == nil {
		t.Fatalf("session %s unresolvable after restart", dmKey)
	}
	if got := sessionKeyFor("helen", Target{Kind: TargetDM, Channel: "D100", User: "U1"}); got != dmKey {
		t.Errorf("dm key after restart = %q, want %q", got, dmKey)
	}
	if got := sessionKeyFor("helen", Target{Kind: TargetThread, Channel: "C200", ThreadTS: "1700000000.000200"}); got != threadKey {
		t.Errorf("thread key after restart = %q, want %q", got, threadKey)
	}

	// The inbound path itself must still land in the same session: a later
	// mention INSIDE the thread (Slack sets thread_ts to the parent) routes
	// to the pre-restart key, not a fresh one.
	h.deliver(mentionEvent("U1", "C200", "<@UBOTID> still there?", "1700000000.000300", "1700000000.000200"))
	envs = h.waitEnvelopes(h.mgr.core("helen"), 1)
	if envs[0].SessionKey != threadKey {
		t.Errorf("in-thread mention after restart keyed %q, want the pre-restart thread session %q", envs[0].SessionKey, threadKey)
	}
	if envs[0].Text != "still there?" {
		t.Errorf("in-thread mention turn text = %q, want the mention text", envs[0].Text)
	}

	// Agent-initiated send with NO inbound: routed by the persisted target.
	conn := h.mgr.BotForSession(threadKey)
	if conn == nil {
		t.Fatal("no bound connection for the thread session after restart")
	}
	if err := conn.SendToSession(threadKey, "wake up"); err != nil {
		t.Fatal(err)
	}
	posts := h.fake.postsTo("C200")
	if len(posts) != 1 {
		t.Fatalf("chat.postMessage calls to C200 = %d, want 1 (the wake send)", len(posts))
	}
	if posts[0].Body["thread_ts"] != "1700000000.000200" {
		t.Errorf("wake send thread_ts = %v, want the original thread", posts[0].Body["thread_ts"])
	}
}

// orderingIndex records the sequence of metadata writes (and nothing else)
// so a test can prove the slack_target write happens BEFORE Enqueue.
type orderingIndex struct {
	platform.SessionIndex
	mu     sync.Mutex
	events []string // "set:<key>" per SetChatMetadata call
}

func (o *orderingIndex) SetChatMetadata(agentID, platformName string, chatID int64, key, value string) error {
	o.mu.Lock()
	o.events = append(o.events, "set:"+key)
	o.mu.Unlock()
	return o.SessionIndex.SetChatMetadata(agentID, platformName, chatID, key, value)
}

func (o *orderingIndex) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

// TestTargetStore_RegisterPersistsAndWritesFlags proves registration writes
// the never_default, registered and slack_target rows — flag FIRST, route
// LAST — and that every metadata write happens BEFORE the envelope is
// enqueued (a turn whose replies cannot be routed, or whose conversation
// could not be marked never-default, must never start).
func TestTargetStore_RegisterPersistsAndWritesFlags(t *testing.T) {
	rec := &orderingIndex{SessionIndex: newTestIndex(t)}
	store := newTargetStore(rec, testLoggerFn)

	target := Target{Kind: TargetThread, Channel: "C1", ThreadTS: "9"}
	chatID, err := store.register("helen", target)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ key, want string }{
		{chatMetaKeySlackTarget, `{"kind":"thread","channel":"C1","thread_ts":"9"}`},
		{"registered", "true"},
		{session.ChatMetaNeverDefault, "true"},
	} {
		if got, err := rec.GetChatMetadata("helen", platformName, chatID, row.key); err != nil || got != row.want {
			t.Errorf("row %s = %q (err %v), want %q", row.key, got, err, row.want)
		}
	}
	// Write order: the never-default flag precedes the route rows, so a
	// partially-written registration can never leave a live route on an
	// unmarked (default-eligible) chat.
	events := rec.snapshot()
	if len(events) != 3 || events[0] != "set:"+session.ChatMetaNeverDefault {
		t.Errorf("write order = %v, want the never_default flag written first", events)
	}

	// Write-before-enqueue, end to end through the receive path: a fake
	// agent records its Enqueue after the metadata writes it depends on.
	recMu := sync.Mutex{}
	var order []string
	fa := &enqueueRecorder{onEnqueue: func() {
		recMu.Lock()
		order = append(order, "enqueue")
		recMu.Unlock()
	}}
	h := &harness{t: t, fake: newFakeSlack(t), idx: nil}
	h.mgr = newManager(&config.SlackSpecific{DefaultAgent: "helen", APIBase: h.fake.apiBase()}, nil, nil, rec, func(s string) string { return s })
	h.mgr.addCore(&agentCore{
		agentID: "helen", api: nil, targets: h.mgr.targets, enq: fa,
		access:    config.AllowedAccess{Users: []string{"U1"}, Only: true},
		accessMap: map[string]bool{"U1": true},
		logger:    testLoggerFn,
	})
	h.deliver(dmEvent("U1", "D1", "hi", "1"))
	recMu.Lock()
	defer recMu.Unlock()
	got := append(rec.snapshot(), order...)
	for i, ev := range got {
		if ev == "enqueue" {
			remaining := got[i+1:]
			if len(remaining) > 0 {
				t.Errorf("metadata writes after Enqueue: %v", remaining)
			}
			break
		}
	}
	if len(got) < 4 || got[0] != "set:"+session.ChatMetaNeverDefault || got[len(got)-1] != "enqueue" {
		t.Errorf("call order = %v, want every metadata write (flag first) strictly before Enqueue", got)
	}
}

// chatMetaRow returns the stored value of one chat_metadata row, "" when
// absent (GetChatMetadata's convention for a missing row).
func chatMetaRow(t *testing.T, idx *session.SessionIndex, agent, platformName string, chatID int64, key string) string {
	t.Helper()
	v, err := idx.GetChatMetadata(agent, platformName, chatID, key)
	if err != nil {
		t.Fatalf("read chat metadata %s: %v", key, err)
	}
	return v
}

// enqueueRecorder records its Enqueue position through onEnqueue.
type enqueueRecorder struct{ onEnqueue func() }

func (a *enqueueRecorder) Enqueue(agent.Envelope) bool {
	a.onEnqueue()
	return true
}
func (a *enqueueRecorder) SetInboxSteerMode(bool)     {}
func (a *enqueueRecorder) StartInbox(context.Context) {}

// TestTargetStore_CollisionRefused proves a stored target whose identity
// disagrees with a newly derived one refuses the message instead of silently
// merging two conversations into one session.
func TestTargetStore_CollisionRefused(t *testing.T) {
	idx := newTestIndex(t)
	store := newTargetStore(idx, testLoggerFn)
	first := Target{Kind: TargetThread, Channel: "C1", ThreadTS: "1"}
	if _, err := store.register("helen", first); err != nil {
		t.Fatal(err)
	}
	// Corrupt the stored row so the same chat id claims a DIFFERENT
	// identity, then re-register the original: derived identity ≠ stored
	// identity → collision.
	chatID := first.ChatID()
	if err := idx.SetChatMetadata("helen", platformName, chatID, chatMetaKeySlackTarget, `{"kind":"thread","channel":"C1","thread_ts":"DIFFERENT"}`); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.cache = map[string]Target{}
	store.mu.Unlock()

	if _, err := store.register("helen", first); err == nil {
		t.Error("collision accepted; want errChatCollision")
	}
}

// TestTargetStore_NilIndexCachesForTheProcess proves a store with no session
// index keeps routing stable within the process: register caches the target
// (the only place a nil-index lookup can hit), so the session's sends still
// resolve and land in the right conversation until the process restarts.
func TestTargetStore_NilIndexCachesForTheProcess(t *testing.T) {
	store := newTargetStore(nil, testLoggerFn)
	target := Target{Kind: TargetThread, Channel: "C1", ThreadTS: "7"}

	chatID, err := store.register("helen", target)
	if err != nil {
		t.Fatalf("nil-index register: %v", err)
	}
	got, ok := store.lookup("helen", chatID)
	if !ok || got != target {
		t.Errorf("lookup after register = %v, %v; want %v, true (cached for this process)", got, ok, target)
	}
	if _, ok := store.lookup("helen", chatID+1); ok {
		t.Error("an unknown chat resolved on a nil-index store")
	}
}

// TestTargetStore_LookupHasNoSideEffect proves lookup is a pure read: it
// creates no rows and no cache entries for unknown chats — a second lookup
// also misses (a polluted cache would answer the empty target, ok=true).
func TestTargetStore_LookupHasNoSideEffect(t *testing.T) {
	idx := newTestIndex(t)
	store := newTargetStore(idx, testLoggerFn)
	if _, ok := store.lookup("helen", 12345); ok {
		t.Error("unknown chat resolved; want miss")
	}
	if got, ok := store.lookup("helen", 12345); ok {
		t.Errorf("second lookup resolved %v; the first miss must not have cached an entry", got)
	}
	if got := chatMetaRow(t, idx, "helen", platformName, 12345, chatMetaKeySlackTarget); got != "" {
		t.Errorf("lookup wrote a slack_target row (%q); reads must not persist", got)
	}
	if got := chatMetaRow(t, idx, "helen", platformName, 12345, "registered"); got != "" {
		t.Errorf("lookup wrote a registered row (%q); reads must not persist", got)
	}
}

// TestSessionKeyForChat_IsPure pins Conn.SessionKeyForChat as a derivation
// only: reading it registers nothing.
func TestSessionKeyForChat_IsPure(t *testing.T) {
	h := newHarness(t, "helen", nil)
	conn := h.mgr.PrimaryBot("helen")
	key := conn.SessionKeyForChat(777)
	if key != "helen/c777" {
		t.Errorf("SessionKeyForChat(777) = %q, want helen/c777", key)
	}
	if got := chatMetaRow(t, h.idx, "helen", platformName, 777, "registered"); got != "" {
		t.Errorf("SessionKeyForChat persisted a row (%q); it must be a pure derivation", got)
	}
}

func newTestIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(t.TempDir() + "/idx.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}
