package main

import (
	"foci/internal/config"
	"foci/internal/delegator/autoapprove"
	"foci/internal/execguard"
)

// autoApproveGroupToggles resolves, per agent, the same built-in group
// switches buildAutoApproveRules reads — config.Resolve's cascade over
// [permissions] and [[agents]].permissions — and ORs them over all agents: a
// group is reported when at least one agent has it enabled, so a per-agent
// true under a global false counts, and a group disabled everywhere is not
// reported.
func autoApproveGroupToggles(cfg *config.Config) (readonly, safeWrite bool) {
	for i := range cfg.Agents {
		perms := config.Resolve(cfg, cfg.Agents[i]).Permissions
		readonly = readonly || perms.AutoApproveCommonReadonly
		safeWrite = safeWrite || perms.AutoApproveCommonSafeWrite
	}
	return readonly, safeWrite
}

// substitutableBuiltinsFor lists the built-in group commands the match-time
// veto will refuse under cfg's group switches — the single finder shared by
// both reporters (the startup WARN and the -check-config preview), so they
// cannot disagree (#1897).
func substitutableBuiltinsFor(cfg *config.Config, env execguard.Env) []autoapprove.SubstitutableBuiltin {
	readonly, safeWrite := autoApproveGroupToggles(cfg)
	return autoapprove.SubstitutableBuiltins(readonly, safeWrite, env)
}

// warnSubstitutableBuiltins is the startup half of the built-in report: one
// WARN per built-in group command the match-time veto will refuse. The
// startup drop covers config entries only, while the veto also hits the
// built-in groups — without this WARN a vetoed built-in (mds, mdq on the
// live host) simply falls through to an approval prompt with no line
// anywhere explaining why (#1897). Called by reportOperatorEnv with the
// same environment the startup drop judged.
func warnSubstitutableBuiltins(cfg *config.Config, env execguard.Env) {
	for _, b := range substitutableBuiltinsFor(cfg, env) {
		configLog.Warnf("[permissions] built-in %s group: command %q will be vetoed at match time and falls through to an approval prompt — %s (%s)",
			b.Group, b.Command, b.Reason, b.Path)
	}
}
