package command

import (
	"context"
)

// LimitResetCommand returns /limit_reset (#3043, Dick 2026-11-11: "/limit_reset
// cmd, force removes the gate"). It lifts the agent's rate-limit gate and its
// account's usage hold at once (Agent.ResetLimits), for the case where the
// operator has reset the account's limit and foci cannot see it: in a usage
// stop nothing runs, so no new usage event arrives to clear the hold. The
// stored state is cleared too, so a restart does not bring the gate back. The
// hold is account-wide, so the lift reaches every agent on the account.
//
// Registered only for a backend declaring CapLimitReset (Claude Code).
func LimitResetCommand() *Command {
	return &Command{
		Name:        "limit_reset",
		Description: "Force-lift the rate-limit gate and usage hold (after the account's limit was reset)",
		Category:    "operations",
		Requires:    RequiresBackend,
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			if cc.Agent == nil {
				return Response{Text: "No agent to reset."}, nil
			}
			res := cc.Agent.ResetLimits("/limit_reset")
			if !res.Cleared() {
				return Response{Text: res.Summary()}, nil
			}
			return Response{Text: "▶️ " + res.Summary()}, nil
		},
	}
}
