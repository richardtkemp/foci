package agent

import (
	"context"
	"fmt"

	"foci/internal/delegator"
)

// SendBackendControl sends a control request to the delegated backend for
// the given session key. Returns (true, nil) if the backend handled the
// request, (false, nil) if no backend exists or it doesn't implement
// ControlSender, and (false, err) on failure.
func (a *Agent) SendBackendControl(ctx context.Context, sessionKey string, req delegator.ControlRequest) (handled bool, err error) {
	if a.DelegatedManager == nil {
		return false, nil
	}

	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return false, fmt.Errorf("get backend for control: %w", err)
	}

	cs, ok := be.(delegator.ControlSender)
	if !ok {
		a.logger().Debugf("backend %T does not implement ControlSender", be)
		return false, nil
	}

	if err := cs.SendControl(ctx, req); err != nil {
		return false, fmt.Errorf("send control: %w", err)
	}

	return true, nil
}

// ResolveBackendModel asks a catalogue-backed delegated backend to canonicalize
// a model alias. It returns handled=false when the agent has no such resolver.
func (a *Agent) ResolveBackendModel(ctx context.Context, sessionKey, model string) (resolution delegator.ModelResolution, handled bool, err error) {
	if a.DelegatedManager == nil {
		return delegator.ModelResolution{}, false, nil
	}
	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return delegator.ModelResolution{}, false, fmt.Errorf("get backend for model resolution: %w", err)
	}
	resolver, ok := be.(delegator.ModelResolver)
	if !ok {
		return delegator.ModelResolution{}, false, nil
	}
	resolution, err = resolver.ResolveModel(ctx, model)
	if err != nil {
		return delegator.ModelResolution{}, true, err
	}
	return resolution, true, nil
}
