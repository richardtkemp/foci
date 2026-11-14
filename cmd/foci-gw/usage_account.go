package main

import (
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
)

// Usage accounts (#3104 phase 3). A delegated agent's usage counts against an
// account: a Claude Code agent's is its login (Spec.UsageAccount, resolved
// by ccaccount from its backend_config.env), so the usage hold, /limit_reset,
// /usage and /mana, and the spawn reserve's cached read are shared by the
// agents on one login and by no others. A backend without UsageAccount
// (codex) keeps the backend-wide hold account and per-agent reads.

// usageTargetFor is what a usage read of agent acfg needs: its id, its
// backend_config.env and its binary (the global section folded in, as the
// backend itself resolves it).
func usageTargetFor(cfg *config.Config, acfg config.AgentConfig) delegator.UsageTarget {
	bc := acfg.BackendConfig
	if spec, ok := delegator.SpecFor(acfg.Backend); ok {
		bc = foldGlobalBackendConfig(cfg, spec, bc)
	}
	return delegator.UsageTarget{AgentID: acfg.ID, Env: bc.Env, Binary: config.DerefStr(bc.Binary)}
}

// usageAccountFor is the usage account of an agent on backend whose
// backend_config.env is env (Spec.UsageAccount), or "" when the backend has
// none or it cannot be resolved (logged; the agent then falls back to the
// backend-wide account, Agent.UsageAccount).
func usageAccountFor(agentID, backend string, env map[string]string) string {
	spec, ok := delegator.SpecFor(backend)
	if !ok || spec.UsageAccount == nil {
		return ""
	}
	acct, err := spec.UsageAccount(env)
	if err != nil {
		log.NewComponentLogger("usagehold/"+agentID).Warnf("resolve the usage account: %v; using the backend-wide account %q", err, backend)
		return ""
	}
	return acct
}
