package agent

import (
	"context"

	"foci/internal/platform"
)

func (a *Agent) AddPlatform(name string, p platform.Sender) {
	a.platformMu.Lock()
	defer a.platformMu.Unlock()
	if a.platforms == nil {
		a.platforms = make(map[string]platform.Sender)
	}
	a.platforms[name] = p
}

func (a *Agent) GetPlatform(name string) platform.Sender {
	a.platformMu.RLock()
	defer a.platformMu.RUnlock()
	return a.platforms[name]
}

func (a *Agent) StartPlatforms(ctx context.Context) error {
	a.platformMu.RLock()
	defer a.platformMu.RUnlock()
	for _, p := range a.platforms {
		if starter, ok := p.(interface{ Start(context.Context) error }); ok {
			if err := starter.Start(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Agent) StopPlatforms() error {
	a.platformMu.RLock()
	defer a.platformMu.RUnlock()
	var firstErr error
	for _, p := range a.platforms {
		if stopper, ok := p.(interface{ Stop() error }); ok {
			if err := stopper.Stop(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
