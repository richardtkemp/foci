package command

import (
	"context"
	"fmt"

	"foci/internal/delegator"
	"foci/internal/tools"
)

// PlanCommand creates a /plan command that puts the coding-agent backend into
// plan mode for the user's request. It is only registered for delegated (CC)
// backends that supplied a plan delivery via delegator.RegisterPlan; API agents
// and plan-less backends never see it (cmd/foci-gw/commands.go).
//
// The command owns the generic concerns — delegated-only, non-empty args, a
// resolved session, and deps wiring — and delegates the backend-specific
// mechanism to the injected delivery: a verbatim "/plan" slash command (cctmux)
// or an EnterPlanMode turn (ccstream). The behaviour lives with each backend.
func PlanCommand(delivery delegator.PlanDelivery) *Command {
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

			deps := delegator.PlanDeps{
				SessionKey: sk,
				Backend: func() (delegator.Delegator, error) {
					return cc.Agent.DelegatedManager.Get(ctx, sk)
				},
			}
			// Typed-nil check: assign Notifier only when the concrete pointer is
			// non-nil, so the delivery's interface-nil guard works (a nil
			// *AsyncNotifier boxed in an interface is itself non-nil).
			if cc.Agent.AsyncNotifier != nil {
				deps.Notifier = cc.Agent.AsyncNotifier
			}

			text, err := delivery(ctx, deps, req.Args)
			if err != nil {
				return Response{}, err
			}
			return Response{Text: text}, nil
		},
	}
}
