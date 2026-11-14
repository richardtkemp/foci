package main

import (
	"sync"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/usagelog"
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

// usageHistory is the usage history store (#3103): every usage figure the
// gateway receives from a backend with /usage is written to it. Opened at
// startup (openUsageHistory); nil when it could not be opened (then nothing
// is stored, and /mana cached says it holds nothing).
var (
	usageHistoryMu sync.Mutex
	usageHistory   *usagelog.Store
)

// openUsageHistory opens the usage history at path and makes it the
// gateway's store. A failure is logged and leaves no store.
func openUsageHistory(path string) *usagelog.Store {
	st, err := usagelog.Open(path)
	if err != nil {
		mainLog.Errorf("usage history store: %v (usage figures are not stored)", err)
		return nil
	}
	setUsageHistory(st)
	return st
}

func setUsageHistory(st *usagelog.Store) {
	usageHistoryMu.Lock()
	usageHistory = st
	usageHistoryMu.Unlock()
}

func currentUsageHistory() *usagelog.Store {
	usageHistoryMu.Lock()
	defer usageHistoryMu.Unlock()
	return usageHistory
}

// usageBindingFor is agent acfg's view of the usage history: its figures are
// stored under its usage account (Spec.UsageAccount for Claude Code, so the
// agents on one login share it and agents on other logins never do; codex:
// per agent), and /mana cached reads that account only. Nil for a backend
// with no usage query, or with no store.
func usageBindingFor(cfg *config.Config, acfg config.AgentConfig) *usagelog.Binding {
	return newUsageBinding(acfg.Backend, acfg.ID, usageTargetFor(cfg, acfg).Env)
}

// newUsageBinding is usageBindingFor from the agent's backend, id and
// backend_config.env (the global section folded in).
func newUsageBinding(backend, agentID string, env map[string]string) *usagelog.Binding {
	st := currentUsageHistory()
	if st == nil {
		return nil
	}
	if _, ok := delegator.UsageQueryFor(backend); !ok {
		return nil
	}
	lg := log.NewComponentLogger("usagelog/" + agentID)
	return &usagelog.Binding{
		Store:   st,
		Backend: backend,
		Account: usagelog.AccountKey(backend, agentID, usageAccountFor(agentID, backend, env)),
		AgentID: agentID,
		OnError: func(err error) { lg.Warnf("store a usage report: %v", err) },
	}
}
