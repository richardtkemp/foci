package slack

// nodefault_test.go: requirement 6 — a Slack conversation never becomes an
// agent's default chat or default session, however recently it was active,
// and agent-wide traffic never posts into it.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/session"
)

// TestSlackNeverBecomesDefaultChat proves the never-default rule end to end:
// after DM and thread contact, no default chat exists on slack, default
// session resolution skips the Slack sessions (even though they are the most
// recently user-active roots), and route resolution for a bare agent target
// finds nothing via Slack.
func TestSlackNeverBecomesDefaultChat(t *testing.T) {
	h := newHarness(t, "helen", map[string]string{"C9": "helen"})
	h.allow("helen", true, "U1")

	// Telegram chat 555: older activity, so the Slack sessions are the most
	// recently active ones.
	idx := h.idx
	now := time.Now().UTC().Truncate(time.Second)
	idx.Upsert(session.SessionIndexEntry{SessionKey: "helen/c555", SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
		CreatedAt: now.Add(-48 * time.Hour), LastActivityAt: now.Add(-24 * time.Hour)})
	idx.TouchUserActivity("helen/c555", now.Add(-24*time.Hour))
	if err := idx.SetChatMetadata("helen", "telegram", 555, "registered", "true"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetDefaultChat("helen", "telegram", 555); err != nil {
		t.Fatal(err)
	}

	// Slack contact: a DM (older) and a thread (most recent of all), through
	// the real receive path so their chat_metadata rows exist; the session
	// rows are upserted as the agent's inbox would.
	h.mgr.botUserID.Store("UBOTID")
	h.deliver(dmEvent("U1", "D1", "hi", "1"))
	threadTarget := Target{Kind: TargetThread, Channel: "C9", ThreadTS: "1"}
	h.deliver(mentionEvent("U1", "C9", "<@UBOTID> work", "1", ""))
	threadKey := sessionKeyFor("helen", threadTarget)
	dmKey := sessionKeyFor("helen", Target{Kind: TargetDM, Channel: "D1", User: "U1"})
	idx.Upsert(session.SessionIndexEntry{SessionKey: dmKey, SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
		CreatedAt: now.Add(-20 * time.Hour), LastActivityAt: now.Add(-2 * time.Hour)})
	idx.Upsert(session.SessionIndexEntry{SessionKey: threadKey, SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
		CreatedAt: now.Add(-time.Hour), LastActivityAt: now})
	idx.TouchUserActivity(dmKey, now.Add(-2*time.Hour))
	idx.TouchUserActivity(threadKey, now)

	if got := idx.DefaultChatForAgent("helen", platformName); got != 0 {
		t.Errorf("DefaultChatForAgent(slack) = %d, want 0 (Slack never becomes a default chat)", got)
	}
	if got := idx.DefaultSessionKeyForAgentOn("helen", ""); got != "helen/c555" {
		t.Errorf("DefaultSessionKeyForAgentOn = %q, want the telegram chat (rung 4 must skip the newer Slack sessions)", got)
	}
	if got := idx.DefaultSessionKeyForAgentOn("helen", platformName); got != "helen/c555" {
		t.Errorf("DefaultSessionKeyForAgentOn(preferred slack) = %q, want the telegram fallback (Slack is never preferred)", got)
	}

	// route.Resolver with a bare agent target resolves through the default
	// session — the telegram chat, never the Slack conversations.
	res := route.Resolver{Index: idx}
	rt, err := res.ParseTarget("helen")
	if err != nil {
		t.Fatal(err)
	}
	got, err := res.Resolve(rt)
	if err != nil {
		t.Fatalf("resolve bare agent target: %v", err)
	}
	if got.SessionKey != "helen/c555" {
		t.Errorf("bare agent target resolved to %q, want the telegram default", got.SessionKey)
	}
}

// TestSlackOnlyAgentNeverGainsDefaultSession proves the no-default case with
// no other platform to fall back to (the gap TestSlackNeverBecomesDefaultChat
// leaves open: there a telegram default exists, so rung 4's filter is never
// the last line). A Slack-only agent whose Slack root is the most recently
// active session has NO default session at all — rung 4 skips it — and a
// bare-agent route target resolves to nothing.
func TestSlackOnlyAgentNeverGainsDefaultSession(t *testing.T) {
	h := newHarness(t, "helen", map[string]string{"C9": "helen"})
	h.allow("helen", true, "U1")

	// Slack contact through the real receive path, and session rows upserted
	// as the agent's inbox would — the Slack roots are the agent's ONLY
	// sessions, the thread the most recently active of all.
	h.mgr.botUserID.Store("UBOTID")
	h.deliver(dmEvent("U1", "D1", "hi", "1"))
	h.deliver(mentionEvent("U1", "C9", "<@UBOTID> work", "1", ""))
	threadKey := sessionKeyFor("helen", Target{Kind: TargetThread, Channel: "C9", ThreadTS: "1"})
	dmKey := sessionKeyFor("helen", Target{Kind: TargetDM, Channel: "D1", User: "U1"})
	idx := h.idx
	now := time.Now().UTC().Truncate(time.Second)
	idx.Upsert(session.SessionIndexEntry{SessionKey: dmKey, SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
		CreatedAt: now.Add(-20 * time.Hour), LastActivityAt: now.Add(-2 * time.Hour)})
	idx.Upsert(session.SessionIndexEntry{SessionKey: threadKey, SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
		CreatedAt: now.Add(-time.Hour), LastActivityAt: now})
	idx.TouchUserActivity(dmKey, now.Add(-2*time.Hour))
	idx.TouchUserActivity(threadKey, now)

	if got := idx.DefaultChatForAgent("helen", platformName); got != 0 {
		t.Errorf("DefaultChatForAgent(slack) = %d, want 0", got)
	}
	if got := idx.DefaultSessionKeyForAgentOn("helen", ""); got != "" {
		t.Errorf("DefaultSessionKeyForAgentOn = %q, want \"\" (the most recent root is a Slack session; rung 4 must skip it, leaving nothing)", got)
	}
	if got := idx.DefaultSessionKeyForAgentOn("helen", platformName); got != "" {
		t.Errorf("DefaultSessionKeyForAgentOn(preferred slack) = %q, want \"\" (Slack is never preferred and never the fallback)", got)
	}

	// A bare-agent route target finds nothing: there is no default session
	// to resolve to, and the Slack conversations are not it.
	res := route.Resolver{Index: idx}
	rt, err := res.ParseTarget("helen")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := res.Resolve(rt); err == nil {
		t.Errorf("bare agent target resolved to %q; want no resolution (no default session exists)", got.SessionKey)
	} else if !errors.Is(err, route.ErrNoSession) {
		t.Errorf("bare agent target error = %v, want ErrNoSession", err)
	}
}

// TestAgentWideSendsNeverPostToSlack proves the agent-wide send shapes —
// Broadcast over AllForAgent, the Primary fallback, and a startup-notice
// shape — post NOTHING into Slack when a Slack conversation is the agent's
// most recently active one. The drop is logged: the unbound connection's
// session-blind sends leave a WARN naming what was dropped (req 6's "dropped
// and logged").
func TestAgentWideSendsNeverPostToSlack(t *testing.T) {
	f := newRoutingFixture(t)
	cm := platform.NewConnectionManagerAdapter[*Conn](f.h.mgr)

	// route.Broadcast skips the unbound conn (it declares the agent-wide
	// drop), so a broadcast notice reaches nothing here — it cannot even
	// attempt a Slack post. Drive it with a SendNotification anyway: the
	// loop body must stay empty.
	for _, conn := range route.Broadcast(cm, "helen") {
		conn.SendNotification("⚡ agent-wide notice")
	}

	// The Primary fallback shapes: SendText / SendTextWithButtons /
	// SendDocument / SetTyping on the unbound connection Slack contributes.
	logs := captureLogLines(t, log.WARN)
	primary := cm.Primary("helen")
	if primary == nil {
		t.Fatal("no primary connection")
	}
	if primary.PlatformName() != platformName {
		t.Fatalf("primary = %s; with slack alone it is the slack unbound conn", primary.PlatformName())
	}
	_ = primary.SendText("startup: restarted")
	if bs, ok := primary.(platform.ButtonSender); ok {
		_, _ = bs.SendTextWithButtons("approve?", []platform.ButtonChoice{{Label: "Allow", Data: "allow"}}, "im:")
	}
	_ = primary.SendDocument("/nonexistent", "")
	primary.SetTyping(true)

	// Nothing posted at all: every agent-wide send dropped on the unbound
	// connection (the harness sent no warm message here).
	if posts := f.h.fake.postCalls(); len(posts) != 0 {
		t.Fatalf("posts = %+v; agent-wide sends must not reach Slack", posts)
	}
	if calls := f.h.fake.totalCalls(); calls != 0 {
		t.Errorf("agent-wide sends made %d API calls total, want 0", calls)
	}
	// The drop is visible in the log: one WARN per session-blind send that
	// reached the unbound conn (SendText, the button prompt; SendDocument
	// fails on the missing file only after the target lookup drops it).
	dropped := 0
	for _, line := range logs() {
		if strings.Contains(line, "dropped") && strings.Contains(line, "no session to address") {
			dropped++
		}
	}
	if dropped == 0 {
		t.Error("no WARN logged for the dropped agent-wide sends; req 6 drops must be logged, not silent")
	}
}
