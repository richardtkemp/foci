package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"foci/internal/platform"
)

// block is one Block Kit block, pre-encoded. Slack's block payloads are
// deeply nested and only written here, so raw JSON keeps the builders local.
type block = json.RawMessage

// maxBlockIDLen is Slack's cap on a block_id (docs.slack.dev). A longer id is
// refused rather than truncated — a truncated id would silently lose the
// session the press needs to route.
const maxBlockIDLen = 255

// errNoSessionAddress is returned by a session-blind send on a connection
// with no session to address (the unbound per-agent instance): the send is
// dropped, never posted to a guessed conversation.
var errNoSessionAddress = errors.New("slack: no session to address")

// errForeignSession is returned when a bound view is asked to address a
// session in another conversation (Conn.sessionGuard, #2459 req 5): the
// call is refused and logged, nothing is sent or edited.
var errForeignSession = errors.New("slack: session belongs to another conversation than this view's")

// msgRef is a sent Slack message's identity: "<channel>:<ts>" for a message
// in a DM, "<channel>:<ts>:<thread_ts>" for a thread reply. The thread is
// part of the identity because two threads of one channel are different
// conversations (#2459 req 5): an edit-by-id can then be guarded on the full
// conversation, not just the channel. Edit-by-id callers
// (CancelInteractiveMessage, the expiry sweep, the compaction ⏳→✅ edit)
// hold no session context, so the id is self-addressed.
type msgRef struct {
	Channel  string
	TS       string
	ThreadTS string // "" for a DM (unthreaded) message
}

func (r msgRef) String() string {
	if r.ThreadTS == "" {
		return r.Channel + ":" + r.TS
	}
	return r.Channel + ":" + r.TS + ":" + r.ThreadTS
}

// parseMsgRef splits "<channel>:<ts>" and "<channel>:<ts>:<thread_ts>";
// ok=false for anything else. Channel ids and timestamps contain no colons,
// so the split is unambiguous.
func parseMsgRef(id string) (msgRef, bool) {
	parts := strings.Split(id, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return msgRef{}, false
	}
	if parts[0] == "" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
		return msgRef{}, false
	}
	ref := msgRef{Channel: parts[0], TS: parts[1]}
	if len(parts) == 3 {
		ref.ThreadTS = parts[2]
	}
	return ref, true
}

// parseEditRef parses an id an edit surface acts on, reporting the refusal
// for an id this transport could never have issued.
func parseEditRef(msgID string) (msgRef, error) {
	ref, ok := parseMsgRef(msgID)
	if !ok {
		return msgRef{}, fmt.Errorf("slack: edit of unknown message id %q", msgID)
	}
	return ref, nil
}

// buttonBlockID builds the actions block's block_id —
// "<pressBlockIDPrefix><row>:<sessionKey>" — so a button press carries its
// own session. ok=false when the id would exceed Slack's cap.
func buttonBlockID(row int, sessionKey string) (string, bool) {
	id := fmt.Sprintf("%s%d:%s", pressBlockIDPrefix, row, sessionKey)
	if len(id) > maxBlockIDLen {
		return "", false
	}
	return id, true
}

// buttonBlocks renders the prompt body and one actions block per button row.
// Toggle buttons are dropped (Slack has no in-place prompt re-render loop in
// A1; the app does the same). The returned error names an over-long block_id
// — the send is refused, never truncated.
func buttonBlocks(text string, buttons []platform.ButtonChoice, callbackPrefix, sessionKey string) ([]block, error) {
	out := []block{sectionBlock(text)}
	byRow := make(map[int][]platform.ButtonChoice)
	for _, b := range buttons {
		if b.Toggle != nil {
			continue
		}
		byRow[b.Row] = append(byRow[b.Row], b)
	}
	rows := make([]int, 0, len(byRow))
	for row := range byRow {
		rows = append(rows, row)
	}
	sort.Ints(rows)
	for i, row := range rows {
		choices := byRow[row]
		id, ok := buttonBlockID(i, sessionKey)
		if !ok {
			return nil, fmt.Errorf("block_id for row %d exceeds Slack's %d-character cap (session %s): refusing the send rather than truncating it", row, maxBlockIDLen, sessionKey)
		}
		elements := make([]map[string]any, len(choices))
		for j, c := range choices {
			elements[j] = map[string]any{
				"type":      "button",
				"action_id": fmt.Sprintf("btn_%d", j),
				"text":      map[string]any{"type": "plain_text", "text": c.Label},
				"value":     callbackPrefix + c.Data,
			}
		}
		actions := map[string]any{"type": "actions", "block_id": id, "elements": elements}
		blob, err := json.Marshal(actions)
		if err != nil {
			return nil, err
		}
		out = append(out, blob)
	}
	return out, nil
}

// escapeText escapes Slack's three markup control characters so agent text
// renders literally: unescaped, a "<!channel>" or "<@U…>" in a reply would
// notify or ping the whole channel from the bot. A1 sends plain text (#2459
// A2 adds Markdown rendering); every field Slack parses (message text, block
// text, an edit, an upload comment) passes through here on the way in.
func escapeText(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")
	return text
}

// sectionBlock renders the prompt body as one mrkdwn section.
func sectionBlock(text string) block {
	blob, err := json.Marshal(map[string]any{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": escapeText(text)},
	})
	if err != nil {
		return block(`{"type":"section","text":{"type":"plain_text","text":"?"}}`)
	}
	return blob
}

// post sends one message to a target's own conversation — the channel and,
// for a thread target, the thread. It never sets reply_broadcast: a channel
// reply stays in the thread.
func (c *agentCore) post(ctx context.Context, t Target, text string) (msgRef, error) {
	ts, err := c.api.postMessage(ctx, postParams{
		Channel:  t.Channel,
		ThreadTS: t.threadTS(),
		Text:     escapeText(text),
	})
	if err != nil {
		return msgRef{}, err
	}
	return msgRef{Channel: t.Channel, TS: ts, ThreadTS: t.threadTS()}, nil
}

// postButtons sends a body with button rows into a target's conversation.
func (c *agentCore) postButtons(ctx context.Context, t Target, text string, buttons []platform.ButtonChoice, callbackPrefix, sessionKey string) (msgRef, error) {
	blocks, err := buttonBlocks(text, buttons, callbackPrefix, sessionKey)
	if err != nil {
		return msgRef{}, err
	}
	ts, err := c.api.postMessage(ctx, postParams{
		Channel:  t.Channel,
		ThreadTS: t.threadTS(),
		Text:     escapeText(text),
		Blocks:   blocks,
	})
	if err != nil {
		return msgRef{}, err
	}
	return msgRef{Channel: t.Channel, TS: ts, ThreadTS: t.threadTS()}, nil
}

// editText replaces a message's text and strips its buttons (blocks: []).
func (c *agentCore) editText(ctx context.Context, ref msgRef, text string) error {
	return c.api.updateMessage(ctx, updateParams{Channel: ref.Channel, TS: ref.TS, Text: escapeText(text), Blocks: []block{}})
}

// upload sends a file into a target's conversation (captioned when caption
// fits Slack's limits; a long caption rides as the initial comment text the
// same way, since external upload has no separate message).
func (c *agentCore) upload(ctx context.Context, t Target, filename string, data []byte, caption string) error {
	return c.api.uploadFile(ctx, uploadParams{
		Filename:       filename,
		Data:           data,
		Channel:        t.Channel,
		ThreadTS:       t.threadTS(),
		InitialComment: escapeText(caption),
	})
}
