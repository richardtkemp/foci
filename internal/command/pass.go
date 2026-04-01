package command

import (
	"context"
	"fmt"

	"foci/internal/tools"
)

// PassCommand creates a /pass command that forwards raw text directly to the
// delegated backend (Claude Code). This bypasses foci's command dispatch,
// allowing users to run CC slash commands that would otherwise be intercepted
// by foci (e.g., /context, /model, /compact).
//
// Only available for agents with a delegated backend — returns an error for
// API-mode agents where there's no backend to forward to.
//
// Usage: /pass /context
//        /pass /model opus
//        /pass /help
func PassCommand() *Command {
	return &Command{
		Name:        "pass",
		Description: "Forward a command directly to Claude Code",
		Category:    "operations",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			if cc.Agent.DelegatedManager == nil {
				return Response{}, fmt.Errorf("/pass is only available for delegated backends (Claude Code)")
			}

			if req.Args == "" {
				return Response{}, fmt.Errorf("usage: /pass <command>\nexample: /pass /context")
			}

			sk := tools.SessionKeyFromContext(ctx)
			if sk == "" {
				sk = req.SessionKey
			}
			if sk == "" {
				return Response{}, fmt.Errorf("no active session")
			}

			be, err := cc.Agent.DelegatedManager.Get(ctx, sk)
			if err != nil {
				return Response{}, fmt.Errorf("get backend: %w", err)
			}

			if err := be.SendCommand(ctx, req.Args); err != nil {
				return Response{}, fmt.Errorf("send command: %w", err)
			}

			return Response{Text: fmt.Sprintf("↗ Sent to CC: `%s`", req.Args)}, nil
		},
	}
}
