package slack

// press_test.go: Block Kit button presses (#2459 req 5, 7) — the session
// travels in the block_id, the presser passes the allowlist, and a resolved
// prompt edits its own message.

import (
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/platform"
)

// pressPayload builds a block_actions payload like Slack delivers it.
func pressPayload(user, channel, ts, blockID, value string) json.RawMessage {
	p := map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": user},
		"channel": map[string]any{"id": channel},
		"message": map[string]any{"ts": ts},
		"actions": []any{map[string]any{"block_id": blockID, "value": value}},
	}
	blob, _ := json.Marshal(p)
	return blob
}

// postedBlockID extracts the actions block's block_id from a recorded
// chat.postMessage body (the press must echo back exactly what was sent).
func postedBlockID(t *testing.T, call fakeCall) string {
	t.Helper()
	blocks, ok := call.Body["blocks"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("post carries no blocks: %+v", call.Body)
	}
	for _, b := range blocks {
		blk, _ := b.(map[string]any)
		if blk["type"] == "actions" {
			id, _ := blk["block_id"].(string)
			return id
		}
	}
	t.Fatalf("post carries no actions block: %+v", call.Body)
	return ""
}

// postPrompt sends a live interactive prompt to the DM target through a
// bound view and returns the recorded post's block_id.
func (h *harness) postPrompt(t *testing.T, promptID string) string {
	t.Helper()
	_, err := platform.SendInteractiveMessageWithID(
		func() platform.Connection {
			return h.boundConn("helen", Target{Kind: TargetDM, Channel: "D100", User: "U1"})
		},
		promptID, "", "Allow Bash?", []platform.ButtonChoice{
			{Label: "Allow", Data: "allow"},
			{Label: "Deny", Data: "deny"},
		}, func(c platform.ButtonChoice) string { return "✅ " + c.Label }, nil)
	if err != nil {
		t.Fatal(err)
	}
	posts := h.fake.postsTo("D100")
	if len(posts) == 0 {
		t.Fatal("prompt never posted")
	}
	return postedBlockID(t, posts[len(posts)-1])
}

// TestPress_RefusedUserChangesNothing proves an unallowed presser resolves
// nothing: no callback fires, no edit lands — and the prompt stays live for
// an allowed presser.
func TestPress_RefusedUserChangesNothing(t *testing.T) {
	h := newHarness(t, "helen", nil)
	h.allow("helen", true, "U1")
	blockID := h.postPrompt(t, "req-refused")

	press, ok := parsePress(pressPayload("UEVIL", "D100", "5", blockID, "im:req-refused:0"))
	if !ok {
		t.Fatal("test press payload did not parse")
	}
	before := len(h.fake.callsFor("chat.update"))
	h.mgr.handlePress(press)
	if got := len(h.fake.callsFor("chat.update")); got != before {
		t.Errorf("refused press made %d edits", got-before)
	}

	// The prompt is still live: an allowed press resolves it.
	allowed, _ := parsePress(pressPayload("U1", "D100", "5", blockID, "im:req-refused:0"))
	h.mgr.handlePress(allowed)
	if got := len(h.fake.callsFor("chat.update")); got != before+1 {
		t.Errorf("allowed press after refused one made %d edits, want 1", got-before)
	}
}

// TestPress_AllowedUserResolvesAndEdits proves the happy path: an allowed
// presser resolves the prompt and the message is edited in place with its
// buttons stripped.
func TestPress_AllowedUserResolvesAndEdits(t *testing.T) {
	h := newHarness(t, "helen", nil)
	h.allow("helen", true, "U1")
	blockID := h.postPrompt(t, "req-press")

	if !strings.HasPrefix(blockID, "foci:0:helen/c") {
		t.Errorf("prompt block_id = %q, want it to carry the session key", blockID)
	}

	press, ok := parsePress(pressPayload("U1", "D100", "5", blockID, "im:req-press:1"))
	if !ok {
		t.Fatal("press did not parse")
	}
	h.mgr.handlePress(press)

	updates := h.fake.callsFor("chat.update")
	if len(updates) != 1 {
		t.Fatalf("edits = %d, want 1", len(updates))
	}
	if updates[0].Body["channel"] != "D100" || updates[0].Body["ts"] != "5" {
		t.Errorf("edit addressed %+v, want the pressed message", updates[0].Body)
	}
	if text, _ := updates[0].Body["text"].(string); text != "✅ Deny" {
		t.Errorf("edit text = %q, want the callback's ✅ Deny", text)
	}
	if blob, _ := json.Marshal(updates[0].Body["blocks"]); string(blob) != "[]" {
		t.Errorf("edit left blocks %s; buttons must be stripped", blob)
	}
}

// TestPress_EscapesMarkupInResolutionText proves the press resolution's edit
// text is escaped like every other Slack-bound text: a resolution carrying
// <!channel> renders as text in the edited message, never notifies.
func TestPress_EscapesMarkupInResolutionText(t *testing.T) {
	h := newHarness(t, "helen", nil)
	h.allow("helen", true, "U1")
	conn := h.boundConn("helen", Target{Kind: TargetDM, Channel: "D100", User: "U1"})

	if _, err := platform.SendInteractiveMessageWithID(
		func() platform.Connection { return conn },
		"req-ping", "", "Allow?", []platform.ButtonChoice{{Label: "Allow", Data: "allow"}},
		func(c platform.ButtonChoice) string { return "✅ chose <!channel> & <@U999>" }, nil); err != nil {
		t.Fatal(err)
	}
	posts := h.fake.postCalls()
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(posts))
	}
	press, ok := parsePress(pressPayload("U1", "D100", "5", postedBlockID(t, posts[0]), "im:req-ping:0"))
	if !ok {
		t.Fatal("press did not parse")
	}
	h.mgr.handlePress(press)

	updates := h.fake.callsFor("chat.update")
	if len(updates) != 1 {
		t.Fatalf("edits = %d, want 1", len(updates))
	}
	if text, _ := updates[0].Body["text"].(string); text != "✅ chose &lt;!channel&gt; &amp; &lt;@U999&gt;" {
		t.Errorf("edit text = %q, want the escaped resolution", text)
	}
}

// TestPress_NonImPrefixIgnored proves a press whose value is not a foci
// interactive token changes nothing, even when the value matches a LIVE
// prompt's token minus the "im:" prefix (another payload sharing the
// channel must not resolve foci's prompt) — and the prompt stays live for a
// properly prefixed press afterwards.
func TestPress_NonImPrefixIgnored(t *testing.T) {
	h := newHarness(t, "helen", nil)
	h.allow("helen", true, "U1")
	blockID := h.postPrompt(t, "req-nonim")

	press, ok := parsePress(pressPayload("U1", "D100", "5", blockID, "req-nonim:0"))
	if !ok {
		t.Fatal("press did not parse")
	}
	before := h.fake.totalCalls()
	h.mgr.handlePress(press)
	if got := h.fake.totalCalls(); got != before {
		t.Errorf("non-im press made %d API calls, want 0 (a value without the im: prefix is not foci's, however live the token it names)", got-before)
	}

	// The prompt is still live: the prefixed value resolves it.
	allowed, _ := parsePress(pressPayload("U1", "D100", "5", blockID, "im:req-nonim:0"))
	h.mgr.handlePress(allowed)
	if got := len(h.fake.callsFor("chat.update")); got != 1 {
		t.Errorf("press with the im: prefix after the ignored one made %d edits, want 1", got)
	}
}

// TestPress_ForeignBlockIDIgnored proves parsePress rejects a block_id that
// is not foci's (another app's button can never route into foci).
func TestPress_ForeignBlockIDIgnored(t *testing.T) {
	if _, ok := parsePress(pressPayload("U1", "D100", "5", "otherapp:0:helen/c1", "im:x:0")); ok {
		t.Error("foreign block_id parsed; want rejected")
	}
	if _, ok := parsePress(pressPayload("U1", "D100", "5", "", "im:x:0")); ok {
		t.Error("empty block_id parsed; want rejected")
	}
}
