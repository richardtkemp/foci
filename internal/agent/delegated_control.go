package agent

import (
	"context"
	"fmt"

	"foci/internal/backend"
	"foci/internal/log"
)

// SendBackendControl sends a control request to the delegated backend for
// the given session key. Returns (true, nil) if the backend handled the
// request, (false, nil) if no backend exists or it doesn't implement
// ControlSender, and (false, err) on failure.
func (a *Agent) SendBackendControl(ctx context.Context, sessionKey string, req backend.ControlRequest) (handled bool, err error) {
	if a.DelegatedManager == nil {
		return false, nil
	}

	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return false, fmt.Errorf("get backend for control: %w", err)
	}

	cs, ok := be.(backend.ControlSender)
	if !ok {
		log.Debugf("agent", "backend %T does not implement ControlSender", be)
		return false, nil
	}

	if err := cs.SendControl(ctx, req); err != nil {
		return false, fmt.Errorf("send control: %w", err)
	}

	return true, nil
}
