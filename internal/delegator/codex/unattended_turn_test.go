package codex

import (
	"bytes"
	"testing"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
)

// openUnattendedTurn arms the turn-state a system-triggered turn would
// install: openTurn with TurnEvents{Trigger, Unattended: true} (exactly what
// beginTurn installs from the agent layer's buildTurnEvents for a non-user
// trigger).
func openUnattendedTurn(b *Backend, trigger string) {
	openTurn(b, &delegator.TurnEvents{Trigger: trigger, Unattended: true})
}

// TestUnattendedTurn_CommandApprovalAcceptedWithoutPrompt: a command approval
// arriving while a system-triggered turn (reflection) is in flight is
// accepted on the wire — no keyboard raised, nothing left pending. The
// alternative blocked the turn forever on a chat nobody was watching
// (incident 2026-10-27).
func TestUnattendedTurn_CommandApprovalAcceptedWithoutPrompt(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	prompted := ""
	b.permPromptFn = func(itemID, _, _, _ string, _ []delegator.PromptChoice) { prompted = itemID }

	openUnattendedTurn(b, "reflection")
	b.onCommandApproval([]byte(`{"itemId":"item_uc","threadId":"th_1","turnId":"tn_1","reason":"dangerous","command":"rm -rf /tmp/x","cwd":"/tmp"}`), 51)

	if prompted != "" {
		t.Errorf("user prompted (%q) during an unattended system turn; want no prompt", prompted)
	}
	if id, decision := parseApprovalResponse(t, buf.String()); id != 51 || decision != "accept" {
		t.Errorf("response = (%d, %q), want (51, accept)", id, decision)
	}
	b.permMu.Lock()
	n := len(b.pendingPerms)
	b.permMu.Unlock()
	if n != 0 {
		t.Errorf("pendingPerms has %d entries, want 0", n)
	}
}

// TestUnattendedTurn_FileChangeApprovalAcceptedWithoutPrompt: ditto for a
// fileChange approval. NOTE (owner's decision): this means an unattended
// system turn CAN write files.
func TestUnattendedTurn_FileChangeApprovalAcceptedWithoutPrompt(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	prompted := ""
	b.permPromptFn = func(itemID, _, _, _ string, _ []delegator.PromptChoice) { prompted = itemID }

	openUnattendedTurn(b, "consolidation")
	b.onFileChangeApproval([]byte(`{"itemId":"item_uf","threadId":"th_1","turnId":"tn_1","reason":"writing memory"}`), 52)

	if prompted != "" {
		t.Errorf("user prompted (%q) during an unattended system turn; want no prompt", prompted)
	}
	if id, decision := parseApprovalResponse(t, buf.String()); id != 52 || decision != "accept" {
		t.Errorf("response = (%d, %q), want (52, accept)", id, decision)
	}
	b.permMu.Lock()
	n := len(b.pendingPerms)
	b.permMu.Unlock()
	if n != 0 {
		t.Errorf("pendingPerms has %d entries, want 0", n)
	}
}

// TestUnattendedTurn_AutoApproveStillRunsFirst: tryAutoApprove keeps priority
// during a system turn — a matching rule accepts exactly as before (same wire
// result), and a NON-matching command falls through to the unattended accept
// rather than a prompt.
func TestUnattendedTurn_AutoApproveStillRunsFirst(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	b.autoApproveRules = autoapprove.Compile([]string{"Bash:ls"})
	prompted := ""
	b.permPromptFn = func(itemID, _, _, _ string, _ []delegator.PromptChoice) { prompted = itemID }

	openUnattendedTurn(b, "keepalive")
	b.onCommandApproval([]byte(`{"itemId":"item_uls","command":"ls -la","cwd":"/tmp"}`), 53)
	if prompted != "" {
		t.Errorf("prompted (%q) for an allowlisted command during a system turn", prompted)
	}
	if id, decision := parseApprovalResponse(t, buf.String()); id != 53 || decision != "accept" {
		t.Errorf("auto-approved response = (%d, %q), want (53, accept)", id, decision)
	}

	buf.Reset()
	b.onCommandApproval([]byte(`{"itemId":"item_urm","command":"rm -rf /tmp/x","cwd":"/tmp"}`), 54)
	if prompted != "" {
		t.Errorf("prompted (%q) for a non-matching command during a system turn; want unattended accept", prompted)
	}
	if id, decision := parseApprovalResponse(t, buf.String()); id != 54 || decision != "accept" {
		t.Errorf("unattended response = (%d, %q), want (54, accept)", id, decision)
	}
}

// TestUnattendedTurn_PermissionApprovalStillDenies: onPermissionApproval
// already auto-denies unconditionally; a system turn must not change that.
func TestUnattendedTurn_PermissionApprovalStillDenies(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)

	openUnattendedTurn(b, "reflection")
	b.onPermissionApproval([]byte(`{}`), 55)

	if id, decision := parseApprovalResponse(t, buf.String()); id != 55 || decision != "decline" {
		t.Errorf("response = (%d, %q), want (55, decline)", id, decision)
	}
}

// TestUnattendedTurn_UserTurnStillPrompts is the regression guard: the SAME
// backend, a turn that is NOT system-triggered — the approvals surface via
// permPromptFn exactly as before, and nothing is answered on the wire.
func TestUnattendedTurn_UserTurnStillPrompts(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	var promptedCmd, promptedFC string
	b.permPromptFn = func(itemID, _, _, _ string, _ []delegator.PromptChoice) {
		switch itemID {
		case "item_a":
			promptedCmd = itemID
		case "item_b":
			promptedFC = itemID
		}
	}

	openTurn(b, &delegator.TurnEvents{Trigger: "telegram"})
	b.onCommandApproval([]byte(`{"itemId":"item_a","threadId":"th_1","turnId":"tn_1","command":"rm -rf /tmp/x","cwd":"/tmp"}`), 61)
	b.onFileChangeApproval([]byte(`{"itemId":"item_b","threadId":"th_1","turnId":"tn_1","reason":"editing config"}`), 62)

	if promptedCmd != "item_a" {
		t.Errorf("command approval during user turn: prompted = %q, want item_a", promptedCmd)
	}
	if promptedFC != "item_b" {
		t.Errorf("fileChange approval during user turn: prompted = %q, want item_b", promptedFC)
	}
	if buf.Len() != 0 {
		t.Errorf("no wire output expected while prompting, got %q", buf.String())
	}
	b.permMu.Lock()
	n := len(b.pendingPerms)
	b.permMu.Unlock()
	if n != 2 {
		t.Errorf("pendingPerms has %d entries, want 2 (both prompts pending)", n)
	}
}

// TestUnattendedTurn_ApprovalAfterTurnEndStillPrompts: the flag is
// turn-scoped — once the system turn completes (completeTurn), approvals on
// the same backend prompt again.
func TestUnattendedTurn_ApprovalAfterTurnEndStillPrompts(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newPermTestBackend(&buf)
	prompted := ""
	b.permPromptFn = func(itemID, _, _, _ string, _ []delegator.PromptChoice) { prompted = itemID }

	openUnattendedTurn(b, "reflection")
	b.completeTurn(&delegator.TurnResult{Text: "done"})

	b.onCommandApproval([]byte(`{"itemId":"item_ae","threadId":"th_1","turnId":"tn_1","command":"ls","cwd":"/tmp"}`), 63)
	if prompted != "item_ae" {
		t.Errorf("prompted = %q after system turn completed, want item_ae", prompted)
	}
	if buf.Len() != 0 {
		t.Errorf("no wire output expected while prompting, got %q", buf.String())
	}
}
