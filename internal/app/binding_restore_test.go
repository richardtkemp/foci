package app

import (
	"context"
	"reflect"
	"testing"
	"time"

	"foci/internal/app/fap"
	"foci/internal/platform"
)

// TestFrameStore_RestorableConvs proves the restore set is exactly the convs with
// a VISIBLE frame and a known agent_id — invisible-only convs (typing) and legacy
// rows with no agent are excluded, and the result is distinct.
func TestFrameStore_RestorableConvs(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	// c1: visible + agent → restorable (twice, to prove DISTINCT).
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: false})
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 2, wire: "w", sentMs: now, visible: true})
	// c2: visible but NO agent (legacy) → excluded.
	s.insert(frameWrite{convID: "c2", agentID: "", seq: 1, wire: "w", sentMs: now, visible: true})
	// c3: agent but only invisible (typing) → excluded.
	s.insert(frameWrite{convID: "c3", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: false})

	got := s.RestorableConvs()
	if len(got) != 1 || got[0].convID != "c1" || got[0].agentID != "clutch" {
		t.Fatalf("RestorableConvs = %+v, want only {c1, clutch}", got)
	}
}

// TestEnsureBinding_PersistsConvID proves binding creation writes the conv_id
// chat-metadata row — the preimage of the one-way chatID hash — which is what
// makes the conversation durable before its first frame and the default pin
// resolvable without a live binding.
func TestEnsureBinding_PersistsConvID(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}

	h.ensureBinding(nil, "clutch", "conv-x")

	got, err := idx.GetChatMetadata("clutch", "app", chatIDForConv("conv-x"), "conv_id")
	if err != nil || got != "conv-x" {
		t.Fatalf("conv_id row = %q (err %v), want conv-x", got, err)
	}
}

// TestStartAll_RestoresFramelessRegisteredConvs proves the startup restore set
// is the union of the frame store and persisted conv_id rows: a conversation
// created (and maybe starred) but never used has no frames, yet must survive a
// restart — pre-durability it silently vanished at the first restart, leaving
// any default pin dangling.
func TestStartAll_RestoresFramelessRegisteredConvs(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	s := tempFrameStore(t)
	h.frames = s
	now := time.Now().UnixMilli()
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})
	// Registered on a previous run, never used: conv_id row, no frames.
	if err := idx.SetChatMetadata("clutch", "app", chatIDForConv("c2"), "conv_id", "c2"); err != nil {
		t.Fatal(err)
	}

	h.StartAll(context.Background())

	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.convs["c1"] == nil || h.convs["c2"] == nil {
		t.Fatalf("StartAll must restore both the framed and the frameless conv: %v", h.convs)
	}
	if h.convs["c2"].snapshotClient() != nil {
		t.Error("restored frameless binding must be socketless until the app reconnects")
	}
}

// TestDeliverBinding_ResurrectsPinnedDefault proves a session-blind send reaches
// the pinned default conversation even when its binding isn't live (e.g. after a
// restart with no durable frames): the persisted conv_id row reverses the
// one-way chatID hash and the binding is recreated on demand.
func TestDeliverBinding_ResurrectsPinnedDefault(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.ensureBinding(nil, "clutch", "conv-a") // persists the conv_id row
	if err := idx.SetDefaultChat("clutch", "app", chatIDForConv("conv-a")); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart with an empty frame store: fresh hub, same index.
	h2 := newTestHub()
	h2.deps = platform.ProviderDeps{SessionIndex: idx}
	b, via := h2.deliverBinding("clutch")
	if b == nil || b.convID != "conv-a" {
		t.Fatalf("deliverBinding = %+v, want resurrected conv-a", b)
	}
	if via != "default" {
		t.Errorf("via = %q, want default", via)
	}
}

// TestDeliverBinding_UnresolvablePinFallsBack proves a default pin with no
// conv_id row (recorded before conv_id persistence existed) cannot be
// resurrected, so delivery falls back to the most recent conversation instead
// of dropping — and reports the true rung, not "default".
func TestDeliverBinding_UnresolvablePinFallsBack(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	if err := idx.SetDefaultChat("clutch", "app", 12345); err != nil {
		t.Fatal(err)
	}
	live := &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	h.convs["c1"] = live

	b, via := h.deliverBinding("clutch")
	if b != live {
		t.Fatalf("deliverBinding = %+v, want the live most-recent conv c1", b)
	}
	if via != "most-recent" {
		t.Errorf("via = %q, want most-recent", via)
	}
}

// TestStartAll_RestoresBindings proves bindings are rebuilt from the durable store
// at startup (socketless), so bindingForSession resolves before the app reconnects.
func TestStartAll_RestoresBindings(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	s := tempFrameStore(t)
	h.frames = s
	now := time.Now().UnixMilli()
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})
	s.insert(frameWrite{convID: "c2", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})

	h.StartAll(context.Background())

	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.convs["c1"] == nil || h.convs["c2"] == nil {
		t.Fatalf("StartAll did not restore both bindings: %v", h.convs)
	}
	if b := h.convs["c1"]; b.snapshotClient() != nil {
		t.Error("restored binding must be socketless (nil client) until the app reconnects")
	}
}

// TestHandleConversationArchive proves archive sets the is_archived flag
// (reversibly) and leaves the binding + frames intact — it does NOT purge,
// drop the binding, or flip session status. Round-trips through unarchive.
func TestHandleConversationArchive(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	s := tempFrameStore(t)
	h.frames = s
	now := time.Now().UnixMilli()
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})

	b := &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}
	h.convs["c1"] = b
	h.bySession[b.sessionKey] = b

	// Archive: flag set, binding + frames retained.
	h.handleConversationArchive(fakeClient(), fap.ConversationArchive{ConversationID: "c1", Archived: true})
	if !idx.ArchivedChatsForAgent("clutch", "app")[42] {
		t.Error("archive must set the is_archived flag for chatID 42")
	}
	if h.convs["c1"] == nil || h.bySession[b.sessionKey] == nil {
		t.Error("archive must NOT drop the binding (flag-based, not destructive)")
	}
	if len(s.RestorableConvs()) != 1 {
		t.Error("archive must NOT purge frames (history retained for unarchive)")
	}

	// Unarchive: flag cleared, binding + frames still intact.
	h.handleConversationArchive(fakeClient(), fap.ConversationArchive{ConversationID: "c1", Archived: false})
	if idx.ArchivedChatsForAgent("clutch", "app")[42] {
		t.Error("unarchive must clear the is_archived flag")
	}
	if h.convs["c1"] == nil {
		t.Error("unarchive must leave the binding live")
	}

	// Unknown conversation: no panic, no flag written.
	h.handleConversationArchive(fakeClient(), fap.ConversationArchive{ConversationID: "ghost", Archived: true})
	if len(idx.ArchivedChatsForAgent("clutch", "app")) != 0 {
		t.Error("archive of unknown conv must not write a flag")
	}
}

// TestHandleConversationArchive_RefusesDefault proves archiving the agent's
// default chat is refused: the flag is not written, an archive_default
// ErrorFrame is sent, and the roster is re-pushed so the client's optimistic
// archived flag reverts. A non-default chat still archives normally.
func TestHandleConversationArchive_RefusesDefault(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}
	if err := idx.SetDefaultChat("clutch", "app", 42); err != nil {
		t.Fatal(err)
	}

	c := fakeClient()
	h.handleConversationArchive(c, fap.ConversationArchive{ConversationID: "c1", Archived: true})
	if idx.ArchivedChatsForAgent("clutch", "app")[42] {
		t.Error("archiving the default chat must be refused")
	}
	var errCode string
	sawHello := false
	for _, f := range drain(t, c) {
		switch f.t {
		case fap.TypeError:
			errCode, _ = f.d["code"].(string)
		case fap.TypeHello:
			sawHello = true
		}
	}
	if errCode != "archive_default" {
		t.Errorf("error code = %q, want archive_default", errCode)
	}
	if !sawHello {
		t.Error("refusal must re-push the roster to revert the client's optimistic flag")
	}

	// A non-default conversation still archives.
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "clutch", chatID: 43}
	h.handleConversationArchive(c, fap.ConversationArchive{ConversationID: "c2", Archived: true})
	if !idx.ArchivedChatsForAgent("clutch", "app")[43] {
		t.Error("non-default conversation must archive normally")
	}
}

// TestAgentRoster_MarksArchivedConversation proves the roster surfaces Archived
// for chats flagged is_archived on the app platform, and only those — mirroring
// the IsDefault roster test. The roster is the app's source of truth for
// archived state across devices and fresh pairings.
func TestAgentRoster_MarksArchivedConversation(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "ag", chatID: 42}
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "ag", chatID: 99}
	if err := idx.SetArchivedChat("ag", "app", 99, true); err != nil {
		t.Fatal(err)
	}

	roster := h.agentRoster()
	if len(roster) != 1 {
		t.Fatalf("roster = %d agents, want 1", len(roster))
	}
	var archived, total int
	for _, ci := range roster[0].Conversations {
		total++
		if ci.Archived {
			archived++
			if ci.ID != "c2" {
				t.Errorf("Archived set on %q, want c2 (chatID 99)", ci.ID)
			}
		}
	}
	if total != 2 || archived != 1 {
		t.Fatalf("roster convs total=%d archived=%d, want 2/1", total, archived)
	}
}

// TestAgentRoster_CacheExpiryIsLiveNotCached proves the roster's CacheExpiryMs
// is recomputed from the agent (agentCore.CacheExpiryMs) at roster-build time,
// not read off the binding's cached cacheExpiryMs field. That field is a
// push-dedup cache set by setCacheExpiry on a live touch — it is NOT
// rehydrated when ensureBinding rebuilds a binding after a restart (see
// ensureBinding's comment), so before this fix a session idle since before the
// last restart reported a stale/zero CacheExpiryMs (the client reads 0 as
// "warm") until its next touch, even though the session's real cache had long
// expired. This models exactly that: a binding with cacheExpiryMs still at its
// post-restart zero value, backed by an agent whose SessionIndex-derived
// CacheExpiry is real and non-expired.
func TestAgentRoster_CacheExpiryIsLiveNotCached(t *testing.T) {
	h := newTestHub()
	registerFakeAgent(h, "ag") // fakeAgent.CacheExpiryMs reports time.Now()+5m
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "ag", sessionKey: "ag/c1", cacheExpiryMs: 0}

	roster := h.agentRoster()
	if len(roster) != 1 || len(roster[0].Conversations) != 1 {
		t.Fatalf("roster = %+v, want 1 agent with 1 conversation", roster)
	}
	got := roster[0].Conversations[0].CacheExpiryMs
	if got <= time.Now().UnixMilli() {
		t.Fatalf("CacheExpiryMs = %d, want a live future expiry from the agent (not the cached zero field)", got)
	}
}

// TestConvBindingFieldCensus is a "did you forget to rehydrate this" tripwire.
// Go struct literals don't enforce complete field initialization, so a new
// convBinding field silently defaults to its zero value unless someone
// remembers to wire it into ensureBinding's restart-rehydration path — exactly
// how the cacheExpiryMs restart bug slipped through (an idle session kept
// reporting a stale "warm" cache indicator for hours after a gateway
// restart). This enumerates every field via reflection and requires each to be
// classified into exactly one bucket below; an unclassified (or
// double-classified) field fails the test, forcing the "does this need
// restart rehydration?" question to be answered explicitly instead of
// defaulting to "no" by silent omission.
func TestConvBindingFieldCensus(t *testing.T) {
	// constructorSupplied: given a correct value directly in ensureBinding's
	// struct literal (or an equivalent make()/lookup right there) every time a
	// binding is built — nothing to rehydrate because construction always
	// supplies it fresh.
	constructorSupplied := map[string]bool{
		"convID": true, "sessionKey": true, "agentID": true, "chatID": true,
		"replayDepth": true, "replayTTL": true, "store": true,
		"notifyOffline": true, "seq": true, "seen": true,
	}
	// durableBacked: correctness depends on a value from persistent storage
	// (frameStore / SessionIndex) that predates this process — MUST be
	// explicitly rehydrated in ensureBinding when a binding is rebuilt after a
	// restart, or a restored binding reports a wrong/stale value.
	durableBacked := map[string]bool{
		"lastPreview": true, "lastActMs": true, "features": true,
	}
	// restartSafe: legitimately empty/zero immediately after a fresh process —
	// either genuinely-transient runtime state (no live sockets, no turn in
	// flight yet) or, for cacheExpiryMs specifically, a push-dedup cache whose
	// zero value is harmless because the one place its truth matters
	// (agentRoster's snapshot) recomputes live from the agent instead of
	// reading this field (see the cache-expiry comment in agentRoster).
	restartSafe := map[string]bool{
		"mu": true, "clients": true, "clientStates": true, "buffer": true,
		"seenOrder": true, "turnKind": true, "turnDetail": true,
		"subagentDetail": true, "waitingDetail": true, "activityKind": true,
		"activityDetail": true, "cacheExpiryMs": true,
	}

	rt := reflect.TypeOf(convBinding{})
	all := make(map[string]bool, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		all[name] = true
		classified := 0
		for _, bucket := range []map[string]bool{constructorSupplied, durableBacked, restartSafe} {
			if bucket[name] {
				classified++
			}
		}
		switch {
		case classified == 0:
			t.Errorf("convBinding field %q is not classified in TestConvBindingFieldCensus — decide whether it needs restart rehydration in ensureBinding (add to durableBacked) or is safely zero/fresh at construction (add to restartSafe or constructorSupplied), then update this test", name)
		case classified > 1:
			t.Errorf("convBinding field %q is classified in more than one bucket", name)
		}
	}
	// Stale bucket entries (a renamed/removed field) would silently stop
	// testing anything — catch those too.
	for label, bucket := range map[string]map[string]bool{
		"constructorSupplied": constructorSupplied,
		"durableBacked":       durableBacked,
		"restartSafe":         restartSafe,
	} {
		for name := range bucket {
			if !all[name] {
				t.Errorf("%s lists %q, which is not a convBinding field anymore", label, name)
			}
		}
	}
}
