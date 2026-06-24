package command

import (
	"context"
	"fmt"

	"foci/internal/delegator"
	"foci/internal/tools"
)

// PlanMode selects how PlanCommand delivers the request to the coding-agent
// backend. It is chosen at registration time (cmd/foci-gw/commands.go) from
// the agent's configured backend, so the command never has to detect the
// backend kind at runtime.
type PlanMode int

const (
	// PlanNativeSlash forwards "/plan <args>" verbatim to a TUI backend
	// (cctmux / claude-code-tmux), whose CC instance handles the native
	// /plan slash command itself.
	PlanNativeSlash PlanMode = iota

	// PlanEnterTool sends a natural-language prompt asking CC to invoke the
	// EnterPlanMode tool. Used for the ccstream (claude-code) headless
	// backend, where the native /plan slash command "opens an interactive
	// panel and isn't available in this environment".
	PlanEnterTool
)

// PlanCommand creates a /plan command that puts Claude Code into plan mode for
// the user's request. It is only registered for delegated (CC) backends; API
// agents never see it.
//
// Delivery depends on mode (fixed per backend at registration):
//   - PlanNativeSlash (cctmux): forwards "/plan <args>" verbatim — exactly what
//     the user typed, unmanipulated — as a fire-and-forget slash command.
//   - PlanEnterTool (ccstream): injects a fresh turn carrying a natural-language
//     prompt that instructs CC to invoke EnterPlanMode for <args>, since the
//     native /plan slash command is unavailable in the headless stream.
func PlanCommand(mode PlanMode) *Command {
	return &Command{
		Name:        "plan",
		Description: "Start Claude Code plan mode for the given request",
		Category:    "operations",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			if cc.Agent.DelegatedManager == nil {
				return Response{}, fmt.Errorf("/plan is only available for delegated backends (Claude Code)")
			}
			if req.Args == "" {
				return Response{}, fmt.Errorf("usage: /plan <what to plan>\nexample: /plan add rate limiting to the API")
			}

			sk := tools.SessionKeyFromContext(ctx)
			if sk == "" {
				sk = req.SessionKey
			}
			if sk == "" {
				return Response{}, fmt.Errorf("no active session")
			}

			switch mode {
			case PlanNativeSlash:
				be, err := cc.Agent.DelegatedManager.Get(ctx, sk)
				if err != nil {
					return Response{}, fmt.Errorf("get backend: %w", err)
				}
				// Forward verbatim — exactly what the user sent, no manipulation.
				cmd := "/plan " + req.Args
				if err := be.Inject(ctx, delegator.Inject{
					Source: delegator.SourcePass,
					Text:   cmd,
				}); err != nil {
					return Response{}, fmt.Errorf("send command: %w", err)
				}
				return Response{Text: fmt.Sprintf("↗ Sent to CC: `%s`", cmd)}, nil

			case PlanEnterTool:
				// The native /plan slash command is unavailable headless, so drive
				// a fresh turn that asks CC to invoke the EnterPlanMode tool. Use
				// AsyncNotifier (the #845 proper-turn injection path) rather than a
				// raw Inject so the turn gets full post-turn bookkeeping.
				if cc.Agent.AsyncNotifier == nil {
					return Response{}, fmt.Errorf("/plan unavailable: async injection not configured")
				}
				prompt := "please invoke EnterPlanMode tool for:\n" + req.Args
				cc.Agent.AsyncNotifier.InjectToAgent(sk, prompt, "", "plan-command")
				return Response{Text: "📋 Entering plan mode…"}, nil

			default:
				return Response{}, fmt.Errorf("/plan: unknown plan mode %d", mode)
			}
		},
	}
}
