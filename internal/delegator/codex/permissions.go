package codex

import (
	"encoding/json"
	"strings"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
)

// tryAutoApprove checks a command against compiled auto-approve rules.
// If matched, sends "accept" directly and returns true (no user prompt).
func (b *Backend) tryAutoApprove(rpcID int64, itemID, command string) bool {
	if len(b.autoApproveRules) == 0 {
		return false
	}
	input, _ := json.Marshal(map[string]string{"command": command})
	if !autoapprove.MatchWithEnv(b.autoApproveRules, "Bash", input, b.autoApproveEnv) {
		return false
	}
	b.lg.Infof("auto-approved: command=%q item=%s", command, itemID)
	b.respondApproval(rpcID, "accept")
	return true
}

func (b *Backend) onCommandApproval(line []byte, rpcID int64) {
	var params commandApprovalParams
	if err := json.Unmarshal(line, &params); err != nil {
		b.lg.Warnf("dropping malformed command approval: %v", err)
		return
	}

	if b.tryAutoApprove(rpcID, params.ItemID, params.Command) {
		return
	}

	b.permMu.Lock()
	b.pendingPerms[rpcID] = &pendingApproval{
		rpcID:   rpcID,
		itemID:  params.ItemID,
		command: params.Command,
	}
	b.permMu.Unlock()

	summary := "Run: " + params.Command
	if params.Reason != "" {
		summary = strings.TrimPrefix(params.Reason, "May I ")
	}

	if b.permPromptFn != nil {
		b.permPromptFn(
			params.ItemID,
			"Approve command: "+params.Command,
			summary,
			"",
			[]delegator.PromptChoice{
				{Label: "Allow", Data: "allow"},
				{Label: "Deny", Data: "deny"},
			},
		)
	} else {
		b.lg.Warnf("no permission prompt handler, auto-denying command: %s", params.Command)
		b.respondApproval(rpcID, "decline")
	}
}

// onFileChangeApproval handles item/fileChange/requestApproval.
func (b *Backend) onFileChangeApproval(line []byte, rpcID int64) {
	var params fileChangeApprovalParams
	if err := json.Unmarshal(line, &params); err != nil {
		b.lg.Warnf("dropping malformed file approval: %v", err)
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
		summary = strings.TrimPrefix(params.Reason, "May I ")
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
		b.lg.Warnf("no permission prompt handler, auto-denying file change")
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

func (b *Backend) onPermissionApproval(_ []byte, rpcID int64) {
	b.lg.Debugf("permission approval request (id=%d), auto-denying", rpcID)
	b.respondApproval(rpcID, "decline")
}

// respondApproval sends the approval decision back to the app-server.
func (b *Backend) respondApproval(rpcID int64, decision string) {
	b.permMu.Lock()
	delete(b.pendingPerms, rpcID)
	isEmpty := len(b.pendingPerms) == 0
	b.permMu.Unlock()

	if err := b.writer.sendResponse(rpcID, approvalResponse{Decision: decision}); err != nil {
		b.lg.Errorf("failed to send approval response: %v", err)
	}

	if isEmpty && b.onPromptsCleared != nil {
		b.onPromptsCleared()
	}
}

// permResponder mirrors the (unexported, method-local) interface the platform
// layer duck-types in internal/agent/delegated_permission.go. The match there
// is structural with no compile-time check — which is exactly how a wrong
// RespondToPermission signature shipped once before — so this assertion pins
// the contract statically.
type permResponder interface {
	RespondToPermission(requestID string, allow bool, message string) error
}

var _ permResponder = (*Backend)(nil)

// RespondToPermission resolves a pending approval by item ID.
// Implements the permResponder interface (same signature as ccstream):
// RespondToPermission(requestID string, allow bool, message string) error.
//
// message is accepted for interface compatibility but is NOT forwarded to
// codex: its approval-response schema is decision-only (verified against codex
// 0.144.5 — CommandExecution/FileChangeRequestApprovalResponse carry just a
// `decision` enum, no message/reason field), so a deny reason has nowhere to
// go on the wire.
func (b *Backend) RespondToPermission(requestID string, allow bool, message string) error {
	_ = message // see doc comment: codex has no wire field for a deny reason
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
