//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"
)

// permissionScript builds a cc-stub script body that emits a single
// can_use_tool control_request after the assistant message. The caller
// supplies the requestID — predictable IDs let tests construct callback
// data strings before pushing the trigger message:
//
//	id := "req-bash-1"
//	h.WriteCCStubScript(t, "alpha", permissionScript(id, "Bash", map[string]any{"command": "ls"}, nil))
//	stub.PushUpdate(token, plainTextUpdate("trigger"))
//	stub.PushCallbackQuery(token, "im:"+id+":0", chatID, userID, 0) // 0 = Allow
//
// suggestions, if non-nil, populates the request's permission_suggestions
// so foci's Choices() emits the third "Always: <prefix>" button.
func permissionScript(requestID, toolName string, input map[string]any, suggestions []map[string]any) []byte {
	body := map[string]any{
		"text": "okay, asking for permission",
		"permission_requests": []map[string]any{
			{
				"tool_name":  toolName,
				"input":      input,
				"request_id": requestID,
			},
		},
	}
	if len(suggestions) > 0 {
		body["permission_requests"].([]map[string]any)[0]["permission_suggestions"] = suggestions
	}
	b, _ := json.Marshal(body)
	return b
}

// multiPermissionScript builds a cc-stub script body that emits N
// concurrent can_use_tool control_requests in a single turn — used by
// the concurrent-prompts assertion to verify foci tracks each by its
// request_id without collisions.
func multiPermissionScript(text string, reqs []map[string]any) []byte {
	body := map[string]any{"text": text, "permission_requests": reqs}
	b, _ := json.Marshal(body)
	return b
}

// callbackForAllow builds the callback_data string that simulates the
// user pressing the "Allow" button on a permission prompt with the given
// request_id. Index 0 of Choices() is always "allow" (see
// permissions.go:Choices). The "im:" prefix is the platform's interactive
// message dispatch tag.
func callbackForAllow(requestID string) string {
	return "im:" + requestID + ":0"
}

// callbackForDeny builds the callback_data string for the "Deny" button
// at index 1 of the choices.
func callbackForDeny(requestID string) string {
	return "im:" + requestID + ":1"
}

// callbackForAllowAlways builds the callback_data string for the n-th
// "Always: <prefix>" button (index 2+n in the choices). Tests that
// supply a single permission_suggestion target n=0.
func callbackForAllowAlways(requestID string, n int) string {
	return fmt.Sprintf("im:%s:%d", requestID, 2+n)
}

// callbackForUnknownIndex builds a callback_data string for a button
// index that doesn't exist (e.g. index 99). Tests use this to verify
// foci's bounds-checking treats out-of-range as a no-op / deny.
func callbackForUnknownIndex(requestID string, index int) string {
	return fmt.Sprintf("im:%s:%d", requestID, index)
}

// permissionRequestEntries filters the recorder to permission_request
// entries, in emission order. Tests use it to discover auto-generated
// request_ids when the script didn't pre-specify them.
func permissionRequestEntries(entries []recorderEntry) []recorderEntry {
	var out []recorderEntry
	for _, e := range entries {
		if e.Kind == "permission_request" {
			out = append(out, e)
		}
	}
	return out
}

// controlResponseEntries filters the recorder to control_response
// entries, in arrival order.
func controlResponseEntries(entries []recorderEntry) []recorderEntry {
	var out []recorderEntry
	for _, e := range entries {
		if e.Kind == "control_response" {
			out = append(out, e)
		}
	}
	return out
}

// findControlResponse polls the recorder until a control_response entry
// matching the given requestID appears. Returns the parsed inner payload
// (e.g. {"behavior":"allow",...}) on success, or nil on timeout.
func findControlResponse(t *testing.T, h *testharness.Harness, requestID string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range controlResponseEntries(readRecorderEntries(t, h.RecorderPath())) {
			if e.ControlRequestID != requestID {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(e.ControlResponse, &payload); err != nil {
				t.Fatalf("decode control_response for %s: %v\nraw=%s", requestID, err, e.ControlResponse)
			}
			return payload
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// waitForPermissionPrompt polls the Telegram stub's sent calls for a
// sendMessage whose body contains the supplied substring. Returns the
// matching call on success, or zero value + false on timeout. Useful to
// verify a permission prompt fired with expected wording before pressing
// the callback button.
func waitForPermissionPrompt(t *testing.T, stub *testharness.TelegramStub, token string, textSubstr string, timeout time.Duration) (testharness.SentCall, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, call := range stub.PeekSent(token) {
			if call.Method != "sendMessage" {
				continue
			}
			if strings.Contains(string(call.Body), textSubstr) {
				return call, true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return testharness.SentCall{}, false
}

// sendMessageHasInlineKeyboard returns whether the recorded sendMessage
// body has a reply_markup.inline_keyboard field. Used by tests that
// distinguish permission prompts (have buttons) from auto-approve flows
// (no buttons because no prompt was sent).
//
// gotgbot encodes outgoing sendMessage calls as application/x-www-form-
// urlencoded, so the harness's recordCall sees reply_markup as a
// JSON-encoded STRING value inside the form, not a nested map. Decode
// the string before probing for inline_keyboard.
func sendMessageHasInlineKeyboard(body []byte) bool {
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return false
	}
	raw, ok := env["reply_markup"].(string)
	if !ok || raw == "" {
		return false
	}
	var rm map[string]any
	if err := json.Unmarshal([]byte(raw), &rm); err != nil {
		return false
	}
	_, ok = rm["inline_keyboard"]
	return ok
}

// decodeInlineKeyboard extracts the inline_keyboard rows from a
// sendMessage body. Each button is returned as {text, callback_data}.
// Returns nil if the body has no inline keyboard. Handles the form-
// encoded JSON-string-inside-JSON-map shape gotgbot produces.
func decodeInlineKeyboard(body []byte) [][]map[string]string {
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	raw, ok := env["reply_markup"].(string)
	if !ok || raw == "" {
		return nil
	}
	var rm struct {
		InlineKeyboard [][]map[string]string `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(raw), &rm); err != nil {
		return nil
	}
	return rm.InlineKeyboard
}

// peekSendMessageBody returns the text field of the most recent
// sendMessage call to the given token. Useful for asserting fenced-block
// command formatting in permission prompts. The text comes through with
// HTML escapes intact (foci sends parse_mode=HTML), so backticks and
// other markdown-style characters are NOT interpreted here — but the
// command itself is rendered inside <pre><code>...</code></pre>.
func peekSendMessageBody(stub *testharness.TelegramStub, token string) string {
	calls := stub.PeekSent(token)
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method != "sendMessage" {
			continue
		}
		var env map[string]any
		_ = json.Unmarshal(calls[i].Body, &env)
		if t, ok := env["text"].(string); ok {
			return t
		}
	}
	return ""
}
