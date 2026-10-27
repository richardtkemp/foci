package codex

import (
	"encoding/json"
	"path/filepath"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
	"foci/internal/delegator/sessionenv"
)

// tryAutoApprove checks a command against compiled auto-approve rules.
// If matched, sends "accept" directly and returns true (no user prompt).
// vetoReason is the rule engine's veto when a rule matched but was overridden
// ("" when nothing matched).
func (b *Backend) tryAutoApprove(rpcID int64, itemID, command string) (approved bool, vetoReason string) {
	if len(b.autoApproveRules) == 0 {
		return false, ""
	}
	input, _ := json.Marshal(map[string]string{"command": command})
	matched, vetoReason := autoapprove.MatchWithEnv(b.autoApproveRules, "Bash", input, b.autoApproveEnv)
	if !matched {
		// A rule matched but the substitutability guard overrode it — say why,
		// or the refusal is invisible to everyone (#1906).
		if vetoReason != "" {
			b.logWarnf("auto-approve VETOED: command=%q item=%s — %s", command, itemID, vetoReason)
		}
		return false, vetoReason
	}
	b.logInfof("auto-approved: command=%q item=%s", command, itemID)
	b.respondApproval(rpcID, "accept")
	return true, ""
}

// fileChangeRuleTool maps a codex file change kind (PatchChangeKind.Type) to
// the auto-approve tool whose rules govern it: adding a file is a Write,
// updating one an Edit. A delete has no rule type in foci, so no rule can
// permit it (absent here).
var fileChangeRuleTool = map[string]string{
	"add":    "Write",
	"update": "Edit",
}

// fileChangePermitted reports whether foci's auto-approve rules permit every
// path of a file change: each path must match a rule for its kind's tool
// (fileChangeRuleTool), and a rename (update with move_path) must also be
// permitted to Write its destination. Relative paths resolve against the
// workspace, as ccstream and opencode do. An empty change list (the item was
// not cached) or a kind with no rule type is not permitted. vetoReason is the
// first veto.
func (b *Backend) fileChangePermitted(changes []fileChangeEntry) (permitted bool, vetoReason string) {
	if len(b.autoApproveRules) == 0 || len(changes) == 0 {
		return false, ""
	}
	for _, c := range changes {
		tool, ok := fileChangeRuleTool[c.Kind.Type]
		if !ok || c.Path == "" {
			return false, ""
		}
		if ok, veto := b.pathPermitted(tool, c.Path); !ok {
			return false, veto
		}
		if c.Kind.MovePath != "" {
			if ok, veto := b.pathPermitted("Write", c.Kind.MovePath); !ok {
				return false, veto
			}
		}
	}
	return true, ""
}

// pathPermitted matches one file path against the rules for tool.
func (b *Backend) pathPermitted(tool, path string) (bool, string) {
	if !filepath.IsAbs(path) && b.workDir != "" {
		path = filepath.Join(b.workDir, path)
	}
	input, _ := json.Marshal(map[string]string{"file_path": path})
	return autoapprove.MatchWithEnv(b.autoApproveRules, tool, input, b.autoApproveEnv)
}

func (b *Backend) onCommandApproval(line []byte, rpcID int64) {
	var params commandApprovalParams
	if err := json.Unmarshal(line, &params); err != nil {
		b.logWarnf("dropping malformed command approval: %v", err)
		return
	}

	// Auto-approve rules and the prompt text must both see the command the
	// agent asked for, not foci's session-env wrapper around it — otherwise
	// installing the hook silently invalidates every operator-written Bash
	// rule and shows the user a command they didn't write. Codex still raises
	// this approval for a hook-rewritten command (the hook's mandatory
	// permissionDecision:allow does not bypass the approval flow), so the
	// unwrap is the only thing keeping the two in sync.
	command := sessionenv.UnwrapDisplayCommand(params.Command)

	approved, vetoReason := b.tryAutoApprove(rpcID, params.ItemID, command)
	if approved {
		return
	}

	// Nobody can answer this approval (an unattended turn): the rules above
	// did not permit it, so it is declined, never prompted — a prompt would
	// block the turn forever (incident 2026-10-27; delegator.Unattended).
	if label, ok := b.unattended(); ok {
		b.logWarnf("%s item=%s", delegator.UnattendedDeniedLog(b.startOpts.SessionKey, label, "run: "+command, vetoReason), params.ItemID)
		b.respondApproval(rpcID, "decline")
		return
	}

	b.permMu.Lock()
	b.pendingPerms[rpcID] = &pendingApproval{
		rpcID:   rpcID,
		itemID:  params.ItemID,
		command: command,
	}
	b.permMu.Unlock()

	summary := "Run: " + command
	if params.Reason != "" {
		summary = params.Reason
	}

	if b.permPromptFn != nil {
		b.permPromptFn(
			params.ItemID,
			"Approve command: "+command,
			summary,
			"",
			[]delegator.PromptChoice{
				{Label: "Allow", Data: "allow"},
				{Label: "Deny", Data: "deny"},
			},
		)
	} else {
		b.logWarnf("no permission prompt handler, auto-denying command: %s", command)
		b.respondApproval(rpcID, "decline")
	}
}

// onFileChangeApproval handles item/fileChange/requestApproval.
func (b *Backend) onFileChangeApproval(line []byte, rpcID int64) {
	var params fileChangeApprovalParams
	if err := json.Unmarshal(line, &params); err != nil {
		b.logWarnf("dropping malformed file approval: %v", err)
		return
	}

	// Nobody can answer this approval (an unattended turn): foci's rules
	// decide it from the change's paths (fileChangePermitted) — accept when
	// every path is permitted, otherwise decline. Never prompted.
	if label, ok := b.unattended(); ok {
		changes := b.lookupItemChanges(params.ItemID)
		action := "write " + summarizePaths(changes)
		if len(changes) == 0 {
			action = "make a file change (paths unknown)"
		}
		if permitted, vetoReason := b.fileChangePermitted(changes); permitted {
			b.logInfof("%s item=%s", delegator.UnattendedAllowedLog(b.startOpts.SessionKey, label, action), params.ItemID)
			b.respondApproval(rpcID, "accept")
		} else {
			b.logWarnf("%s item=%s", delegator.UnattendedDeniedLog(b.startOpts.SessionKey, label, action, vetoReason), params.ItemID)
			b.respondApproval(rpcID, "decline")
		}
		return
	}

	b.permMu.Lock()
	b.pendingPerms[rpcID] = &pendingApproval{
		rpcID:  rpcID,
		itemID: params.ItemID,
	}
	b.permMu.Unlock()

	// Try to extract file paths from the stashed item (item/started
	// fires before the approval request).
	detail := b.lookupItemDetail(params.ItemID)

	// Prefer the extracted file-path detail (what will actually be written),
	// falling back to the model's self-reported reason, then a generic label.
	// This previously ran in the OPPOSITE order — params.Reason unconditionally
	// clobbered the file-path detail — so the user approved against the model's
	// reason instead of the actual file list.
	summary := detail
	if summary == "" && params.Reason != "" {
		summary = params.Reason
	}
	if summary == "" {
		summary = "File change"
	}

	prompt := "Approve file change"
	if detail != "" {
		prompt = "Write: " + detail
	}

	if b.permPromptFn != nil {
		b.permPromptFn(
			params.ItemID,
			prompt,
			summary,
			"",
			[]delegator.PromptChoice{
				{Label: "Allow", Data: "allow"},
				{Label: "Deny", Data: "deny"},
			},
		)
	} else {
		b.logWarnf("no permission prompt handler, auto-denying file change")
		b.respondApproval(rpcID, "decline")
	}
}

// lookupItemDetail returns a human-readable detail string for a stashed item
// (file paths for fileChange, empty for other types). Uses the item cache
// populated by item/started notifications, which fire before approval requests.
func (b *Backend) lookupItemDetail(itemID string) string {
	b.itemMu.Lock()
	item, ok := b.itemCache[itemID]
	b.itemMu.Unlock()
	if !ok {
		return ""
	}
	switch item.Type {
	case "fileChange":
		return summarizePaths(item.Changes)
	case "webSearch":
		return item.Query
	default:
		return ""
	}
}

// lookupItemChanges returns the paths a stashed fileChange item will write
// (nil when the item is not cached or is not a file change). Same cache as
// lookupItemDetail.
func (b *Backend) lookupItemChanges(itemID string) []fileChangeEntry {
	b.itemMu.Lock()
	item, ok := b.itemCache[itemID]
	b.itemMu.Unlock()
	if !ok || item.Type != "fileChange" {
		return nil
	}
	return item.Changes
}

func (b *Backend) onPermissionApproval(_ []byte, rpcID int64) {
	b.logDebugf("permission approval request (id=%d), auto-denying", rpcID)
	b.respondApproval(rpcID, "decline")
}

// unattended reports whether an approval raised now has nobody to answer it,
// and the label its log lines carry (delegator.Unattended). Codex has no
// skip-permissions session mode, so only the in-flight turn decides. Guarded
// by turnMu: approvals arrive on the reader goroutine while turns begin/end on
// the inject path. ("", false) between turns — TurnEvents is cleared at turn
// end, so approvals then surface via permPromptFn again.
func (b *Backend) unattended() (label string, ok bool) {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	return delegator.Unattended(false, b.turnEvents)
}

// respondApproval sends the approval decision back to the app-server.
func (b *Backend) respondApproval(rpcID int64, decision string) {
	b.permMu.Lock()
	delete(b.pendingPerms, rpcID)
	isEmpty := len(b.pendingPerms) == 0
	b.permMu.Unlock()

	if err := b.process().writer.sendResponse(rpcID, approvalResponse{Decision: decision}); err != nil {
		b.logErrorf("failed to send approval response: %v", err)
	}

	if isEmpty && b.onPromptsCleared != nil {
		b.onPromptsCleared()
	}
}

// RespondToPermission implements delegator.PermissionResponder: it resolves a
// pending approval by item ID with accept or decline.
//
// Only d.Allow reaches codex: its approval-response schema is decision-only
// (verified against codex 0.144.5 — CommandExecution/
// FileChangeRequestApprovalResponse carry just a `decision` enum, no
// message/reason field), so a deny reason has nowhere to go on the wire, and
// Remember/RulePrefix (an "allow always") answer as a plain accept.
func (b *Backend) RespondToPermission(requestID string, d delegator.PermissionDecision) error {
	allow := d.Allow
	b.permMu.Lock()
	var rpcID int64
	found := false
	for id, perm := range b.pendingPerms {
		if perm.itemID == requestID {
			rpcID = id
			found = true
			break
		}
	}
	b.permMu.Unlock()

	if !found {
		return errNoPendingApproval
	}
	decision := "accept"
	if !allow {
		decision = "decline"
	}
	b.respondApproval(rpcID, decision)
	return nil
}

var errNoPendingApproval = permissionError("codex: no pending approval for item")

type permissionError string

func (e permissionError) Error() string { return string(e) }
