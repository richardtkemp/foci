package slack

// receive_test.go: the inbound routing rules (#2459 req 2, 3, 7) — agent
// selection, the unmapped-channel reply, the DM default, the interim
// allowlist, and command-looking text as ordinary messages. Every negative
// test runs beside a positive control (a plain allowed DM starts exactly one
// turn) so a dead harness cannot pass.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
)

// errForcedFailure is failingKeyIndex's injected write error.
var errForcedFailure = errors.New("forced write failure")

// controlDM proves the harness is alive: an allowed DM to the default agent
// starts exactly one turn.
func controlDM(t *testing.T, h *harness) *agent.Envelope {
	t.Helper()
	h.deliver(dmEvent("U1", "D100", "control", "1"))
	envs := h.waitEnvelopes(h.mgr.core("helen"), 1)
	if len(envs) != 1 {
		t.Fatalf("control dm started %d turns, want 1", len(envs))
	}
	return &envs[0]
}

// TestRouting_DMGoesToDefaultAgent proves a DM lands on the configured
// default agent with the DM session key, the sender's id and text verbatim,
// and no group-chat flag.
func TestRouting_DMGoesToDefaultAgent(t *testing.T) {
	h := newHarness(t, "helen", nil)
	h.allow("helen", true, "U1")

	h.deliver(dmEvent("U1", "D100", "hello", "1"))
	envs := h.waitEnvelopes(h.mgr.core("helen"), 1)
	if envs[0].SessionKey != sessionKeyFor("helen", Target{Kind: TargetDM, Channel: "D100", User: "U1"}) {
		t.Errorf("dm session key = %q", envs[0].SessionKey)
	}
	if envs[0].UserID != "U1" || envs[0].Text != "hello" {
		t.Errorf("dm envelope = %+v", envs[0])
	}
	if envs[0].IsGroupChat {
		t.Error("dm flagged as group chat")
	}
}

// TestRouting_MappedChannelMentionGoesToItsAgent proves a mention in a
// mapped channel reaches that channel's agent, flagged as a group chat,
// with the mention text (minus the bot mention) and a thread session rooted
// at the mention's own ts.
func TestRouting_MappedChannelMentionGoesToItsAgent(t *testing.T) {
	h := newHarness(t, "", map[string]string{"C200": "helen"})
	h.allow("helen", true, "U1")

	h.deliver(mentionEvent("U1", "C200", "<@UBOTID> fix this", "1700000000.000200", ""))
	envs := h.waitEnvelopes(h.mgr.core("helen"), 1)
	if envs[0].Text != "fix this" {
		t.Errorf("turn text = %q, want the mention text", envs[0].Text)
	}
	if !envs[0].IsGroupChat {
		t.Error("channel mention not flagged as group chat")
	}
	wantKey := sessionKeyFor("helen", Target{Kind: TargetThread, Channel: "C200", ThreadTS: "1700000000.000200"})
	if envs[0].SessionKey != wantKey {
		t.Errorf("session key = %q, want %q (the new thread rooted at the mention)", envs[0].SessionKey, wantKey)
	}
}

// TestRouting_TwoAgentsTwoChannels proves the channel→agent mapping keeps
// two agents' traffic apart: each mention reaches its own agent under its
// own session key, never the other's.
func TestRouting_TwoAgentsTwoChannels(t *testing.T) {
	h := newHarness(t, "helen", map[string]string{"C200": "helen", "C201": "bob"})
	h.allow("helen", true, "U1")
	h.allow("bob", true, "U1")

	h.deliver(mentionEvent("U1", "C200", "<@UBOTID> for helen", "1", ""))
	h.deliver(mentionEvent("U1", "C201", "<@UBOTID> for bob", "1", ""))
	helenEnvs := h.waitEnvelopes(h.mgr.core("helen"), 1)
	bobEnvs := h.waitEnvelopes(h.mgr.core("bob"), 1)

	if helenEnvs[0].Text != "for helen" || bobEnvs[0].Text != "for bob" {
		t.Errorf("cross-routed: helen got %q, bob got %q", helenEnvs[0].Text, bobEnvs[0].Text)
	}
	if helenEnvs[0].SessionKey == bobEnvs[0].SessionKey {
		t.Error("two agents share a session key")
	}
	if session.AgentIDFromKey(helenEnvs[0].SessionKey) != "helen" || session.AgentIDFromKey(bobEnvs[0].SessionKey) != "bob" {
		t.Error("envelopes carry the wrong agent")
	}
}

// TestRouting_UnmappedChannelMentionRepliesOnceInThread proves the "not set
// up here" rule: one reply in the mention's thread, no turn, no target row —
// and (review note: the reply is per thread, not per mention) a second
// mention in the same thread within the hold stays quiet.
func TestRouting_UnmappedChannelMentionRepliesOnceInThread(t *testing.T) {
	h := newHarness(t, "", map[string]string{"C200": "helen"})
	h.allow("helen", true, "U1")

	h.deliver(mentionEvent("U1", "C999", "<@UBOTID> anyone?", "1700000000.000999", ""))
	h.deliver(mentionEvent("U1", "C999", "<@UBOTID> again", "1700000000.001000", "1700000000.000999"))

	posts := h.fake.postsTo("C999")
	if len(posts) != 1 {
		t.Fatalf("posts to the unmapped channel = %d, want 1", len(posts))
	}
	if posts[0].Body["thread_ts"] != "1700000000.000999" {
		t.Errorf("reply thread_ts = %v, want the mention's thread", posts[0].Body["thread_ts"])
	}
	if text, _ := posts[0].Body["text"].(string); !strings.Contains(text, "isn't set up") {
		t.Errorf("reply text = %q, want the not-set-up notice", text)
	}
	if body := posts[0].Body; body["reply_broadcast"] != nil {
		t.Error("unmapped reply broadcast to the channel")
	}
	// No turn, no state: the unmapped chat has no target row.
	if envs := h.mgr.core("helen").enq.(*fakeAgent).drained(); len(envs) != 0 {
		t.Errorf("unmapped mention started %d turns", len(envs))
	}
	chatID := Target{Kind: TargetThread, Channel: "C999", ThreadTS: "1700000000.000999"}.ChatID()
	if got := chatMetaRow(t, h.idx, "helen", platformName, chatID, chatMetaKeySlackTarget); got != "" {
		t.Errorf("unmapped mention registered a target row (%q)", got)
	}

	// Positive control: a mapped channel still routes.
	h.deliver(mentionEvent("U1", "C200", "<@UBOTID> real one", "1", ""))
	h.waitEnvelopes(h.mgr.core("helen"), 1)
}

// TestRouting_TrailingMentionStillRoutes proves mention position does not
// decide routing: a mention at the END of the message ("cc the bot") starts
// a turn with the words before it in a mapped channel, and gets the same
// one-per-thread "not set up here" reply in an unmapped one (#2459 req 2, 3
// — review F1: the text before the bot's mention is the user's message).
func TestRouting_TrailingMentionStillRoutes(t *testing.T) {
	t.Run("mapped channel starts a turn with the pre-mention text", func(t *testing.T) {
		h := newHarness(t, "", map[string]string{"C200": "helen"})
		h.allow("helen", true, "U1")

		h.deliver(mentionEvent("U1", "C200", "Can you check this <@UBOTID>", "1700000000.000200", ""))
		envs := h.waitEnvelopes(h.mgr.core("helen"), 1)
		if envs[0].Text != "Can you check this" {
			t.Errorf("turn text = %q, want %q", envs[0].Text, "Can you check this")
		}
		wantKey := sessionKeyFor("helen", Target{Kind: TargetThread, Channel: "C200", ThreadTS: "1700000000.000200"})
		if envs[0].SessionKey != wantKey {
			t.Errorf("session key = %q, want %q", envs[0].SessionKey, wantKey)
		}
	})

	t.Run("unmapped channel gets the not-set-up reply", func(t *testing.T) {
		h := newHarness(t, "", map[string]string{"C200": "helen"})
		h.allow("helen", true, "U1")

		h.deliver(mentionEvent("U1", "C999", "anyone alive <@UBOTID>", "1700000000.000999", ""))
		posts := h.fake.postsTo("C999")
		if len(posts) != 1 {
			t.Fatalf("posts to the unmapped channel = %d, want 1", len(posts))
		}
		if posts[0].Body["thread_ts"] != "1700000000.000999" {
			t.Errorf("reply thread_ts = %v, want the mention's thread", posts[0].Body["thread_ts"])
		}
		if text, _ := posts[0].Body["text"].(string); !strings.Contains(text, "isn't set up") {
			t.Errorf("reply text = %q, want the not-set-up notice", text)
		}
		if envs := h.mgr.core("helen").enq.(*fakeAgent).drained(); len(envs) != 0 {
			t.Errorf("unmapped trailing mention started %d turns", len(envs))
		}
	})
}

// TestRouting_NoDefaultAgentDMStartsNoTurn proves a DM with no default agent
// starts nothing and stays silent (no reply: there is no agent to answer):
// the router itself answers ok=false, and the dropped DM is logged at INFO
// (no default configured) — never as an error, which would claim a routing
// failure where the config simply maps no agent.
func TestRouting_NoDefaultAgentDMStartsNoTurn(t *testing.T) {
	h := newHarness(t, "", map[string]string{"C200": "helen"})
	h.allow("helen", true, "U1")

	if agentID, ok := newRouter(h.cfg).forDM(); ok || agentID != "" {
		t.Errorf("forDM with no default_agent = (%q, %v), want (\"\", false)", agentID, ok)
	}

	logs := captureLogLines(t, log.INFO)
	h.deliver(dmEvent("U1", "D100", "hello?", "1"))
	ignored := 0
	for _, line := range logs() {
		if strings.Contains(line, "no default_agent") {
			ignored++
		}
		if strings.Contains(line, "no Slack connection") {
			t.Errorf("DM without default_agent logged a routing failure: %s", line)
		}
	}
	if ignored != 1 {
		t.Errorf("dm without default_agent logged %d no-default INFO lines, want 1:\n%s", ignored, strings.Join(logs(), "\n"))
	}
	if envs := h.mgr.core("helen").enq.(*fakeAgent).drained(); len(envs) != 0 {
		t.Errorf("dm without default_agent started %d turns", len(envs))
	}
	if calls := h.fake.totalCalls(); calls != 0 {
		t.Errorf("dm without default_agent made %d Web API calls, want 0", calls)
	}
}

// TestRouting_RefusedEnqueueIsLoggedNotSilent proves a message the agent's
// inbox refuses (the queue-full shape) is dropped VISIBLY: a WARN naming the
// agent, the user and the session reaches the log, and the sender is told in the
// same conversation (#2847), as app, Telegram and Discord do (#2762).
func TestRouting_RefusedEnqueueTellsTheSender(t *testing.T) {
	cases := []struct {
		name       string
		ev         slackEvent
		channel    string
		wantThread string
		channels   map[string]string
	}{
		{name: "DM", ev: dmEvent("U1", "D100", "hello?", "1"), channel: "D100"},
		{
			name:       "channel thread",
			ev:         mentionEvent("U1", "C100", "<@UBOTID> hello?", "2", "1"),
			channel:    "C100",
			wantThread: "1",
			channels:   map[string]string{"C100": "helen"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "helen", tc.channels)
			h.allow("helen", true, "U1")
			h.mgr.core("helen").enq.(*fakeAgent).refuseEnqueue = true

			logs := captureLogLines(t, log.WARN)
			h.deliver(tc.ev)

			dropped := 0
			for _, line := range logs() {
				if strings.Contains(line, "refused the enqueue") && strings.Contains(line, "helen") && strings.Contains(line, "U1") {
					dropped++
				}
			}
			if dropped != 1 {
				t.Errorf("refused enqueue logged %d matching WARNs, want 1:\n%s", dropped, strings.Join(logs(), "\n"))
			}
			posts := h.fake.postsTo(tc.channel)
			if len(posts) != 1 {
				t.Fatalf("refused enqueue posted %d messages to %s, want 1", len(posts), tc.channel)
			}
			if got := posts[0].Body["text"]; got != agent.InboxRefusedNotice {
				t.Errorf("notice text = %q, want %q", got, agent.InboxRefusedNotice)
			}
			if got, _ := posts[0].Body["thread_ts"].(string); got != tc.wantThread {
				t.Errorf("notice thread_ts = %q, want %q", got, tc.wantThread)
			}
		})
	}
}

// failingKeyIndex wraps a real index and fails the SetChatMetadata keys in
// fail, so a test can drive a registration write failure end to end.
type failingKeyIndex struct {
	*session.SessionIndex
	fail map[string]bool
}

func (f *failingKeyIndex) SetChatMetadata(agentID, platformName string, chatID int64, key, value string) error {
	if f.fail[key] {
		return errForcedFailure
	}
	return f.SessionIndex.SetChatMetadata(agentID, platformName, chatID, key, value)
}

// newFailingManager builds a manager over an index whose SetChatMetadata
// fails the keys in fail — the shared setup of the registration-failure
// subtests.
func newFailingManager(t *testing.T, fail map[string]bool) (*harness, *failingKeyIndex, *session.SessionIndex) {
	t.Helper()
	idx := newTestIndex(t)
	fake := &failingKeyIndex{SessionIndex: idx, fail: fail}
	h := &harness{t: t, fake: newFakeSlack(t), idx: idx}
	h.cfg = &config.SlackSpecific{DefaultAgent: "helen", APIBase: h.fake.apiBase()}
	h.mgr = newManager(h.cfg, nil, nil, fake, func(s string) string { return s })
	h.mgr.botUserID.Store("UBOTID")
	h.allow("helen", true, "U1")
	return h, fake, idx
}

// TestRouting_RegistrationWriteFailureRefusesMessage proves registration
// failures fail closed end to end (#2459 req 6): whichever write fails, the
// message starts NO turn — nothing is enqueued — and NO route is left
// behind (BotForSession stays nil, so agent-initiated sends to the session
// are refused rather than posted), no default-eligibility row lands, and
// the next contact (write recovered) succeeds.
func TestRouting_RegistrationWriteFailureRefusesMessage(t *testing.T) {
	dmTarget := Target{Kind: TargetDM, Channel: "D100", User: "U1"}
	chatID := dmTarget.ChatID()

	t.Run("never-default write failure", func(t *testing.T) {
		h, fake, idx := newFailingManager(t, map[string]bool{session.ChatMetaNeverDefault: true})

		h.deliver(dmEvent("U1", "D100", "hello", "1"))
		if envs := h.mgr.core("helen").enq.(*fakeAgent).drained(); len(envs) != 0 {
			t.Fatalf("dm with a failed never-default write started %d turns, want 0 (fail closed)", len(envs))
		}
		for _, key := range []string{chatMetaKeySlackTarget, "registered", session.ChatMetaNeverDefault} {
			if got := chatMetaRow(t, idx, "helen", platformName, chatID, key); got != "" {
				t.Errorf("row %s = %q despite the failed never-default write; want none (the chat must not be default-eligible)", key, got)
			}
		}
		if h.mgr.BotForSession(sessionKeyFor("helen", dmTarget)) != nil {
			t.Error("failed registration left a live route (BotForSession non-nil)")
		}

		// The write recovers: the next contact registers and starts the turn.
		fake.fail = nil
		h.deliver(dmEvent("U1", "D100", "try again", "2"))
		envs := h.waitEnvelopes(h.mgr.core("helen"), 1)
		if envs[0].Text != "try again" {
			t.Errorf("retried dm text = %q", envs[0].Text)
		}
		for _, key := range []string{chatMetaKeySlackTarget, "registered", session.ChatMetaNeverDefault} {
			want := "true"
			if key == chatMetaKeySlackTarget {
				want = `{"kind":"dm","channel":"D100","user":"U1"}`
			}
			if got := chatMetaRow(t, idx, "helen", platformName, chatID, key); got != want {
				t.Errorf("after retry, row %s = %q, want %q", key, got, want)
			}
		}
	})

	t.Run("slack_target write failure leaves no route", func(t *testing.T) {
		h, fake, idx := newFailingManager(t, map[string]bool{chatMetaKeySlackTarget: true})
		sessionKey := sessionKeyFor("helen", dmTarget)

		h.deliver(dmEvent("U1", "D100", "hello", "1"))
		if envs := h.mgr.core("helen").enq.(*fakeAgent).drained(); len(envs) != 0 {
			t.Fatalf("dm with a failed slack_target write started %d turns, want 0 (fail closed)", len(envs))
		}
		// No route anywhere: not the in-memory cache, not the durable row a
		// later lookup would cache from — BotForSession stays nil and a
		// session-addressed send is refused with nothing posted.
		if h.mgr.BotForSession(sessionKey) != nil {
			t.Fatal("failed slack_target write left a live route (BotForSession non-nil)")
		}
		mark := h.fake.totalCalls()
		if err := h.mgr.PrimaryBot("helen").SendToSession(sessionKey, "leak?"); !errors.Is(err, errNoSessionAddress) {
			t.Errorf("SendToSession to the refused session = %v, want errNoSessionAddress", err)
		}
		if got := h.fake.totalCalls(); got != mark {
			t.Errorf("send to the refused session made %d API calls, want 0", got-mark)
		}
		// The chat IS marked never-default (the flag row precedes the route
		// row), so the leftover ownership rows cannot make it default-eligible.
		if got := chatMetaRow(t, idx, "helen", platformName, chatID, session.ChatMetaNeverDefault); got != "true" {
			t.Errorf("never_default row = %q after a failed slack_target write; want true (flag precedes the route)", got)
		}

		// Recovery: the next contact writes the missing row and routes.
		fake.fail = nil
		h.deliver(dmEvent("U1", "D100", "try again", "2"))
		envs := h.waitEnvelopes(h.mgr.core("helen"), 1)
		if envs[0].SessionKey != sessionKey {
			t.Errorf("retried dm session key = %q, want %q", envs[0].SessionKey, sessionKey)
		}
		if err := h.mgr.BotForSession(sessionKey).SendToSession(sessionKey, "now routable"); err != nil {
			t.Errorf("send after recovery = %v", err)
		}
		if posts := h.fake.postsTo("D100"); len(posts) != 1 {
			t.Errorf("posts to the DM after recovery = %d, want 1", len(posts))
		}
	})
}

// TestRouting_UnmappedHoldPruned proves the per-thread "not set up here" hold
// is pruned on insert: once a hold expires the same thread gets a fresh
// reply, and stale entries for other threads are dropped, so the hold map
// stays bounded by live threads rather than every unmapped thread ever seen.
func TestRouting_UnmappedHoldPruned(t *testing.T) {
	h := newHarness(t, "", map[string]string{"C200": "helen"})
	h.allow("helen", true, "U1")

	h.deliver(mentionEvent("U1", "C999", "<@UBOTID> hi", "1", ""))
	if got := len(h.fake.postsTo("C999")); got != 1 {
		t.Fatalf("first mention replies = %d, want 1", got)
	}

	// Age this thread's hold past the window and plant a second expired
	// hold: the next mention replies again and the prune drops the stale
	// entries instead of keeping them forever.
	h.mgr.unmappedMu.Lock()
	h.mgr.unmapped["C999/1"] = time.Now().Add(-2 * h.mgr.unmappedHold)
	h.mgr.unmapped["C888/old"] = time.Now().Add(-2 * h.mgr.unmappedHold)
	h.mgr.unmappedMu.Unlock()

	h.deliver(mentionEvent("U1", "C999", "<@UBOTID> again", "2", "1"))
	if got := len(h.fake.postsTo("C999")); got != 2 {
		t.Errorf("replies after the hold expired = %d, want 2 (a fresh notice per window)", got)
	}
	h.mgr.unmappedMu.Lock()
	size := len(h.mgr.unmapped)
	h.mgr.unmappedMu.Unlock()
	if size != 1 {
		t.Errorf("hold map holds %d entries after pruning, want 1 (only the live thread)", size)
	}
}

// TestReceive_IgnoredEventStartsNoTurnNoReply runs the ignore table end to
// end: none of these events reaches an agent or produces a reply.
func TestReceive_IgnoredEventStartsNoTurnNoReply(t *testing.T) {
	h := newHarness(t, "helen", nil)
	h.allow("helen", true, "U1")
	controlDM(t, h) // harness alive

	events := []slackEvent{
		{Type: "message", ChannelType: "im", User: "U1", BotID: "B1", Text: "bot", TS: "b1"},
		{Type: "message", ChannelType: "im", User: "UBOTID", Text: "self", TS: "s1"},
		{Type: "message", ChannelType: "im", User: "U1", Subtype: "message_changed", Text: "edit", TS: "e1"},
		{Type: "message", ChannelType: "im", User: "U1", Subtype: "message_deleted", TS: "e2"},
		{Type: "message", ChannelType: "channel", User: "U1", Text: "no mention", TS: "c1"},
		{Type: "message", ChannelType: "channel", User: "U1", Text: "in bot thread, no mention", TS: "c2", ThreadTS: "c1"},
		{Type: "member_joined_channel", User: "U1", Channel: "D100"},
	}
	beforePosts := len(h.fake.postCalls())
	beforeEnvs := len(h.mgr.core("helen").enq.(*fakeAgent).drained())
	for _, ev := range events {
		h.deliver(ev)
	}
	if got := len(h.fake.postCalls()); got != beforePosts {
		t.Errorf("ignored events produced %d posts", got-beforePosts)
	}
	if got := len(h.mgr.core("helen").enq.(*fakeAgent).drained()); got != beforeEnvs {
		t.Errorf("ignored events started %d turns", got-beforeEnvs)
	}
}

// TestAccess_RefusedUserStartsNoTurn proves the interim access rule: the
// allowlist cascade resolved for the routed agent; an empty list with the
// strict default blocks everyone; a refused message starts no turn, gets no
// reply, and leaves no target row.
func TestAccess_RefusedUserStartsNoTurn(t *testing.T) {
	t.Run("empty list blocks everyone", func(t *testing.T) {
		h := newHarness(t, "helen", nil)
		h.allow("helen", true) // interim default: nobody allowed
		control := &fakeAgent{}
		h.mgr.core("helen").enq = control
		h.deliver(dmEvent("U1", "D100", "hello", "1"))
		if envs := control.drained(); len(envs) != 0 {
			t.Errorf("refused dm started %d turns", len(envs))
		}
		if calls := h.fake.totalCalls(); calls != 0 {
			t.Errorf("refused dm made %d API calls, want 0", calls)
		}
		chatID := Target{Kind: TargetDM, Channel: "D100", User: "U1"}.ChatID()
		if got := chatMetaRow(t, h.idx, "helen", platformName, chatID, chatMetaKeySlackTarget); got != "" {
			t.Errorf("refused message registered a target row (%q)", got)
		}
	})

	t.Run("listed user is allowed", func(t *testing.T) {
		h := newHarness(t, "helen", nil)
		h.allow("helen", true, "U1")
		h.deliver(dmEvent("U1", "D100", "hello", "1"))
		if envs := h.waitEnvelopes(h.mgr.core("helen"), 1); len(envs) != 1 {
			t.Fatalf("allowed dm started %d turns, want 1", len(envs))
		}
	})

	t.Run("allowed_users_only=false opens the door", func(t *testing.T) {
		h := newHarness(t, "helen", nil)
		h.allow("helen", false)
		h.deliver(dmEvent("U1", "D100", "hello", "1"))
		h.waitEnvelopes(h.mgr.core("helen"), 1)
	})

	t.Run("per-agent list resolved for the routed agent", func(t *testing.T) {
		h := newHarness(t, "helen", map[string]string{"C201": "bob"})
		h.allow("helen", true, "U1") // helen allows U1; bob allows nobody
		h.allow("bob", true)
		h.deliver(mentionEvent("U1", "C201", "<@UBOTID> hi bob", "1", ""))
		if envs := h.mgr.core("bob").enq.(*fakeAgent).drained(); len(envs) != 0 {
			t.Errorf("mention routed to bob started %d turns though bob's list is empty", len(envs))
		}
		if envs := h.mgr.core("helen").enq.(*fakeAgent).drained(); len(envs) != 0 {
			t.Errorf("refused mention leaked to helen (%d turns)", len(envs))
		}
	})
}

// TestCommandLookingTextIsOrdinaryMessage proves no foci command runs from
// Slack: command-looking text is enqueued verbatim, exactly as typed.
func TestCommandLookingTextIsOrdinaryMessage(t *testing.T) {
	h := newHarness(t, "helen", nil)
	h.allow("helen", true, "U1")

	for i, text := range []string{"/restart", "/stop", ".tmux", "/secrets list", "/model opus"} {
		h.deliver(dmEvent("U1", "D100", text, string(rune('1'+i))))
	}
	envs := h.waitEnvelopes(h.mgr.core("helen"), 5)
	want := []string{"/restart", "/stop", ".tmux", "/secrets list", "/model opus"}
	for i, env := range envs {
		if env.Text != want[i] {
			t.Errorf("envelope %d text = %q, want %q verbatim", i, env.Text, want[i])
		}
	}
	// A command would have produced a reply (or run); here nothing posted.
	if posts := h.fake.postCalls(); len(posts) != 0 {
		t.Errorf("command-looking text produced %d posts, want 0 (no command registry involved)", len(posts))
	}
}

// TestRouting_BareMention covers the no-words input shape (#2459 A1 review
// F1): a mention whose text is only "<@BOT>" still satisfies the trigger, so
// in an unmapped channel it gets the one-per-thread "not set up here" reply,
// exactly as a mention with words does. In a mapped channel it starts no turn
// and registers nothing (the reply there is an open product decision; silence
// is today's behaviour). A bare DM stays ignored: a DM has no mention.
func TestRouting_BareMention(t *testing.T) {
	t.Run("unmapped channel gets the not-set-up reply", func(t *testing.T) {
		h := newHarness(t, "", map[string]string{"C200": "helen"})
		h.deliver(mentionEvent("U1", "C999", "<@UBOTID>", "1700000000.000301", ""))
		posts := h.fake.postsTo("C999")
		if len(posts) != 1 {
			t.Fatalf("posts to the unmapped channel = %d, want 1", len(posts))
		}
		if posts[0].Body["thread_ts"] != "1700000000.000301" {
			t.Errorf("reply thread_ts = %v, want the mention's thread", posts[0].Body["thread_ts"])
		}
		if text, _ := posts[0].Body["text"].(string); !strings.Contains(text, "isn't set up") {
			t.Errorf("reply text = %q, want the not-set-up notice", text)
		}
	})

	t.Run("mapped channel starts no turn and registers nothing", func(t *testing.T) {
		h := newHarness(t, "", map[string]string{"C200": "helen"})
		h.deliver(mentionEvent("U1", "C200", "  <@UBOTID>  ", "1700000000.000302", ""))
		// Positive control in the same channel: a worded mention routes, so
		// the bare one was not lost to a broken harness.
		h.deliver(mentionEvent("U1", "C200", "<@UBOTID> real one", "1700000000.000303", ""))
		envs := h.waitEnvelopes(h.mgr.core("helen"), 1)
		if len(envs) != 1 || envs[0].Text != "real one" {
			t.Fatalf("turns = %+v, want only the worded mention", envs)
		}
		if posts := h.fake.postsTo("C200"); len(posts) != 0 {
			t.Errorf("bare mention in a mapped channel posted %d messages, want 0", len(posts))
		}
		chatID := Target{Kind: TargetThread, Channel: "C200", ThreadTS: "1700000000.000302"}.ChatID()
		if got := chatMetaRow(t, h.idx, "helen", platformName, chatID, chatMetaKeySlackTarget); got != "" {
			t.Errorf("bare mention registered a target row (%q)", got)
		}
	})
}

// TestRouting_FailedRegistrationLeftoverRows pins exactly which rows each
// failed registration write leaves behind, as docs/WIRING.md's Slack
// "Sessions" paragraph states (#2459 A1 review F6): a failed never_default
// write leaves nothing; a failed registered write leaves only the
// never_default row; a failed slack_target write leaves never_default and
// registered. In every case no slack_target row (no route) remains.
func TestRouting_FailedRegistrationLeftoverRows(t *testing.T) {
	dmTarget := Target{Kind: TargetDM, Channel: "D100", User: "U1"}
	chatID := dmTarget.ChatID()
	for _, tc := range []struct {
		failKey string
		want    map[string]string // row → value; "" means absent
	}{
		{session.ChatMetaNeverDefault, map[string]string{session.ChatMetaNeverDefault: "", "registered": "", chatMetaKeySlackTarget: ""}},
		{"registered", map[string]string{session.ChatMetaNeverDefault: "true", "registered": "", chatMetaKeySlackTarget: ""}},
		{chatMetaKeySlackTarget, map[string]string{session.ChatMetaNeverDefault: "true", "registered": "true", chatMetaKeySlackTarget: ""}},
	} {
		t.Run(tc.failKey, func(t *testing.T) {
			h, _, idx := newFailingManager(t, map[string]bool{tc.failKey: true})
			h.deliver(dmEvent("U1", "D100", "hello", "1"))
			for key, want := range tc.want {
				if got := chatMetaRow(t, idx, "helen", platformName, chatID, key); got != want {
					t.Errorf("after a failed %s write, row %s = %q, want %q", tc.failKey, key, got, want)
				}
			}
		})
	}
}
