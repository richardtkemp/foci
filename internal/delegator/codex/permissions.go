package codex

import (
	"encoding/json"

	"foci/internal/delegator"
)

// onCommandApproval handles item/commandExecution/requestApproval — a
// server-initiated JSON-RPC request asking the client to approve a command.
func (b *Backend) onCommandApproval(line []byte, rpcID int64) {
	var params commandApprovalParams
	if err := json.Unmarshal(line, &params); err != nil {
		b.lg.Warnf("dropping malformed command approval: %v", err)
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
		summary = params.Reason
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

	summary := "File change"
	if params.Reason != "" {
		summary = params.Reason
	}

	if b.permPromptFn != nil {
		b.permPromptFn(
			params.ItemID,
			"Approve file change",
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

// onPermissionApproval handles item/permissions/requestApproval — a
// request from the built-in request_permissions tool.
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

// RespondToPermission resolves a pending approval by item ID.
// Implements the permResponder interface (same signature as ccstream):
// RespondToPermission(requestID string, allow bool, message string) error.
func (b *Backend) RespondToPermission(requestID string, allow bool, message string) error {
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
