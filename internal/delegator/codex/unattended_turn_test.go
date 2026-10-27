package codex

import (
	"bytes"
	"strconv"
	"testing"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
)

// openUnattendedTurn arms the turn-state a system-triggered turn would
// install: openTurn with TurnEvents{Trigger, Unattended: true} (exactly what
// beginTurn installs from the agent layer's buildTurnEvents for an unattended
// trigger).
func openUnattendedTurn(b *Backend, trigger string) {
	openTurn(b, &delegator.TurnEvents{Trigger: trigger, Unattended: true})
}

// assertNothingPending fails if an approval was stored pending.
func assertNothingPending(t *testing.T, b *Backend) {
	t.Helper()
	b.permMu.Lock()
	n := len(b.pendingPerms)
	b.permMu.Unlock()
	if n != 0 {
		t.Errorf("pendingPerms has %d entries, want 0", n)
	}
}

// TestUnattendedTurn_CommandApprovalFromRules: a command approval arriving
// while an unattended turn is in flight is decided by foci's rules — accepted
// when a rule permits it, declined (with a WARN) when none does. Never a
// prompt: one would block the turn forever on a chat nobody is watching
// (incident 2026-10-27).
func TestUnattendedTurn_CommandApprovalFromRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, command, want string
	}{
		{"rule permits", "ls -la", "accept"},
		{"no rule", "rm -rf /tmp/x", "decline"},
		{"rule refuses substitution", `ls "$(pwd)"`, "decline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			b := newPermTestBackend(&buf)
			b.autoApproveRules = autoapprove.Compile([]string{"Bash:ls *"})
			prompted := ""
			b.permPromptFn = func(itemID, _, _, _ string, _ []delegator.PromptChoice) { prompted = itemID }

			openUnattendedTurn(b, "reflection")
			b.onCommandApproval([]byte(`{"itemId":"item_uc","threadId":"th_1","turnId":"tn_1","command":`+strconv.Quote(tc.command)+`,"cwd":"/tmp"}`), 51)

			if prompted != "" {
				t.Errorf("user prompted (%q) during an unattended turn; want no prompt", prompted)
			}
			if id, decision := parseApprovalResponse(t, buf.String()); id != 51 || decision != tc.want {
				t.Errorf("response = (%d, %q), want (51, %s)", id, decision, tc.want)
			}
			assertNothingPending(t, b)
		})
	}
}

// TestUnattendedTurn_FileChangeApprovalFromRules: a fileChange approval during
// an unattended turn is decided from the change's cached paths: every path
// must match a rule for its kind (add → Write, update → Edit, a rename's
// destination → Write). A delete, an unruled path or an uncached item is
// declined.
func TestUnattendedTurn_FileChangeApprovalFromRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		changes []fileChangeEntry // nil: item not cached
		want    string
	}{
		{"add permitted", []fileChangeEntry{{Path: "/tmp/w/a.go", Kind: patchChangeKind{Type: "add"}}}, "accept"},
		{"update permitted", []fileChangeEntry{{Path: "/tmp/w/a.go", Kind: patchChangeKind{Type: "update"}}}, "accept"},
		{"update outside rules", []fileChangeEntry{{Path: "/etc/hosts", Kind: patchChangeKind{Type: "update"}}}, "decline"},
		{"one path of two unruled", []fileChangeEntry{
			{Path: "/tmp/w/a.go", Kind: patchChangeKind{Type: "update"}},
			{Path: "/etc/hosts", Kind: patchChangeKind{Type: "update"}},
		}, "decline"},
		{"rename to unruled destination", []fileChangeEntry{{Path: "/tmp/w/a.go", Kind: patchChangeKind{Type: "update", MovePath: "/etc/a.go"}}}, "decline"},
		{"delete has no rule type", []fileChangeEntry{{Path: "/tmp/w/a.go", Kind: patchChangeKind{Type: "delete"}}}, "decline"},
		{"item not cached", nil, "decline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			b := newPermTestBackend(&buf)
			b.autoApproveRules = autoapprove.Compile([]string{"Write:/tmp/w/**", "Edit:/tmp/w/**"})
			b.itemCache = map[string]itemEnvelope{}
			if tc.changes != nil {
				b.itemCache["item_uf"] = itemEnvelope{Type: "fileChange", ID: "item_uf", Changes: tc.changes}
			}
			prompted := ""
			b.permPromptFn = func(itemID, _, _, _ string, _ []delegator.PromptChoice) { prompted = itemID }

			openUnattendedTurn(b, "consolidation")
			b.onFileChangeApproval([]byte(`{"itemId":"item_uf","threadId":"th_1","turnId":"tn_1","reason":"writing memory"}`), 52)

			if prompted != "" {
				t.Errorf("user prompted (%q) during an unattended turn; want no prompt", prompted)
			}
			if id, decision := parseApprovalResponse(t, buf.String()); id != 52 || decision != tc.want {
				t.Errorf("response = (%d, %q), want (52, %s)", id, decision, tc.want)
			}
			assertNothingPending(t, b)
		})
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
