package agent

import (
	"context"
)

// SendPermissionResponse sends a keystroke to the backend agent's TUI
// for the given session key. Used for permission prompt responses where
// the CC TUI expects a keypress, not pasted text.
func (a *Agent) SendPermissionResponse(ctx context.Context, sessionKey string, key string) error {
	if a.BackendManager == nil {
		return nil
	}
	be, err := a.BackendManager.Get(ctx, sessionKey)
	if err != nil {
		return err
	}
	return be.SendKeystroke(ctx, key)
}
