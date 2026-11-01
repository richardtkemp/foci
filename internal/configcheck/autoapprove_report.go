package configcheck

import (
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
	"foci/internal/execguard"
)

// AgentConsultsAllowlist is the one predicate for "this agent's permission
// requests flow through foci's auto-approve allowlist, with a prompt the
// match-time veto can reach":
//
//   - only DELEGATED agents get the rules at all: buildAutoApproveRules
//     runs in configureDelegated, so an API agent's toggles are inert;
//   - only backends that consult foci's allowlist (Spec declares
//     CapCommandApprovalAllowlist) ever match a rule against a permission
//     request;
//   - a skip_permissions agent never prompts: its backend runs everything
//     unattended, so no command there can stop auto-approving, and text
//     about an approval prompt would be false.
//
// Every consumer of that condition calls this — the Command Approval
// section of the agent environment (buildEnvironmentDelegated) and the
// built-in veto reporters (#1897) — so the prompt text and the reporters
// cannot drift from each other or from the enforcer.
func AgentConsultsAllowlist(a config.AgentConfig) bool {
	if !a.IsDelegated() {
		return false
	}
	spec, ok := delegator.SpecFor(a.Backend)
	return ok && spec.Supports(delegator.CapCommandApprovalAllowlist) && !config.DerefBool(a.BackendConfig.SkipPermissions)
}

// autoApproveGroupToggles resolves, per agent the built-in groups are live
// for (AgentConsultsAllowlist — the enforcer's whole serving condition, not
// just the toggle value buildAutoApproveRules reads), the same group
// switches buildAutoApproveRules reads — config.Resolve's cascade over
// [permissions] and [[agents]].permissions — and ORs them: a group is
// reported when at least one such agent has it enabled, so a per-agent true
// under a global false counts, and a group disabled everywhere (or enabled
// only where it cannot run) is not reported.
func autoApproveGroupToggles(cfg *config.Config) (readonly, safeWrite bool) {
	for i := range cfg.Agents {
		if !AgentConsultsAllowlist(cfg.Agents[i]) {
			continue
		}
		perms := config.Resolve(cfg, cfg.Agents[i]).Permissions
		readonly = readonly || perms.AutoApproveCommonReadonly
		safeWrite = safeWrite || perms.AutoApproveCommonSafeWrite
	}
	return readonly, safeWrite
}

// SubstitutableBuiltinsFor lists the built-in group commands the match-time
// veto will refuse under cfg's group switches — the single finder shared by
// both reporters (the gateway's startup WARN and the check's preview), so
// they cannot disagree (#1897).
func SubstitutableBuiltinsFor(cfg *config.Config, env execguard.Env) []autoapprove.SubstitutableBuiltin {
	readonly, safeWrite := autoApproveGroupToggles(cfg)
	return autoapprove.SubstitutableBuiltins(readonly, safeWrite, env)
}
