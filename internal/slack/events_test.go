package slack

import "testing"

// TestFilter_IgnoredEvents proves the "DMs and @mentions only" rule's
// negative half: bot/self/subtype/channel-without-mention events never
// classify as inbound messages.
func TestFilter_IgnoredEvents(t *testing.T) {
	cases := []struct {
		name   string
		ev     slackEvent
		reason ignoreReason
	}{
		{"bot_id set", slackEvent{Type: "message", ChannelType: "im", User: "U1", BotID: "B123", Text: "hi", TS: "1"}, ignoreBotMessage},
		{"bot_message subtype", slackEvent{Type: "message", ChannelType: "im", User: "U1", Subtype: "bot_message", Text: "hi", TS: "1"}, ignoreBotMessage},
		{"own message", slackEvent{Type: "message", ChannelType: "im", User: "UBOTID", Text: "hi", TS: "1"}, ignoreSelfMessage},
		{"no sender", slackEvent{Type: "message", ChannelType: "im", Text: "hi", TS: "1"}, ignoreNoSender},
		{"message_changed", slackEvent{Type: "message", ChannelType: "im", User: "U1", Subtype: "message_changed", Text: "hi", TS: "1"}, ignoreSubtype},
		{"message_deleted", slackEvent{Type: "message", ChannelType: "im", User: "U1", Subtype: "message_deleted", TS: "1"}, ignoreSubtype},
		{"channel_join", slackEvent{Type: "message", ChannelType: "channel", User: "U1", Subtype: "channel_join", Text: "joined", TS: "1"}, ignoreNotAddressed},
		{"thread_broadcast", slackEvent{Type: "message", ChannelType: "im", User: "U1", Subtype: "thread_broadcast", Text: "hi", TS: "1"}, ignoreSubtype},
		{"channel message without mention", slackEvent{Type: "message", ChannelType: "channel", User: "U1", Text: "just talking", TS: "1"}, ignoreNotAddressed},
		{"channel message in bot thread without mention", slackEvent{Type: "message", ChannelType: "channel", User: "U1", Text: "still just talking", TS: "2", ThreadTS: "1"}, ignoreNotAddressed},
		{"group message", slackEvent{Type: "message", ChannelType: "group", User: "U1", Text: "hi", TS: "1"}, ignoreNotAddressed},
		{"mpim message", slackEvent{Type: "message", ChannelType: "mpim", User: "U1", Text: "hi", TS: "1"}, ignoreNotAddressed},
		{"reaction_added", slackEvent{Type: "reaction_added", User: "U1", Channel: "C1"}, ignoreOtherType},
		{"empty dm", slackEvent{Type: "message", ChannelType: "im", User: "U1", Text: "  ", TS: "1"}, ignoreReason("empty message")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, reason := classifyEvent(tc.ev, "UBOTID")
			if reason == "" {
				t.Fatalf("event classified as %+v; want ignored", msg)
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

// TestFilter_AcceptedEvents proves the positive half: a plain DM, a
// file-sharing DM (with and without text), and top-level/in-thread mentions
// all classify — with the thread and mention-stripping rules applied.
func TestFilter_AcceptedEvents(t *testing.T) {
	t.Run("plain dm", func(t *testing.T) {
		msg, reason := classifyEvent(dmEvent("U1", "D100", "hello", "1700000000.000100"), "UBOTID")
		if reason != "" {
			t.Fatalf("plain dm ignored: %s", reason)
		}
		if msg.Kind != inboundDM || msg.User != "U1" || msg.Channel != "D100" || msg.Text != "hello" {
			t.Errorf("dm = %+v", msg)
		}
		if msg.ThreadTS != "" {
			t.Errorf("dm carries thread_ts %q; DM replies are not threaded", msg.ThreadTS)
		}
	})

	t.Run("file share dm with text", func(t *testing.T) {
		ev := dmEvent("U1", "D100", "the report", "1")
		ev.Subtype = "file_share"
		ev.Files = []slackFile{{Name: "report.pdf"}}
		msg, reason := classifyEvent(ev, "UBOTID")
		if reason != "" {
			t.Fatalf("file-share dm ignored: %s", reason)
		}
		want := "the report\n[shared file: report.pdf]"
		if msg.Text != want {
			t.Errorf("text = %q, want %q", msg.Text, want)
		}
	})

	t.Run("file-only dm", func(t *testing.T) {
		ev := dmEvent("U1", "D100", "", "1")
		ev.Subtype = "file_share"
		ev.Files = []slackFile{{Name: "chart.png"}}
		msg, reason := classifyEvent(ev, "UBOTID")
		if reason != "" {
			t.Fatalf("file-only dm ignored: %s", reason)
		}
		if msg.Text != "[shared file: chart.png]" {
			t.Errorf("text = %q, want the file line", msg.Text)
		}
	})

	t.Run("top-level mention roots a thread", func(t *testing.T) {
		msg, reason := classifyEvent(mentionEvent("U1", "C200", "<@UBOTID> fix this", "1700000000.000200", ""), "UBOTID")
		if reason != "" {
			t.Fatalf("mention ignored: %s", reason)
		}
		if msg.Kind != inboundMention {
			t.Errorf("kind = %v, want mention", msg.Kind)
		}
		if msg.ThreadTS != "1700000000.000200" {
			t.Errorf("thread_ts = %q, want the mention's own ts (a top-level mention roots a new thread)", msg.ThreadTS)
		}
		if msg.Text != "fix this" {
			t.Errorf("text = %q, want the mention stripped", msg.Text)
		}
	})

	t.Run("in-thread mention keeps its thread", func(t *testing.T) {
		msg, reason := classifyEvent(mentionEvent("U1", "C200", "<@UBOTID> and also", "1700000000.000300", "1700000000.000200"), "UBOTID")
		if reason != "" {
			t.Fatalf("in-thread mention ignored: %s", reason)
		}
		if msg.ThreadTS != "1700000000.000200" {
			t.Errorf("thread_ts = %q, want the parent thread's", msg.ThreadTS)
		}
	})

	t.Run("mention text without the bot token is kept whole", func(t *testing.T) {
		// Only another user's mention is present: nothing to strip.
		msg, _ := classifyEvent(mentionEvent("U1", "C200", "<@USOMEONE> hi", "1", ""), "UBOTID")
		if msg.Text != "<@USOMEONE> hi" {
			t.Errorf("text = %q, want %q", msg.Text, "<@USOMEONE> hi")
		}
	})

	t.Run("trailing mention keeps the words before it", func(t *testing.T) {
		// The mention need not lead: "cc the bot" puts it at the end, and
		// the words before it are the message.
		msg, reason := classifyEvent(mentionEvent("U1", "C200", "Can you check this <@UBOTID>", "1", ""), "UBOTID")
		if reason != "" {
			t.Fatalf("trailing mention ignored: %s", reason)
		}
		if msg.Text != "Can you check this" {
			t.Errorf("text = %q, want %q", msg.Text, "Can you check this")
		}
	})

	t.Run("mid-message mention keeps both halves", func(t *testing.T) {
		msg, reason := classifyEvent(mentionEvent("U1", "C200", "hey <@UBOTID> how are you", "1", ""), "UBOTID")
		if reason != "" {
			t.Fatalf("mid-message mention ignored: %s", reason)
		}
		if msg.Text != "hey how are you" {
			t.Errorf("text = %q, want %q", msg.Text, "hey how are you")
		}
	})
}

// TestFilter_DecodesEntities proves inbound text is decoded from Slack's
// entity encoding (&lt; &gt; &amp;): the agent sees the characters the user
// typed. A double-encoded &amp;lt; (the user literally typing "&lt;")
// survives as the literal text, and the <@U…> mention token — real angle
// brackets on the wire — is unaffected.
func TestFilter_DecodesEntities(t *testing.T) {
	t.Run("dm with entities", func(t *testing.T) {
		msg, reason := classifyEvent(dmEvent("U1", "D100", "a &lt; b &amp; c &gt; d", "1"), "UBOTID")
		if reason != "" {
			t.Fatalf("dm ignored: %s", reason)
		}
		if msg.Text != "a < b & c > d" {
			t.Errorf("text = %q, want the decoded characters", msg.Text)
		}
	})

	t.Run("literal &lt; the user typed survives", func(t *testing.T) {
		msg, _ := classifyEvent(dmEvent("U1", "D100", "type &amp;lt; here", "1"), "UBOTID")
		if msg.Text != "type &lt; here" {
			t.Errorf("text = %q, want the literal %q", msg.Text, "type &lt; here")
		}
	})

	t.Run("mention decode then strip", func(t *testing.T) {
		msg, _ := classifyEvent(mentionEvent("U1", "C200", "<@UBOTID> 5 &lt; 6 &amp; 7 &gt; 4", "1", ""), "UBOTID")
		if msg.Text != "5 < 6 & 7 > 4" {
			t.Errorf("text = %q, want the mention stripped and the entities decoded", msg.Text)
		}
	})

	t.Run("mention token is not an entity", func(t *testing.T) {
		// The bot mention is stripped; another user's mention must survive
		// decoding as the literal token (it is not &lt;@U…&gt; on the wire).
		msg, _ := classifyEvent(mentionEvent("U1", "C200", "<@UBOTID> cc <@U9>", "1", ""), "UBOTID")
		if msg.Text != "cc <@U9>" {
			t.Errorf("text = %q, want the other mention intact", msg.Text)
		}
	})
}
