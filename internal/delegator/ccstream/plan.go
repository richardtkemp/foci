package ccstream

import (
	"context"
	"fmt"

	"foci/internal/delegator"
)

// plan.go — ccstream's /plan delivery (registered in init(), ccstream.go).
//
// The native /plan slash command "opens an interactive panel and isn't
// available in this environment" (it's a CC TUI affordance gated off the
// headless stream). So instead of forwarding the slash command, drive a fresh
// turn that asks CC to invoke the EnterPlanMode tool for the user's request.
// Use the agent-level notifier — the #845 proper-turn injection path — so the
// plan turn gets full post-turn bookkeeping (save, compaction check) rather
// than being folded raw into stdin.
func planDelivery(_ context.Context, deps delegator.PlanDeps, args string) (string, error) {
	if deps.Notifier == nil {
		return "", fmt.Errorf("/plan unavailable: async injection not configured")
	}
	deps.Notifier.InjectToAgent(deps.SessionKey, "please invoke EnterPlanMode tool for:\n"+args, "", "plan-command")
	return "📋 Entering plan mode…", nil
}
