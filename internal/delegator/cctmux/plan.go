package cctmux

import (
	"context"
	"fmt"

	"foci/internal/delegator"
)

// plan.go — cctmux's /plan delivery (registered in init(), claudecode.go).
//
// In the tmux TUI the native /plan slash command works, so forward
// "/plan <args>" verbatim — exactly what the user typed, no manipulation — as a
// fire-and-forget slash command and let CC's TUI handle plan mode itself.
func planDelivery(ctx context.Context, deps delegator.PlanDeps, args string) (string, error) {
	be, err := deps.Backend()
	if err != nil {
		return "", fmt.Errorf("get backend: %w", err)
	}
	cmd := "/plan " + args
	if err := be.Inject(ctx, delegator.Inject{Source: delegator.SourcePass, Text: cmd}); err != nil {
		return "", fmt.Errorf("send command: %w", err)
	}
	return fmt.Sprintf("↗ Sent to CC: `%s`", cmd), nil
}
