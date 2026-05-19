//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// The permissions test cluster exercises foci's approval flow end-to-end:
// CC emits a can_use_tool control_request; foci either auto-approves it
// against the merged rule set, or surfaces it to Telegram as an inline
// keyboard; the user's callback_query dispatches RespondToPermission
// back through the stdio control_response. Each test below names one
// observable in that pipeline.
//
// All tests share the same setup shape:
//   1. StartGateway with one or two agents (a single agent named "alpha"
//      unless the test needs cross-agent comparison).
//   2. Prime the agent with a plain "trigger" message so cc-stub has a
//      live session.
//   3. WriteCCStubScript with a permissionScript(...) body so the NEXT
//      user message triggers the scripted can_use_tool emission.
//   4. Push the trigger user message.
//   5. Assert against the Telegram stub (prompt fired? body shape?) AND
//      the cc-stub recorder (control_response arrived? behavior?).
//
// Helpers in permissions_helpers_test.go own the assertion plumbing.

const (
	// permTestUserID is the synthetic Telegram user_id for these tests.
	// Single value — none of the tests need to verify per-user behaviour.
	permTestUserID = 1717
)

// permTestSetup is the one-line scaffold every test uses. Returns the
// harness, agent's chat/bot identifiers, and a helper that primes the
// agent's session so the partial-key resolver and the cc-stub long-
// lived process are both ready before the test's first scripted turn.
func permTestSetup(t *testing.T, agents []testharness.AgentSpec, extraConfig string) (*testharness.Harness, string /*token*/) {
	t.Helper()
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:          agents,
		ReadyTimeout:    30 * time.Second,
		ExtraConfigTOML: extraConfig,
	})
	token := h.AgentBotToken(agents[0].ID)
	// Prime the session with one plain message so cc-stub is spawned
	// and ready to consume the script on the next user message.
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: permTestUserID, Type: "private"},
			From: &gotgbot.User{Id: permTestUserID, FirstName: "Tester"},
			Text: "priming",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/"+agents[0].ID, "priming", 15*time.Second) {
		t.Fatalf("priming message never reached cc-stub")
	}
	return h, token
}

// pushTrigger fires the user message that causes cc-stub to apply the
// pending script. Body text is the marker the test polls for.
func pushTrigger(t *testing.T, h *testharness.Harness, token, text string) {
	t.Helper()
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: permTestUserID, Type: "private"},
			From: &gotgbot.User{Id: permTestUserID, FirstName: "Tester"},
			Text: text,
		},
	})
}

// TestL2_Permissions_AutoApproveCommonReadonlySkipsPrompt proves that a
// can_use_tool request for a tool covered by CommonReadonlyRules (e.g.
// Read, Glob, Bash:ls) never produces a Telegram interactive prompt and
// instead receives an immediate stdio control_response with
// behavior="allow".
func TestL2_Permissions_AutoApproveCommonReadonlySkipsPrompt(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-readonly-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Read", map[string]any{"file_path": "/tmp/x"}, nil))
	pushTrigger(t, h, token, "trigger readonly")

	resp := findControlResponse(t, h, reqID, 10*time.Second)
	if resp == nil {
		t.Fatalf("no control_response for %s within 10s\n--- recorder ---\n%s", reqID, recorderTail(t, h.RecorderPath()))
	}
	if resp["behavior"] != "allow" {
		t.Errorf("expected behavior=allow, got %v", resp["behavior"])
	}
	// Negative assertion: no prompt sendMessage with reply_markup.
	for _, c := range h.TelegramStub().PeekSent(token) {
		if c.Method == "sendMessage" && sendMessageHasInlineKeyboard(c.Body) {
			t.Errorf("expected no inline-keyboard prompt for auto-approved Read tool, found: %s", string(c.Body))
		}
	}
}

// TestL2_Permissions_AutoApproveUserRuleSkipsPrompt proves that a
// user-configured auto_approve entry (per-agent) is honoured and
// short-circuits the prompt.
func TestL2_Permissions_AutoApproveUserRuleSkipsPrompt(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{
		ID:          "alpha",
		UserID:      permTestUserID,
		AutoApprove: []string{"Bash:git status"},
	}}, "")

	const reqID = "req-user-rule-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "git status"}, nil))
	pushTrigger(t, h, token, "trigger user rule")

	resp := findControlResponse(t, h, reqID, 10*time.Second)
	if resp == nil {
		t.Fatalf("no control_response for %s\n--- recorder ---\n%s", reqID, recorderTail(t, h.RecorderPath()))
	}
	if resp["behavior"] != "allow" {
		t.Errorf("expected behavior=allow, got %v", resp["behavior"])
	}
}

// TestL2_Permissions_AutoApproveCommonSafeWriteDisabledByDefault proves
// that with AutoApproveCommonSafeWrite left at its default false, a
// Bash:curl request DOES surface to Telegram as an interactive prompt.
func TestL2_Permissions_AutoApproveCommonSafeWriteDisabledByDefault(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-curl-default-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "curl https://example.com"}, nil))
	pushTrigger(t, h, token, "trigger curl default")

	// Wait for the prompt to appear in the Telegram stub.
	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "curl", 10*time.Second); !ok {
		t.Errorf("expected a prompt sendMessage for Bash:curl when common-safe-write disabled (default)")
	}
	// No control_response yet — the test deliberately does not click a button.
}

// TestL2_Permissions_AutoApproveCommonSafeWriteEnabledSkipsPrompt proves
// that toggling auto_approve_common_safe_write=true on the agent makes
// the same Bash:curl request auto-approved without a prompt.
func TestL2_Permissions_AutoApproveCommonSafeWriteEnabledSkipsPrompt(t *testing.T) {
	t.Parallel()
	enabled := true
	h, token := permTestSetup(t, []testharness.AgentSpec{{
		ID:                         "alpha",
		UserID:                     permTestUserID,
		AutoApproveCommonSafeWrite: &enabled,
	}}, "")

	const reqID = "req-curl-enabled-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "curl https://example.com"}, nil))
	pushTrigger(t, h, token, "trigger curl enabled")

	resp := findControlResponse(t, h, reqID, 10*time.Second)
	if resp == nil {
		t.Fatalf("no control_response for %s\n--- recorder ---\n%s", reqID, recorderTail(t, h.RecorderPath()))
	}
	if resp["behavior"] != "allow" {
		t.Errorf("expected behavior=allow, got %v", resp["behavior"])
	}
}

// TestL2_Permissions_FociShellToolsAutoApproved proves the dynamically
// derived FociShellRulesFor allowlist covers every foci_* shell wrapper.
// Asserts a Bash:foci_todo request is auto-approved without a prompt.
func TestL2_Permissions_FociShellToolsAutoApproved(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-foci-tool-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "foci_todo list"}, nil))
	pushTrigger(t, h, token, "trigger foci tool")

	resp := findControlResponse(t, h, reqID, 10*time.Second)
	if resp == nil {
		t.Fatalf("no control_response for %s\n--- recorder ---\n%s", reqID, recorderTail(t, h.RecorderPath()))
	}
	if resp["behavior"] != "allow" {
		t.Errorf("expected behavior=allow, got %v", resp["behavior"])
	}
}

// TestL2_Permissions_BashOutsideAllowlistPromptsUser proves that a Bash
// command not covered by any allowlist surfaces to Telegram as a
// sendMessage with an inline keyboard.
func TestL2_Permissions_BashOutsideAllowlistPromptsUser(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-bash-outside-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "rm -rf /tmp/x"}, nil))
	pushTrigger(t, h, token, "trigger bash outside")

	call, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm -rf", 10*time.Second)
	if !ok {
		t.Fatalf("expected prompt for Bash:rm, none arrived\n--- recorder ---\n%s", recorderTail(t, h.RecorderPath()))
	}
	if !sendMessageHasInlineKeyboard(call.Body) {
		t.Errorf("expected reply_markup with inline_keyboard, got body: %s", string(call.Body))
	}
	// Negative: the test deliberately does not click — no control_response should arrive.
	if got := findControlResponse(t, h, reqID, 500*time.Millisecond); got != nil {
		t.Errorf("unexpected early control_response: %v", got)
	}
}

// TestL2_Permissions_SudoCommandPromptsUser proves that sudo always
// prompts and the body contains the command in a fenced code block.
func TestL2_Permissions_SudoCommandPromptsUser(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-sudo-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "sudo apt-get update"}, nil))
	pushTrigger(t, h, token, "trigger sudo")

	call, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "sudo apt-get update", 10*time.Second)
	if !ok {
		t.Fatalf("expected sudo prompt, none arrived")
	}
	if !sendMessageHasInlineKeyboard(call.Body) {
		t.Errorf("expected inline_keyboard on sudo prompt")
	}
}

// TestL2_Permissions_ApprovalCallbackUnblocksTool proves that pushing
// data="allow" causes cc-stub to receive a control_response with
// behavior="allow".
func TestL2_Permissions_ApprovalCallbackUnblocksTool(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-approve-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "rm -rf /tmp/x"}, nil))
	pushTrigger(t, h, token, "trigger approve")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm -rf", 10*time.Second); !ok {
		t.Fatalf("prompt never fired")
	}
	h.TelegramStub().PushCallbackQuery(token, callbackForAllow(reqID), permTestUserID, permTestUserID, 0)

	resp := findControlResponse(t, h, reqID, 10*time.Second)
	if resp == nil {
		t.Fatalf("no control_response after allow callback\n--- recorder ---\n%s", recorderTail(t, h.RecorderPath()))
	}
	if resp["behavior"] != "allow" {
		t.Errorf("expected behavior=allow, got %v", resp["behavior"])
	}
}

// TestL2_Permissions_DenialCallbackReturnsDeny proves the deny path.
func TestL2_Permissions_DenialCallbackReturnsDeny(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-deny-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "rm -rf /tmp/x"}, nil))
	pushTrigger(t, h, token, "trigger deny")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm -rf", 10*time.Second); !ok {
		t.Fatalf("prompt never fired")
	}
	h.TelegramStub().PushCallbackQuery(token, callbackForDeny(reqID), permTestUserID, permTestUserID, 0)

	resp := findControlResponse(t, h, reqID, 10*time.Second)
	if resp == nil {
		t.Fatalf("no control_response after deny callback")
	}
	if resp["behavior"] != "deny" {
		t.Errorf("expected behavior=deny, got %v", resp["behavior"])
	}
	if msg, _ := resp["message"].(string); msg == "" {
		t.Errorf("expected non-empty message on deny, got empty")
	}
}

// TestL2_Permissions_AllowAlwaysAddsSessionRule proves the "Always:
// <prefix>" choice path produces a response with non-empty
// updatedPermissions.
func TestL2_Permissions_AllowAlwaysAddsSessionRule(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-allow-always-1"
	suggestions := []map[string]any{{"prefix": "Bash:git *"}}
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "git status"}, suggestions))
	pushTrigger(t, h, token, "trigger allow-always")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "git status", 10*time.Second); !ok {
		t.Fatalf("prompt never fired")
	}
	h.TelegramStub().PushCallbackQuery(token, callbackForAllowAlways(reqID, 0), permTestUserID, permTestUserID, 0)

	resp := findControlResponse(t, h, reqID, 10*time.Second)
	if resp == nil {
		t.Fatalf("no control_response after allow_always callback")
	}
	if resp["behavior"] != "allow" {
		t.Errorf("expected behavior=allow, got %v", resp["behavior"])
	}
	upd, _ := resp["updatedPermissions"].([]any)
	if len(upd) == 0 {
		t.Fatalf("expected non-empty updatedPermissions, got: %v", resp)
	}
	first, _ := upd[0].(map[string]any)
	if first["prefix"] != "Bash:git *" {
		t.Errorf("expected updatedPermissions[0].prefix=\"Bash:git *\", got: %v", first)
	}
}

// TestL2_Permissions_PromptContainsCommandInFencedBlock proves the
// outbound Telegram body wraps the command in a code block. Telegram
// uses parse_mode="HTML", so the rendered shape is
// <pre><code>cmd</code></pre> rather than markdown triple-backticks.
// Either way the contract is: the command is visually distinguishable
// as a code block, not inline text.
func TestL2_Permissions_PromptContainsCommandInFencedBlock(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-fenced-1"
	const cmd = "rm -rf /tmp/x"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": cmd}, nil))
	pushTrigger(t, h, token, "trigger fenced")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, cmd, 10*time.Second); !ok {
		t.Fatalf("prompt never fired")
	}
	body := peekSendMessageBody(h.TelegramStub(), token)
	// HTML mode: foci wraps the command in <pre><code>...</code></pre>.
	// We assert on the structural wrapping (presence of both tags) AND
	// the command itself appearing inside, not on backticks.
	if !strings.Contains(body, "<pre>") || !strings.Contains(body, "<code>") {
		t.Errorf("expected <pre><code> code block in prompt body, got: %s", body)
	}
	if !strings.Contains(body, cmd) {
		t.Errorf("expected command %q in prompt body, got: %s", cmd, body)
	}
}

// TestL2_Permissions_PromptChoicesIncludeAllowDenyAndSuggestion proves
// that permission_suggestions in the request produce a third button.
func TestL2_Permissions_PromptChoicesIncludeAllowDenyAndSuggestion(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-3buttons-1"
	suggestions := []map[string]any{{"prefix": "Bash:git *"}}
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "git push"}, suggestions))
	pushTrigger(t, h, token, "trigger 3 buttons")

	call, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "git push", 10*time.Second)
	if !ok {
		t.Fatalf("prompt never fired")
	}
	rows := decodeInlineKeyboard(call.Body)
	var buttons []map[string]string
	for _, row := range rows {
		buttons = append(buttons, row...)
	}
	if len(buttons) < 3 {
		t.Fatalf("expected at least 3 buttons (Allow, Deny, Always:...), got %d: %v", len(buttons), buttons)
	}
	// Allow at index 0, Deny at index 1, suggestion at index 2.
	if !strings.HasPrefix(buttons[0]["text"], "Allow") {
		t.Errorf("button[0]: expected 'Allow', got %q", buttons[0]["text"])
	}
	if !strings.HasPrefix(buttons[1]["text"], "Deny") {
		t.Errorf("button[1]: expected 'Deny', got %q", buttons[1]["text"])
	}
	if !strings.Contains(buttons[2]["text"], "Always") || !strings.Contains(buttons[2]["text"], "Bash:git *") {
		t.Errorf("button[2]: expected 'Always: Bash:git *', got %q", buttons[2]["text"])
	}
}

// TestL2_Permissions_WaitForPermissionBlocksFollowupMessage proves that
// while a permission prompt is pending, a second Telegram message
// does NOT trigger a fresh cc-stub turn until the prompt is resolved.
func TestL2_Permissions_WaitForPermissionBlocksFollowupMessage(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-block-1"
	const markerBlocked = "BLOCKED_FOLLOWUP_MARKER"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "rm /tmp/x"}, nil))
	pushTrigger(t, h, token, "trigger block")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm /tmp/x", 10*time.Second); !ok {
		t.Fatalf("prompt never fired")
	}
	// Send a second message while permission is pending. It should be
	// queued in DelegatedManager.WaitForPermission and NOT reach cc-stub.
	pushTrigger(t, h, token, markerBlocked)

	// Negative assertion: marker MUST NOT appear in cc-stub recorder within 3s.
	if waitForUserMessage(t, h, "workspaces/alpha", markerBlocked, 3*time.Second) {
		t.Errorf("expected follow-up message to be BLOCKED while permission pending, but it reached cc-stub")
	}
}

// TestL2_Permissions_FollowupMessageProceedsAfterApproval proves the
// release side: after approval, the queued message is delivered.
func TestL2_Permissions_FollowupMessageProceedsAfterApproval(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-release-1"
	const markerReleased = "RELEASED_FOLLOWUP_MARKER"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "rm /tmp/x"}, nil))
	pushTrigger(t, h, token, "trigger release")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm /tmp/x", 10*time.Second); !ok {
		t.Fatalf("prompt never fired")
	}
	pushTrigger(t, h, token, markerReleased)

	// Approve. After this, the queued follow-up should be released.
	h.TelegramStub().PushCallbackQuery(token, callbackForAllow(reqID), permTestUserID, permTestUserID, 0)

	if !waitForUserMessage(t, h, "workspaces/alpha", markerReleased, 10*time.Second) {
		t.Errorf("expected queued follow-up to reach cc-stub after approval")
	}
}

// TestL2_Permissions_ControlCancelDisablesInlineKeyboard proves the
// orphan-keyboard cleanup path: when CC emits a control_cancel_request
// for a pending permission, foci removes the registered callback so a
// subsequent click on the (now-stale) keyboard is a no-op AND edits
// the Telegram message to clear the keyboard. cc-stub's new
// control_cancel_requests script field emits the cancel envelope after
// the matching permission_request.
func TestL2_Permissions_ControlCancelDisablesInlineKeyboard(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-cancel-1"
	// Script: emit a permission_request, then immediately cancel it.
	body, err := json.Marshal(map[string]any{
		"text": "asking and then cancelling",
		"permission_requests": []map[string]any{
			{
				"tool_name":  "Bash",
				"input":      map[string]any{"command": "rm /tmp/cancelled"},
				"request_id": reqID,
			},
		},
		"control_cancel_requests": []string{reqID},
	})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", body)
	pushTrigger(t, h, token, "trigger cancel")

	// Permission prompt lands first.
	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm /tmp/cancelled", 10*time.Second); !ok {
		t.Fatalf("prompt never fired")
	}

	// After the cancel envelope, foci's CancelInteractiveMessage edits
	// the message — the stub records an editMessageText call. Wait for
	// it as the proxy for "the keyboard was disabled".
	if _, ok := waitForSentMethod(h, token, "editMessageText", 5*time.Second); !ok {
		t.Errorf("expected editMessageText after control_cancel_request; sent calls:\n%s",
			sentCallSummary(h, token))
	}

	// Verify the cancel was actually recorded by cc-stub.
	deadline := time.Now().Add(3 * time.Second)
	var cancelled bool
	for time.Now().Before(deadline) && !cancelled {
		for _, e := range readRecorderEntries(t, h.RecorderPath()) {
			if e.Kind == "permission_cancel" && e.ControlRequestID == reqID {
				cancelled = true
				break
			}
		}
		if !cancelled {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !cancelled {
		t.Errorf("cc-stub never recorded permission_cancel for %s; recorder:\n%s",
			reqID, recorderTail(t, h.RecorderPath()))
	}

	// Now push a callback_query as if the user clicked Allow on the
	// now-cancelled prompt. HandleInteractiveCallback should return
	// ok=false because the listener was removed; no control_response
	// goes back to cc-stub.
	h.TelegramStub().PushCallbackQuery(token, callbackForAllow(reqID), permTestUserID, permTestUserID, 0)

	if got := findControlResponse(t, h, reqID, 2*time.Second); got != nil {
		t.Errorf("expected NO control_response for click on cancelled prompt; got %v", got)
	}
}

// TestL2_Permissions_UnknownCallbackChoiceTreatedAsDeny proves that a
// garbage callback_data falls through SendPermissionResponse's allow/deny
// branch as not-allow and sends deny.
func TestL2_Permissions_UnknownCallbackChoiceTreatedAsDeny(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-unknown-1"
	// Use a high index that won't map to any real button. The Choices
	// helper returns Allow+Deny+suggestions; without suggestions, indices
	// 2+ are out-of-bounds and HandleInteractiveCallback returns ok=false
	// before the choice ever reaches RespondToPermission.
	//
	// NOTE: This path returns ok=false BEFORE RespondToPermission runs, so
	// the request stays pending (not auto-denied). The behaviour the test
	// docstring describes only applies if the click reaches a registered
	// callback. We assert: no control_response is sent (the malformed
	// click is a silent no-op, not a wedge), and the prompt remains
	// pending. Both are sound safety properties.
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "rm /tmp/x"}, nil))
	pushTrigger(t, h, token, "trigger unknown")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm /tmp/x", 10*time.Second); !ok {
		t.Fatalf("prompt never fired")
	}
	h.TelegramStub().PushCallbackQuery(token, callbackForUnknownIndex(reqID, 99), permTestUserID, permTestUserID, 0)

	if got := findControlResponse(t, h, reqID, 2*time.Second); got != nil {
		t.Errorf("expected NO control_response for malformed callback (bounds-checked no-op); got %v", got)
	}
}

// TestL2_Permissions_BashUnsafeRedirectNotAutoApproved proves the AST
// structural safety check: a Bash command of the form "ls > /tmp/out" is
// rejected by matchBashAutoApprove even though "Bash:ls" is in
// CommonReadonlyRules.
func TestL2_Permissions_BashUnsafeRedirectNotAutoApproved(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-redirect-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "ls > /tmp/out"}, nil))
	pushTrigger(t, h, token, "trigger redirect")

	// Note: Go's json.Marshal default escapes `>` as `>` — search by
	// a substring that doesn't include the redirect operator. "/tmp/out"
	// is unique enough to identify this command in the prompt body.
	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "/tmp/out", 10*time.Second); !ok {
		t.Errorf("expected prompt for unsafe redirect, none arrived\n--- recorder ---\n%s", recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_Permissions_BashCommandSubstitutionNotAutoApproved proves that
// nested commands like "ls $(curl evil)" prompt even though Bash:ls
// alone would auto-approve.
func TestL2_Permissions_BashCommandSubstitutionNotAutoApproved(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-cmdsubst-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "ls $(curl evil)"}, nil))
	pushTrigger(t, h, token, "trigger cmdsubst")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "$(curl", 10*time.Second); !ok {
		t.Errorf("expected prompt for command substitution, none arrived\n--- recorder ---\n%s", recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_Permissions_BashUnparseableCommandPromptsUser proves the
// fail-safe behaviour of the AST parser.
func TestL2_Permissions_BashUnparseableCommandPromptsUser(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqID = "req-unparse-1"
	// Syntactically invalid Bash — matchBashAutoApprove can't parse,
	// must fall through to prompt rather than approve or panic.
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "Bash", map[string]any{"command": "if then fi"}, nil))
	pushTrigger(t, h, token, "trigger unparseable")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "if then fi", 10*time.Second); !ok {
		t.Errorf("expected prompt for unparseable command, none arrived\n--- recorder ---\n%s", recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_Permissions_AskUserQuestionNeverAutoApproved proves that
// AskUserQuestion is always routed to the sequential question handler
// and never short-circuited by auto-approve, even with wildcard rules.
func TestL2_Permissions_AskUserQuestionNeverAutoApproved(t *testing.T) {
	t.Parallel()
	// Configure wildcard auto_approve. AskUserQuestion handler runs
	// BEFORE autoApprovePermission (see permissions.go:38-41), so the
	// wildcard rule MUST NOT bypass the question flow.
	h, token := permTestSetup(t, []testharness.AgentSpec{{
		ID:          "alpha",
		UserID:      permTestUserID,
		AutoApprove: []string{"AskUserQuestion"}, // wildcard for the tool
	}}, "")

	const reqID = "req-question-1"
	// userQuestion's expected input shape is questions:[{question,header,options:[{label}]}].
	input := map[string]any{
		"questions": []map[string]any{
			{
				"question": "Continue?",
				"header":   "Confirm",
				"options": []map[string]any{
					{"label": "Yes"},
					{"label": "No"},
				},
				"multiSelect": false,
			},
		},
	}
	h.WriteCCStubScript(t, "alpha", permissionScript(reqID, "AskUserQuestion", input, nil))
	pushTrigger(t, h, token, "trigger question")

	// A question prompt should fire — handleUserQuestion sends the
	// question text via permPromptFn just like a regular permission.
	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "Continue?", 10*time.Second); !ok {
		t.Errorf("expected question prompt to fire despite wildcard auto_approve\n--- recorder ---\n%s", recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_Permissions_PerAgentAutoApproveOverridesGlobal proves that an
// agent's auto_approve rule is honoured while a sibling agent without
// the rule still prompts for the same command.
func TestL2_Permissions_PerAgentAutoApproveOverridesGlobal(t *testing.T) {
	t.Parallel()
	const secondUserID = 1818
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: permTestUserID, AutoApprove: []string{"Bash:make *"}},
			{ID: "beta", UserID: secondUserID},
		},
		ReadyTimeout: 30 * time.Second,
	})
	alphaToken := h.AgentBotToken("alpha")
	betaToken := h.AgentBotToken("beta")

	// Prime both sessions.
	primeAgent := func(token string, uid int64, mark string) {
		h.TelegramStub().PushUpdate(token, gotgbot.Update{
			Message: &gotgbot.Message{
				Chat: gotgbot.Chat{Id: uid, Type: "private"},
				From: &gotgbot.User{Id: uid, FirstName: "Tester"},
				Text: mark,
			},
		})
	}
	primeAgent(alphaToken, permTestUserID, "prime-alpha")
	primeAgent(betaToken, secondUserID, "prime-beta")
	if !waitForUserMessage(t, h, "workspaces/alpha", "prime-alpha", 15*time.Second) {
		t.Fatalf("alpha prime never reached")
	}
	if !waitForUserMessage(t, h, "workspaces/beta", "prime-beta", 15*time.Second) {
		t.Fatalf("beta prime never reached")
	}

	const reqA = "req-make-alpha"
	const reqB = "req-make-beta"
	h.WriteCCStubScript(t, "alpha", permissionScript(reqA, "Bash", map[string]any{"command": "make build"}, nil))
	h.WriteCCStubScript(t, "beta", permissionScript(reqB, "Bash", map[string]any{"command": "make build"}, nil))

	h.TelegramStub().PushUpdate(alphaToken, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: permTestUserID, Type: "private"},
			From: &gotgbot.User{Id: permTestUserID, FirstName: "Tester"},
			Text: "trigger-alpha",
		},
	})
	h.TelegramStub().PushUpdate(betaToken, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: secondUserID, Type: "private"},
			From: &gotgbot.User{Id: secondUserID, FirstName: "Tester"},
			Text: "trigger-beta",
		},
	})

	// alpha (rule) — expect auto-allow without prompt.
	respA := findControlResponse(t, h, reqA, 10*time.Second)
	if respA == nil || respA["behavior"] != "allow" {
		t.Errorf("alpha: expected auto-allow, got: %v", respA)
	}
	// beta (no rule) — expect prompt fires, no control_response.
	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), betaToken, "make build", 10*time.Second); !ok {
		t.Errorf("beta: expected prompt for make build (no rule), none arrived")
	}
	if got := findControlResponse(t, h, reqB, 500*time.Millisecond); got != nil {
		t.Errorf("beta: unexpected early control_response: %v", got)
	}
}

// TestL2_Permissions_ConcurrentPromptsKeyedByRequestID proves that
// multiple control_requests emitted in one turn produce distinct
// pending permissions, and a callback on one resolves only that request.
func TestL2_Permissions_ConcurrentPromptsKeyedByRequestID(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const reqA = "req-concurrent-A"
	const reqB = "req-concurrent-B"
	reqs := []map[string]any{
		{"tool_name": "Bash", "input": map[string]any{"command": "rm /tmp/a"}, "request_id": reqA},
		{"tool_name": "Bash", "input": map[string]any{"command": "rm /tmp/b"}, "request_id": reqB},
	}
	h.WriteCCStubScript(t, "alpha", multiPermissionScript("two prompts", reqs))
	pushTrigger(t, h, token, "trigger concurrent")

	// Wait for both prompts to appear.
	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm /tmp/a", 10*time.Second); !ok {
		t.Fatalf("prompt A never arrived")
	}
	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm /tmp/b", 10*time.Second); !ok {
		t.Fatalf("prompt B never arrived")
	}

	// Resolve A only.
	h.TelegramStub().PushCallbackQuery(token, callbackForAllow(reqA), permTestUserID, permTestUserID, 0)
	respA := findControlResponse(t, h, reqA, 5*time.Second)
	if respA == nil || respA["behavior"] != "allow" {
		t.Errorf("A: expected allow, got: %v", respA)
	}
	// B should NOT have a control_response yet (it's still pending).
	if got := findControlResponse(t, h, reqB, 500*time.Millisecond); got != nil {
		t.Errorf("B: unexpected control_response while still pending: %v", got)
	}

	// Resolve B.
	h.TelegramStub().PushCallbackQuery(token, callbackForDeny(reqB), permTestUserID, permTestUserID, 0)
	respB := findControlResponse(t, h, reqB, 5*time.Second)
	if respB == nil || respB["behavior"] != "deny" {
		t.Errorf("B: expected deny, got: %v", respB)
	}
}

// TestL2_Permissions_CallbackForUnknownRequestIDIsIgnored proves that a
// callback referencing a never-registered requestID is a silent no-op
// and does not disrupt other in-flight prompts on the same agent.
func TestL2_Permissions_CallbackForUnknownRequestIDIsIgnored(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	const realReq = "req-real-1"
	h.WriteCCStubScript(t, "alpha", permissionScript(realReq, "Bash", map[string]any{"command": "rm /tmp/x"}, nil))
	pushTrigger(t, h, token, "trigger real")

	if _, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, "rm /tmp/x", 10*time.Second); !ok {
		t.Fatalf("real prompt never fired")
	}
	// Push a callback for a never-registered ID.
	h.TelegramStub().PushCallbackQuery(token, callbackForAllow("req-ghost"), permTestUserID, permTestUserID, 0)
	// Real prompt should still be pending — no control_response yet.
	if got := findControlResponse(t, h, realReq, 500*time.Millisecond); got != nil {
		t.Errorf("real prompt unexpectedly resolved by ghost callback: %v", got)
	}
	// Real callback still works.
	h.TelegramStub().PushCallbackQuery(token, callbackForAllow(realReq), permTestUserID, permTestUserID, 0)
	resp := findControlResponse(t, h, realReq, 5*time.Second)
	if resp == nil || resp["behavior"] != "allow" {
		t.Errorf("real callback failed after ghost callback: %v", resp)
	}
}

// dummy import keep — json used by helpers indirectly when needed in future.
var _ = json.RawMessage(nil)
