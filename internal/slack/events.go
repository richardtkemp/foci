// Package slack is the Slack messaging provider (#2459): one Slack app,
// connected over Socket Mode (an outbound WebSocket, no public URL), serving
// several foci agents. One Slack app routes to several agents: config maps
// each channel to exactly one agent and names one default agent for DMs.
//
// Sessions: every DM user of the default agent is one session; every channel
// thread is one session of the channel's agent. The mapping is deterministic
// (a hash of the Slack conversation identity), so a restart lands the same DM
// or thread in the same session.
//
// The transport is modelled on the app's per-session views, not on
// Telegram/Discord's default-chat model: a bound view (Manager.BotForSession)
// pins every session-blind send to that session's own conversation, and the
// unbound per-agent connection drops agent-wide traffic instead of guessing a
// destination — Slack conversations are never default chats (#2459 A1).
package slack

import (
	"encoding/json"
	"fmt"
	"strings"
)

// platformName is the registry name and chat_metadata platform value.
const platformName = "slack"

// inboundKind classifies an accepted inbound message.
type inboundKind int

const (
	// inboundDM: a message event with channel_type "im" (Slack fires
	// app_mention only in channels, never in DMs).
	inboundDM inboundKind = iota
	// inboundMention: an app_mention in a channel (top-level or in-thread).
	inboundMention
)

// ignoreReason names why classifyEvent rejected an event. Informational only
// (logged at Debug); the constants exist so tests and logs share one
// vocabulary.
type ignoreReason string

const (
	ignoreBotMessage   ignoreReason = "bot message (bot_id or bot_message subtype)"
	ignoreSelfMessage  ignoreReason = "own message"
	ignoreNoSender     ignoreReason = "no sender"
	ignoreSubtype      ignoreReason = "message subtype"
	ignoreNotAddressed ignoreReason = "channel message without a mention"
	ignoreOtherType    ignoreReason = "event type is neither app_mention nor message"
	ignoreEmptyMessage ignoreReason = "empty message"
)

// slackEvent is the inner event of an events_api envelope (docs.slack.dev,
// checked 2026-10-15). Only the fields the classifier reads are modelled.
type slackEvent struct {
	Type        string      `json:"type"`
	Subtype     string      `json:"subtype"`
	User        string      `json:"user"`
	BotID       string      `json:"bot_id"`
	Text        string      `json:"text"`
	Channel     string      `json:"channel"`
	ChannelType string      `json:"channel_type"`
	TS          string      `json:"ts"`
	ThreadTS    string      `json:"thread_ts"`
	Files       []slackFile `json:"files"`
}

type slackFile struct {
	Name string `json:"name"`
}

// inboundMessage is a classified, accepted inbound message ready for routing.
// For a channel mention, ThreadTS is the thread the mention belongs to: the
// event's own thread_ts when it replied in one, else the event's ts (a
// top-level mention roots a new thread at itself). For a DM, ThreadTS is
// empty (DM replies are not threaded).
type inboundMessage struct {
	Kind     inboundKind
	User     string
	Text     string
	Channel  string
	ThreadTS string
	Files    []string
}

// classifyEvent applies the "DMs and @mentions only" rule (#2459 req 3). Pure:
// no I/O, no state beyond the bot's own user id.
//
//	app_mention                 → channel mention (never fires in DMs)
//	message with channel_type im → DM
//	every other message         → ignored: channel turns come from
//	                              app_mention only, so a channel message that
//	                              does not mention the bot starts no turn even
//	                              inside a thread the bot is in, and one
//	                              mention cannot arrive twice
//	every other type            → ignored
//
// Ignored within those: bot_id set or subtype bot_message (any other bot or
// app), the bot's own user, no sender, and any subtype other than "" and
// "file_share" (edits, deletions, joins — never start a turn), and an empty
// DM. A mention is accepted even with no words left after the bot's token is
// stripped: handleMessage decides what it gets. A message that
// shares files counts as a message; file NAMES join the turn text (contents
// are #2459 A2).
func classifyEvent(ev slackEvent, botUserID string) (inboundMessage, ignoreReason) {
	var kind inboundKind
	switch {
	case ev.Type == "app_mention":
		kind = inboundMention
	case ev.Type == "message" && ev.ChannelType == "im":
		kind = inboundDM
	case ev.Type == "message":
		return inboundMessage{}, ignoreNotAddressed
	default:
		return inboundMessage{}, ignoreOtherType
	}

	switch {
	case ev.BotID != "" || ev.Subtype == "bot_message":
		return inboundMessage{}, ignoreBotMessage
	case ev.User == "":
		return inboundMessage{}, ignoreNoSender
	case ev.User == botUserID:
		return inboundMessage{}, ignoreSelfMessage
	case ev.Subtype != "" && ev.Subtype != "file_share":
		return inboundMessage{}, ignoreSubtype
	}

	msg := inboundMessage{
		Kind:     kind,
		User:     ev.User,
		Text:     strings.TrimSpace(decodeEntities(ev.Text)),
		Channel:  ev.Channel,
		ThreadTS: ev.ThreadTS,
	}
	if kind == inboundMention && msg.ThreadTS == "" {
		// A top-level mention roots a new thread at its own message.
		msg.ThreadTS = ev.TS
	}
	if kind == inboundDM {
		msg.ThreadTS = ""
	}
	for _, f := range ev.Files {
		msg.Files = append(msg.Files, f.Name)
	}
	if kind == inboundMention {
		msg.Text = strings.TrimSpace(stripBotMention(msg.Text, botUserID))
	}
	if len(msg.Files) > 0 {
		var b strings.Builder
		if msg.Text != "" {
			b.WriteString(msg.Text)
			b.WriteString("\n")
		}
		for _, name := range msg.Files {
			fmt.Fprintf(&b, "[shared file: %s]\n", name)
		}
		msg.Text = strings.TrimSpace(b.String())
	}
	if msg.Text == "" && kind != inboundMention {
		// A mention with no words ("<@BOT>" alone) still addresses the bot:
		// it is accepted with empty text so routing decides (an unmapped
		// channel owes it the "not set up here" reply; review F1). An empty
		// DM addresses nothing.
		return inboundMessage{}, ignoreEmptyMessage
	}
	return msg, ""
}

// stripBotMention removes the bot's @mention token from a channel message's
// text, wherever it stands: Slack renders the mention as "<@U123>", and the
// words around it — before as well as after — are the user's message
// ("hey <@BOT> check this" → "hey check this", "check this <@BOT>" →
// "check this"). The whitespace that separated the mention from what follows
// is dropped with it, so the two halves do not glue or double-space.
func stripBotMention(text, botUserID string) string {
	token := "<@" + botUserID + ">"
	i := strings.Index(text, token)
	if i < 0 {
		return text
	}
	return text[:i] + strings.TrimLeft(text[i+len(token):], " \t\n\r")
}

// decodeEntities undoes Slack's entity encoding of the three markup control
// characters in message text (&lt; &gt; &amp; for < > & — the counterpart of
// escapeText, which re-encodes them on the way out): the agent must see the
// characters the user typed, not Slack's wire encoding of them. &amp; is
// decoded LAST so text the user literally typed as "&lt;" (&amp;lt; on the
// wire) survives as the literal "&lt;". Angle-bracket tokens are real angle
// brackets on the wire, never entities, so <@U…> mentions and <!channel>
// specials pass through decoding untouched — the bot's own mention is a real
// token too, so decoding never disturbs it before it is stripped.
func decodeEntities(text string) string {
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&amp;", "&")
	return text
}

// pressEvent is a parsed Block Kit button press (an "interactive" envelope
// with payload.type "block_actions").
type pressEvent struct {
	User       string // pressing user id (U…)
	Channel    string // channel the button's message is in
	MessageTS  string // ts of the button's message
	Value      string // the button's value: "<callbackPrefix><Data>"
	SessionKey string // the session foci baked into the actions block's block_id
}

// pressBlockIDPrefix marks an actions block as foci's, so a press on another
// app's message is never misparsed. buttonBlockID builds "<prefix><row>:<sessionKey>".
const pressBlockIDPrefix = "foci:"

// parsePress extracts the press routing fields from a block_actions payload.
// ok=false for payloads that are not block_actions, carry no action, or whose
// block_id is not foci's (another app's button).
func parsePress(payload json.RawMessage) (pressEvent, bool) {
	var p struct {
		Type string `json:"type"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
		Message struct {
			TS string `json:"ts"`
		} `json:"message"`
		Actions []struct {
			BlockID string `json:"block_id"`
			Value   string `json:"value"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return pressEvent{}, false
	}
	if p.Type != "block_actions" || len(p.Actions) == 0 {
		return pressEvent{}, false
	}
	action := p.Actions[0]
	if !strings.HasPrefix(action.BlockID, pressBlockIDPrefix) {
		return pressEvent{}, false
	}
	sessionKey := strings.TrimPrefix(action.BlockID, pressBlockIDPrefix)
	if i := strings.IndexByte(sessionKey, ':'); i >= 0 {
		sessionKey = sessionKey[i+1:] // drop the row number
	}
	if sessionKey == "" || p.User.ID == "" || p.Channel.ID == "" || p.Message.TS == "" {
		return pressEvent{}, false
	}
	return pressEvent{
		User:       p.User.ID,
		Channel:    p.Channel.ID,
		MessageTS:  p.Message.TS,
		Value:      action.Value,
		SessionKey: sessionKey,
	}, true
}
