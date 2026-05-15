//go:build integration

package integration

import (
	"testing"
)

// The permissions test cluster exercises foci's approval flow end-to-end:
// CC emits a can_use_tool control_request; foci either auto-approves it
// against the merged rule set, or surfaces it to Telegram as an inline
// keyboard; the user's callback_query dispatches RespondToPermission
// back through the stdio control_response. Each test below names one
// observable in that pipeline.
//
// All cases here require extending cc-stub with a "scripted can_use_tool"
// mode — currently cc-stub emits tool_use blocks but never asks foci for
// permission. The harness side already plumbs PermissionPromptFunc and
// SendPermissionResponse end-to-end against a stubbed Telegram, so the
// remaining work is the cc-stub control_request half + the callback_query
// stub on the Telegram side.
//
// ---------------------------------------------------------------------------
// IMPLEMENTATION NOTE (first-draft pass)
// ---------------------------------------------------------------------------
// On inspection, every test in this file is gated on extensions the
// rules-of-engagement for this pass forbid us from making (the prompt
// explicitly bans edits to internal/testharness/ and cmd/cc-stub/). The
// concrete gaps:
//
//   1. cc-stub cannot emit a `can_use_tool` control_request. Its main loop
//      handles only "user" envelopes (replying with assistant+result) and
//      "control_request" envelopes (acks them). It has no path that writes
//      a {"type":"control_request","request":{"subtype":"can_use_tool",
//      "tool_name":"...","input":{...}}} envelope onto stdout, and no
//      script field that would let a test author one. Without that, foci's
//      ccstream reader never calls handleToolRequest, so no auto-approve
//      check, no prompt, no pending permission, nothing to observe.
//
//   2. The harness has no helper to push a callback_query update. The
//      TelegramStub.PushUpdate signature does accept any gotgbot.Update,
//      so callback_query *could* be pushed today, but with (1) unaddressed
//      there is never a pending permission for the callback to target.
//
//   3. writeTestConfig does not surface auto_approve / auto_approve_common_*
//      flags. Tests that want to flip those (AutoApproveUserRuleSkipsPrompt,
//      AutoApproveCommonSafeWriteEnabledSkipsPrompt, PerAgentAutoApprove)
//      cannot configure them through the public HarnessOptions surface.
//
// Each test below is therefore tagged with t.Skip("HARNESS GAP: ...")
// naming the smallest extension that would unblock it. The intent is that
// a follow-up pass against testharness/ + cc-stub/ implements these
// extensions; the test bodies can then be filled in without further edits
// to the spec comments.

// TestL2_Permissions_AutoApproveCommonReadonlySkipsPrompt proves that a
// can_use_tool request for a tool covered by CommonReadonlyRules (e.g.
// Read, Glob, Bash:ls) never produces a Telegram interactive prompt and
// instead receives an immediate stdio control_response with
// behavior="allow". Assertion: TelegramStub records zero sendMessage
// calls carrying an inline keyboard, and cc-stub observes the allow
// response before the next user message turn.
func TestL2_Permissions_AutoApproveCommonReadonlySkipsPrompt(t *testing.T) {
	t.Skip("HARNESS GAP: cc-stub needs a scripted can_use_tool mode (emit a control_request with subtype=can_use_tool from a stubScript entry) and must record the inbound control_response so the test can assert behavior=\"allow\" was received without a Telegram prompt being recorded")
}

// TestL2_Permissions_AutoApproveUserRuleSkipsPrompt proves that a
// user-configured auto_approve entry in foci.toml (e.g. "Bash:git *")
// is honoured by ccstream.matchAutoApprove and short-circuits the
// prompt. Distinct from CommonReadonly because the rule travels the
// AgentLevel + GlobalLevel union path in resolved_types.go rather than
// the built-in list.
func TestL2_Permissions_AutoApproveUserRuleSkipsPrompt(t *testing.T) {
	t.Skip("HARNESS GAP: needs (a) the scripted can_use_tool cc-stub extension and (b) writeTestConfig to accept per-agent auto_approve rules (currently the generated foci.toml omits the auto_approve key entirely) so a test can declare auto_approve = [\"Bash:git *\"] on the test agent")
}

// TestL2_Permissions_AutoApproveCommonSafeWriteDisabledByDefault proves
// that with AutoApproveCommonSafeWrite left at its default false, a
// Bash:curl request DOES surface to Telegram as an interactive prompt
// even though it's in the CommonSafeWriteRules list. Pairs with the
// next test to lock the opt-in semantic.
func TestL2_Permissions_AutoApproveCommonSafeWriteDisabledByDefault(t *testing.T) {
	t.Skip("HARNESS GAP: needs (a) cc-stub scripted can_use_tool support so a Bash:curl request can be injected and (b) a TelegramStub assertion helper that inspects the recorded sendMessage body for reply_markup.inline_keyboard (today PeekSent returns the raw JSON form but tests have no shared helper to decode the keyboard layer)")
}

// TestL2_Permissions_AutoApproveCommonSafeWriteEnabledSkipsPrompt proves
// that toggling auto_approve_common_safe_write=true in foci.toml causes
// the same Bash:curl request to be auto-approved without a prompt.
// Together with the previous test this proves the config flag actually
// flips behaviour at the L2 boundary, not just inside the matcher unit.
func TestL2_Permissions_AutoApproveCommonSafeWriteEnabledSkipsPrompt(t *testing.T) {
	t.Skip("HARNESS GAP: needs (a) cc-stub scripted can_use_tool support and (b) writeTestConfig to surface auto_approve_common_safe_write so a test can set it true on a per-agent basis. Same gap as the previous test plus the config-toggle plumbing")
}

// TestL2_Permissions_FociShellToolsAutoApproved proves that the
// dynamically derived FociShellRulesFor(execRegistry.ExportedNames())
// covers every foci_* shell wrapper at runtime — a Bash:foci_todo
// request is auto-approved without a prompt. This catches drift if a
// new foci_* tool is added without being registered as ExecExport.
func TestL2_Permissions_FociShellToolsAutoApproved(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support to inject a Bash:foci_todo request and observe the immediate control_response. The current cc-stub Bash path runs the command directly without ever asking foci for permission, which is the opposite of what this test needs to prove")
}

// TestL2_Permissions_BashOutsideAllowlistPromptsUser proves the negative
// case: a Bash command not covered by any allowlist (e.g.
// "rm -rf /tmp/x") surfaces to Telegram as a sendMessage with an inline
// keyboard containing Allow / Deny buttons, AND the cc-stub does NOT
// see an early control_response. Confirms the prompt path actually
// fires when auto-approve misses.
func TestL2_Permissions_BashOutsideAllowlistPromptsUser(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support (to inject the Bash:rm request) plus a TelegramStub helper to assert on inline_keyboard contents of a recorded sendMessage. Also needs cc-stub to record received control_responses so we can assert the absence of an early allow")
}

// TestL2_Permissions_SudoCommandPromptsUser proves that `sudo cmd` is
// always promoted to a prompt regardless of the embedded command (sudo
// is the canonical user-confirmation surface). The test scripts cc-stub
// to emit a can_use_tool for "sudo apt-get update" and asserts the
// prompt body contains the command in a fenced code block.
func TestL2_Permissions_SudoCommandPromptsUser(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support with a `command` input field (so the test can inject \"sudo apt-get update\") plus a TelegramStub helper that exposes the decoded `text` field of the recorded sendMessage for fenced-block assertion")
}

// TestL2_Permissions_ApprovalCallbackUnblocksTool proves the happy path
// for the user-driven approval: foci prompts, the test pushes a
// callback_query with data="allow", and cc-stub then receives a
// control_response with behavior="allow" and updated_input set. The
// assertion uses cc-stub's recorder to confirm the tool_use moved past
// the gate.
func TestL2_Permissions_ApprovalCallbackUnblocksTool(t *testing.T) {
	t.Skip("HARNESS GAP: needs (a) cc-stub scripted can_use_tool support, (b) cc-stub recorder to capture inbound control_responses (currently it records only invocation/user_message; there is no record of what foci sent back as an allow/deny), and (c) a harness helper to push a callback_query Update with given callback_data targeting an outstanding prompt's request_id")
}

// TestL2_Permissions_DenialCallbackReturnsDeny proves the negative-path
// callback: pushing data="deny" causes RespondToPermission to send
// behavior="deny" with a non-empty message field. cc-stub's recorder
// captures the deny response and the test asserts the tool was NOT
// executed (no Bash side-effect, no follow-up tool_use turn).
func TestL2_Permissions_DenialCallbackReturnsDeny(t *testing.T) {
	t.Skip("HARNESS GAP: needs the same trio as ApprovalCallbackUnblocksTool (scripted can_use_tool, callback_query push helper, cc-stub recording of inbound control_responses) so the test can assert the recorded response had behavior=\"deny\" and message field set")
}

// TestL2_Permissions_AllowAlwaysAddsSessionRule proves the
// "Always: <prefix>" choice path: pushing data="allow_always:Bash:git *"
// causes RespondToPermissionWithRule to send an allow response with a
// non-empty updated_permissions array. The test then issues a second,
// distinct git command and asserts NO second prompt fires — proving
// the rule was registered for the session.
func TestL2_Permissions_AllowAlwaysAddsSessionRule(t *testing.T) {
	t.Skip("HARNESS GAP: needs (a) cc-stub scripted can_use_tool support that can be re-armed for a second turn with a different command, (b) callback_query push helper, and (c) cc-stub recording of inbound control_responses so the test can assert updated_permissions[0].prefix=\"Bash:git *\" was returned")
}

// TestL2_Permissions_PromptContainsCommandInFencedBlock proves the UX
// contract on the outbound Telegram message: formatToolInput renders
// the Bash command inside a triple-backtick fenced code block, never
// as inline code. Regression net for the bug where internal backticks
// in grep patterns broke single-backtick rendering. Asserts on the
// sendMessage body containing "```\n<cmd>\n```".
func TestL2_Permissions_PromptContainsCommandInFencedBlock(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support so a prompt fires, then can assert on the recorded sendMessage's `text` field via existing PeekSent. Could be implemented purely from the cc-stub side — no additional TelegramStub helper required — but the can_use_tool emission gap is the blocker")
}

// TestL2_Permissions_PromptChoicesIncludeAllowDenyAndSuggestion proves
// the inline keyboard layout: when CC's payload carries
// permission_suggestions, the Choices() helper emits Allow + Deny +
// one "Always: <prefix>" button per suggestion. Asserts the recorded
// sendMessage's reply_markup contains all three callback_data values.
func TestL2_Permissions_PromptChoicesIncludeAllowDenyAndSuggestion(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support that includes a permission_suggestions field in the request payload (so foci.Choices() emits the Always: button) plus a helper or shared parse path for decoding reply_markup.inline_keyboard from a recorded sendMessage body")
}

// TestL2_Permissions_WaitForPermissionBlocksFollowupMessage proves the
// blocking semantics of DelegatedManager.WaitForPermission: while a
// permission prompt is pending, a second Telegram message to the same
// agent does NOT trigger a fresh cc-stub turn until the prompt is
// resolved. Regression net for the bug where pending prompts let
// follow-up messages race past the gate and corrupted session state.
func TestL2_Permissions_WaitForPermissionBlocksFollowupMessage(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support to create a pending permission, and a way to keep that permission pending while a second user message is pushed. The negative-side assertion (no second user_message recorder entry within timeout) is straightforward once the prompt can actually fire")
}

// TestL2_Permissions_FollowupMessageProceedsAfterApproval proves the
// release side of the blocking gate: after the user approves and
// SetPermissionPending(false) fires, the queued follow-up message
// proceeds and is delivered to cc-stub. Pair with the previous test —
// together they prove block + release, not just one side.
func TestL2_Permissions_FollowupMessageProceedsAfterApproval(t *testing.T) {
	t.Skip("HARNESS GAP: needs the same trio as ApprovalCallbackUnblocksTool plus the WaitForPermission-block-then-release sequence. Specifically, after approve-callback is pushed the test must observe a NEW user_message recorder entry for the previously-queued message, which requires cc-stub-scripted can_use_tool plus callback_query push")
}

// TestL2_Permissions_ControlCancelDisablesInlineKeyboard proves the
// orphan-keyboard path: if CC sends a control_cancel_request for an
// outstanding permission (typically because a follow-up user message
// interrupted the in-flight tool), the registered prompt-cancel
// listener edits the Telegram message to remove the keyboard and
// shows a "cancelled by follow-up message" marker. Asserts on a
// recorded editMessageText call carrying the cancellation text.
func TestL2_Permissions_ControlCancelDisablesInlineKeyboard(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub to be able to emit BOTH a can_use_tool request AND a subsequent control_cancel_request (envelope shape: {\"type\":\"control_cancel_request\",\"request_id\":\"...\"}) referencing the same request_id. The TelegramStub already records editMessageText calls so the assertion side is ready")
}

// TestL2_Permissions_UnknownCallbackChoiceTreatedAsDeny proves the
// malformed-input safety net: pushing a callback_query with a garbage
// data field (e.g. "asdf") falls through SendPermissionResponse's
// allow/deny logic as a non-allow choice and therefore sends
// behavior="deny" rather than wedging the session. Asserts cc-stub
// observed a deny response.
func TestL2_Permissions_UnknownCallbackChoiceTreatedAsDeny(t *testing.T) {
	t.Skip("HARNESS GAP: needs (a) cc-stub scripted can_use_tool support, (b) callback_query push helper that accepts arbitrary data strings, and (c) cc-stub recording of inbound control_responses to verify the deny was sent")
}

// TestL2_Permissions_BashUnsafeRedirectNotAutoApproved proves the
// AST-level structural safety check: a Bash command of the form
// "ls > /tmp/out" is rejected by matchBashAutoApprove even though
// "Bash:ls" is in CommonReadonlyRules — output redirects to non-
// /dev/null targets must always prompt. Asserts a Telegram prompt is
// recorded for the redirected form.
func TestL2_Permissions_BashUnsafeRedirectNotAutoApproved(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support with a command-field input. Once a Bash request with command=\"ls > /tmp/out\" can be injected, the existing TelegramStub PeekSent can assert that a sendMessage with reply_markup was recorded for the prompt")
}

// TestL2_Permissions_BashCommandSubstitutionNotAutoApproved proves the
// nested-command structural check: "ls $(curl evil)" prompts even
// though "Bash:ls" alone would auto-approve, because the inner CmdSubst
// runs an unapproved command. Regression net for shell-feature-based
// allowlist bypass.
func TestL2_Permissions_BashCommandSubstitutionNotAutoApproved(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support with a command field so command=\"ls $(curl evil)\" can be injected. Same shape as BashUnsafeRedirectNotAutoApproved")
}

// TestL2_Permissions_BashUnparseableCommandPromptsUser proves the
// fail-safe behaviour of the AST parser: a syntactically invalid Bash
// command (e.g. "if then fi") fails matchBashAutoApprove and falls
// through to the user prompt rather than silently approving or
// crashing the gateway.
func TestL2_Permissions_BashUnparseableCommandPromptsUser(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support with a command field. Asserts the foci-gw process is still alive and a sendMessage prompt was recorded — neither half is buildable today without the cc-stub extension")
}

// TestL2_Permissions_AskUserQuestionNeverAutoApproved proves that the
// AskUserQuestion tool — which IS a user-interaction surface, not a
// side-effect surface — always goes through handleUserQuestion's
// sequential prompt flow and is never short-circuited by auto-approve
// rules, even if a "*" wildcard rule were configured. Asserts a
// question prompt sendMessage fires.
func TestL2_Permissions_AskUserQuestionNeverAutoApproved(t *testing.T) {
	t.Skip("HARNESS GAP: needs (a) cc-stub scripted can_use_tool support with tool_name=\"AskUserQuestion\" and a questions[] input field that matches userQuestion's expected shape, plus (b) writeTestConfig to optionally inject a wildcard auto_approve rule so the negative assertion (\"auto_approve didn't bypass the question flow\") has teeth")
}

// TestL2_Permissions_PerAgentAutoApproveOverridesGlobal proves the
// config-resolution layer at the integration boundary: agent A
// configured with auto_approve = ["Bash:make *"] auto-approves "make
// build" while agent B with no rules prompts for the same command,
// even when both share the same global config block. Catches drift
// between resolved_types.go's union logic and ccstream's consumer.
func TestL2_Permissions_PerAgentAutoApproveOverridesGlobal(t *testing.T) {
	t.Skip("HARNESS GAP: needs writeTestConfig to accept per-agent auto_approve rules (so agent A gets [\"Bash:make *\"] and agent B gets nothing) AND cc-stub scripted can_use_tool support to inject the same Bash:make request into each agent's session. Today neither half is available")
}

// TestL2_Permissions_ConcurrentPromptsKeyedByRequestID proves the
// multi-prompt case: when cc-stub emits two can_use_tool requests
// back-to-back before either is answered, foci surfaces TWO distinct
// Telegram messages with distinct callback_data prefixes, and a
// callback on one resolves only that request. Asserts pendingPerms
// transitions 0→2→1→0 as the test answers each in turn.
func TestL2_Permissions_ConcurrentPromptsKeyedByRequestID(t *testing.T) {
	t.Skip("HARNESS GAP: needs cc-stub scripted can_use_tool support that can emit MULTIPLE control_requests in a single turn (the current stubScript schema supports multiple tool_uses, but not multiple permission requests), plus callback_query push helper and a Harness accessor exposing the live Backend.PendingPermissions() count for the 0→2→1→0 assertion")
}

// TestL2_Permissions_CallbackForUnknownRequestIDIsIgnored proves the
// stale-callback safety net: a callback_query referencing a requestID
// that no longer exists in pendingPerms (e.g. user clicks an old
// message after the prompt was resolved) returns the expected "no
// pending permission" error from RespondToPermission and does NOT
// disrupt other in-flight prompts on the same agent.
func TestL2_Permissions_CallbackForUnknownRequestIDIsIgnored(t *testing.T) {
	t.Skip("HARNESS GAP: needs (a) callback_query push helper that targets a synthetic, never-registered request_id, and (b) a way to observe the resulting error path — either via foci-gw stderr scanning for the \"no pending permission\" log line, or via a recorder/registry accessor. Also needs cc-stub scripted can_use_tool support to set up the \"other in-flight prompts\" leg of the assertion")
}
