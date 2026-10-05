package command

import (
	"context"
	"fmt"

	"foci/internal/delegator"
	"foci/internal/tools"
)

// PassCommand creates a /pass command that forwards raw text directly to the
// configured delegated backend. This bypasses foci's command dispatch,
// allowing users to run backend-native slash commands that would otherwise
// be intercepted by foci (e.g., /context, /model, /compact).
//
// Any output the command produces arrives through the backend's normal stream
// events and is delivered like turn output.
//
// Only available for agents with a delegated backend — returns an error for
// API-mode agents where there's no backend to forward to.
//
// Usage: /pass /context
//
//	/pass /model opus
//	/pass /help
//
// backendType is the configured [agents].backend value (e.g. "claude-code",
// "codex"), used only to name the backend in user-facing text — it does not
// change dispatch behaviour, which still goes through DelegatedManager.
func PassCommand(backendType string) *Command {
	backendName := delegator.HumanReadableBackendName(backendType)
	return &Command{
		Name:        "pass",
		Description: fmt.Sprintf("Forward a command directly to %s", backendName),
		Category:    "operations",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			if cc.Agent.DelegatedManager == nil {
				return Response{}, fmt.Errorf("/pass is only available for agents with a delegated backend (%s)", backendName)
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

			if err := be.ImmediateInject(ctx, delegator.Inject{
				Source: delegator.SourcePass,
				Text:   req.Args,
			}); err != nil {
				return Response{}, fmt.Errorf("send command: %w", err)
			}

			return Response{Text: fmt.Sprintf("↗ Sent to %s: `%s`", backendName, req.Args)}, nil
		},
	}
}
