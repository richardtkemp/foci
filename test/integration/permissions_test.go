//go:build integration

package integration

import (
	"testing"
	"time"

	"foci/internal/testharness"
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

// TestL2_Permissions_AutoApproveCommonReadonlySkipsPrompt proves that a
// can_use_tool request for a tool covered by CommonReadonlyRules (e.g.
// Read, Glob, Bash:ls) never produces a Telegram interactive prompt and
// instead receives an immediate stdio control_response with
// behavior="allow". Assertion: TelegramStub records zero sendMessage
// calls carrying an inline keyboard, and cc-stub observes the allow
// response before the next user message turn.
func TestL2_Permissions_AutoApproveCommonReadonlySkipsPrompt(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_AutoApproveUserRuleSkipsPrompt proves that a
// user-configured auto_approve entry in foci.toml (e.g. "Bash:git *")
// is honoured by ccstream.matchAutoApprove and short-circuits the
// prompt. Distinct from CommonReadonly because the rule travels the
// AgentLevel + GlobalLevel union path in resolved_types.go rather than
// the built-in list.
func TestL2_Permissions_AutoApproveUserRuleSkipsPrompt(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_AutoApproveCommonSafeWriteDisabledByDefault proves
// that with AutoApproveCommonSafeWrite left at its default false, a
// Bash:curl request DOES surface to Telegram as an interactive prompt
// even though it's in the CommonSafeWriteRules list. Pairs with the
// next test to lock the opt-in semantic.
func TestL2_Permissions_AutoApproveCommonSafeWriteDisabledByDefault(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_AutoApproveCommonSafeWriteEnabledSkipsPrompt proves
// that toggling auto_approve_common_safe_write=true in foci.toml causes
// the same Bash:curl request to be auto-approved without a prompt.
// Together with the previous test this proves the config flag actually
// flips behaviour at the L2 boundary, not just inside the matcher unit.
func TestL2_Permissions_AutoApproveCommonSafeWriteEnabledSkipsPrompt(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_FociShellToolsAutoApproved proves that the
// dynamically derived FociShellRulesFor(execRegistry.ExportedNames())
// covers every foci_* shell wrapper at runtime — a Bash:foci_todo
// request is auto-approved without a prompt. This catches drift if a
// new foci_* tool is added without being registered as ExecExport.
func TestL2_Permissions_FociShellToolsAutoApproved(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_BashOutsideAllowlistPromptsUser proves the negative
// case: a Bash command not covered by any allowlist (e.g.
// "rm -rf /tmp/x") surfaces to Telegram as a sendMessage with an inline
// keyboard containing Allow / Deny buttons, AND the cc-stub does NOT
// see an early control_response. Confirms the prompt path actually
// fires when auto-approve misses.
func TestL2_Permissions_BashOutsideAllowlistPromptsUser(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_SudoCommandPromptsUser proves that `sudo cmd` is
// always promoted to a prompt regardless of the embedded command (sudo
// is the canonical user-confirmation surface). The test scripts cc-stub
// to emit a can_use_tool for "sudo apt-get update" and asserts the
// prompt body contains the command in a fenced code block.
func TestL2_Permissions_SudoCommandPromptsUser(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_ApprovalCallbackUnblocksTool proves the happy path
// for the user-driven approval: foci prompts, the test pushes a
// callback_query with data="allow", and cc-stub then receives a
// control_response with behavior="allow" and updated_input set. The
// assertion uses cc-stub's recorder to confirm the tool_use moved past
// the gate.
func TestL2_Permissions_ApprovalCallbackUnblocksTool(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_DenialCallbackReturnsDeny proves the negative-path
// callback: pushing data="deny" causes RespondToPermission to send
// behavior="deny" with a non-empty message field. cc-stub's recorder
// captures the deny response and the test asserts the tool was NOT
// executed (no Bash side-effect, no follow-up tool_use turn).
func TestL2_Permissions_DenialCallbackReturnsDeny(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_AllowAlwaysAddsSessionRule proves the
// "Always: <prefix>" choice path: pushing data="allow_always:Bash:git *"
// causes RespondToPermissionWithRule to send an allow response with a
// non-empty updated_permissions array. The test then issues a second,
// distinct git command and asserts NO second prompt fires — proving
// the rule was registered for the session.
func TestL2_Permissions_AllowAlwaysAddsSessionRule(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_PromptContainsCommandInFencedBlock proves the UX
// contract on the outbound Telegram message: formatToolInput renders
// the Bash command inside a triple-backtick fenced code block, never
// as inline code. Regression net for the bug where internal backticks
// in grep patterns broke single-backtick rendering. Asserts on the
// sendMessage body containing "```\n<cmd>\n```".
func TestL2_Permissions_PromptContainsCommandInFencedBlock(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_PromptChoicesIncludeAllowDenyAndSuggestion proves
// the inline keyboard layout: when CC's payload carries
// permission_suggestions, the Choices() helper emits Allow + Deny +
// one "Always: <prefix>" button per suggestion. Asserts the recorded
// sendMessage's reply_markup contains all three callback_data values.
func TestL2_Permissions_PromptChoicesIncludeAllowDenyAndSuggestion(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_WaitForPermissionBlocksFollowupMessage proves the
// blocking semantics of DelegatedManager.WaitForPermission: while a
// permission prompt is pending, a second Telegram message to the same
// agent does NOT trigger a fresh cc-stub turn until the prompt is
// resolved. Regression net for the bug where pending prompts let
// follow-up messages race past the gate and corrupted session state.
func TestL2_Permissions_WaitForPermissionBlocksFollowupMessage(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_FollowupMessageProceedsAfterApproval proves the
// release side of the blocking gate: after the user approves and
// SetPermissionPending(false) fires, the queued follow-up message
// proceeds and is delivered to cc-stub. Pair with the previous test —
// together they prove block + release, not just one side.
func TestL2_Permissions_FollowupMessageProceedsAfterApproval(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_ControlCancelDisablesInlineKeyboard proves the
// orphan-keyboard path: if CC sends a control_cancel_request for an
// outstanding permission (typically because a follow-up user message
// interrupted the in-flight tool), the registered prompt-cancel
// listener edits the Telegram message to remove the keyboard and
// shows a "cancelled by follow-up message" marker. Asserts on a
// recorded editMessageText call carrying the cancellation text.
func TestL2_Permissions_ControlCancelDisablesInlineKeyboard(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_UnknownCallbackChoiceTreatedAsDeny proves the
// malformed-input safety net: pushing a callback_query with a garbage
// data field (e.g. "asdf") falls through SendPermissionResponse's
// allow/deny logic as a non-allow choice and therefore sends
// behavior="deny" rather than wedging the session. Asserts cc-stub
// observed a deny response.
func TestL2_Permissions_UnknownCallbackChoiceTreatedAsDeny(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_BashUnsafeRedirectNotAutoApproved proves the
// AST-level structural safety check: a Bash command of the form
// "ls > /tmp/out" is rejected by matchBashAutoApprove even though
// "Bash:ls" is in CommonReadonlyRules — output redirects to non-
// /dev/null targets must always prompt. Asserts a Telegram prompt is
// recorded for the redirected form.
func TestL2_Permissions_BashUnsafeRedirectNotAutoApproved(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_BashCommandSubstitutionNotAutoApproved proves the
// nested-command structural check: "ls $(curl evil)" prompts even
// though "Bash:ls" alone would auto-approve, because the inner CmdSubst
// runs an unapproved command. Regression net for shell-feature-based
// allowlist bypass.
func TestL2_Permissions_BashCommandSubstitutionNotAutoApproved(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_BashUnparseableCommandPromptsUser proves the
// fail-safe behaviour of the AST parser: a syntactically invalid Bash
// command (e.g. "if then fi") fails matchBashAutoApprove and falls
// through to the user prompt rather than silently approving or
// crashing the gateway.
func TestL2_Permissions_BashUnparseableCommandPromptsUser(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_AskUserQuestionNeverAutoApproved proves that the
// AskUserQuestion tool — which IS a user-interaction surface, not a
// side-effect surface — always goes through handleUserQuestion's
// sequential prompt flow and is never short-circuited by auto-approve
// rules, even if a "*" wildcard rule were configured. Asserts a
// question prompt sendMessage fires.
func TestL2_Permissions_AskUserQuestionNeverAutoApproved(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_PerAgentAutoApproveOverridesGlobal proves the
// config-resolution layer at the integration boundary: agent A
// configured with auto_approve = ["Bash:make *"] auto-approves "make
// build" while agent B with no rules prompts for the same command,
// even when both share the same global config block. Catches drift
// between resolved_types.go's union logic and ccstream's consumer.
func TestL2_Permissions_PerAgentAutoApproveOverridesGlobal(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_ConcurrentPromptsKeyedByRequestID proves the
// multi-prompt case: when cc-stub emits two can_use_tool requests
// back-to-back before either is answered, foci surfaces TWO distinct
// Telegram messages with distinct callback_data prefixes, and a
// callback on one resolves only that request. Asserts pendingPerms
// transitions 0→2→1→0 as the test answers each in turn.
func TestL2_Permissions_ConcurrentPromptsKeyedByRequestID(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}

// TestL2_Permissions_CallbackForUnknownRequestIDIsIgnored proves the
// stale-callback safety net: a callback_query referencing a requestID
// that no longer exists in pendingPerms (e.g. user clicks an old
// message after the prompt was resolved) returns the expected "no
// pending permission" error from RespondToPermission and does NOT
// disrupt other in-flight prompts on the same agent.
func TestL2_Permissions_CallbackForUnknownRequestIDIsIgnored(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
}
