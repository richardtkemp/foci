package agent

import (
	"context"
)

// SendPermissionResponse sends a keystroke to the delegated agent's TUI
// for the given session key. Used for permission prompt responses where
// the CC TUI expects a keypress, not pasted text.
func (a *Agent) SendPermissionResponse(ctx context.Context, sessionKey string, key string) error {
	if a.DelegatedManager == nil {
		return nil
	}
	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return err
	}
	return be.SendKeystroke(ctx, key)
}
