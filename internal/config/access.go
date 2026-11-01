package config

import "strings"

// AllowedAccess is one agent's resolved message allowlist on one platform:
// the union of the agent-level and global allowed_users, and the
// allowed_users_only flag that wins the two-level cascade.
type AllowedAccess struct {
	Users []string
	Only  bool
}

// ResolveAllowedAccess resolves an agent's access.allowed_users /
// allowed_users_only on one platform from the raw agent and global config
// levels — the single copy of the cascade every chat transport uses (telegram
// and discord historically carried two private duplicates; slack joins them):
//
//   - Users is the union of the agent's and the global allowed_users for the
//     platform (agent entries first, deduplicated).
//
//   - Only is the agent's allowed_users_only flag when set, else the global
//     flag, else true (strict). Combined with platform.UserAllowed: only
//     listed users pass, so an empty list blocks everyone; when explicitly
//     false, an empty list allows anyone while a non-empty list still filters.
//
// A change only takes effect after a restart.
func ResolveAllowedAccess(cfg *Config, acfg AgentConfig, platformID string) AllowedAccess {
	var agentUsers, globalUsers []string
	if p := acfg.Platform(platformID); p != nil {
		agentUsers = p.Access.AllowedUsers
	}
	if gp := cfg.Platform(platformID); gp != nil {
		globalUsers = gp.Access.AllowedUsers
	}
	users := SuperveneSlice(agentUsers, globalUsers, func(s string) string { return s })

	only := true
	if p := acfg.Platform(platformID); p != nil && p.Access.AllowedUsersOnly != nil {
		only = *p.Access.AllowedUsersOnly
	} else if gp := cfg.Platform(platformID); gp != nil && gp.Access.AllowedUsersOnly != nil {
		only = *gp.Access.AllowedUsersOnly
	}
	return AllowedAccess{Users: users, Only: only}
}

// DefaultNonOperatorCommands returns a fresh copy of the commands a non-operator
// may run when non_operator_commands is set at neither the agent nor the global
// platform level: help, reset and stop (#2279).
func DefaultNonOperatorCommands() []string {
	return []string{"help", "reset", "stop"}
}

// OperatorAccess is one agent's resolved operator gate on one platform: the
// union of the agent-level and global operator user IDs, and the non-operator
// command list that wins the two-level cascade. Users is empty when nobody is
// restricted; Commands is never nil (an explicitly empty list stays empty and
// never falls back to the default).
type OperatorAccess struct {
	Users    []string
	Commands []string
}

// ResolveOperatorAccess resolves an agent's operator settings on one platform
// from the raw agent and global config levels (#2279):
//
//   - Users is the union of the agent's and the global operator_users for the
//     platform, with each entry trimmed (the clean function of DedupeBy) and
//     blanks and repeats dropped. Trimming is load-bearing: platform user
//     IDs arrive exact ("111"), so a padded entry (" 111 ") would silently
//     restrict the operator it names, and a blank entry must neither turn
//     the gate on with nobody who can match nor match the empty presser ID.
//     A global operator is an operator on every agent of the platform.
//
//   - Commands is the agent's non_operator_commands when set (even when empty),
//     else the global list when set, else DefaultNonOperatorCommands().
//     Command-list entries are NOT blank-filtered here: a blank entry names
//     no registered command, which the policy builder reports at WARN and
//     ignores.
//
// Like allowed_users, a change only takes effect after a restart.
func ResolveOperatorAccess(cfg *Config, acfg AgentConfig, platformID string) OperatorAccess {
	var agentUsers, globalUsers, agentCmds, globalCmds []string
	if p := acfg.Platform(platformID); p != nil {
		agentUsers, agentCmds = p.Access.OperatorUsers, p.Access.NonOperatorCommands
	}
	if gp := cfg.Platform(platformID); gp != nil {
		globalUsers, globalCmds = gp.Access.OperatorUsers, gp.Access.NonOperatorCommands
	}

	clean := func(in []string) []string { return DedupeBy(in, strings.TrimSpace) }
	users := SuperveneSlice(clean(agentUsers), clean(globalUsers), func(s string) string { return s })

	var commands []string
	switch {
	case agentCmds != nil:
		commands = agentCmds
	case globalCmds != nil:
		commands = globalCmds
	default:
		commands = DefaultNonOperatorCommands()
	}

	return OperatorAccess{Users: users, Commands: commands}
}
