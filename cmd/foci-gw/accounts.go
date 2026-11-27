package main

import (
	"sync"
	"time"

	"foci/internal/account"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/relogin"
	"foci/internal/session"
	"foci/internal/usagelog"
)

// Accounts (#3186). An account is one login of a delegated backend
// (internal/account): a Claude Code login location (Dick 2026-11-14, Q11),
// or the one login of codex or opencode. gwAccounts holds every account in
// use, so agents on one login share one Account: one re-login gate (Q13: a
// login holds only that account's sessions), one tmux session for its login
// pane (two accounts' logins can run at once), one sign-in expiry warner
// (Q16), one usage hold and one usage history.
//
// An agent has a pool of accounts (agentAccounts): the folders its
// backend_config.accounts lists, else the host's account. The host's Claude
// Code account keeps relogin.G, the tmux session "foci-cc-login" and
// data/cc-login-expiry.json, so an install that lists no folder behaves as
// before and an upgrade keeps the expiry stages it already sent (H9).
var gwAccounts = &account.Registry{History: currentUsageHistory}

// agentAccounts returns the accounts of an agent on spec's backend whose
// backend_config.accounts is folders, in order: one per folder, or the
// host's when it lists none. A backend with one login for all its agents
// (no Spec.Account: codex, opencode) has that one; the config check warns that
// it ignores the list. A folder whose account cannot be resolved is
// logged and replaced by the login named after the backend.
func agentAccounts(cfg *config.Config, spec delegator.Spec, agentID string, folders []string) []*account.Account {
	if len(folders) == 0 || spec.Account == nil {
		folders = []string{""}
	}
	out := make([]*account.Account, 0, len(folders))
	for _, folder := range folders {
		as, err := delegator.AccountOf(spec, folder)
		if err != nil {
			log.NewComponentLogger("account/"+agentID).Warnf("resolve the %s account of folder %q: %v; using the login named after the backend", spec.DisplayName, folder, err)
			as = delegator.AccountSpec{Key: spec.Name, Host: true}
		}
		out = append(out, accountFor(cfg, spec, as))
	}
	return out
}

// accountFor returns the process's Account for as, a login of spec's
// backend, making it on first use. The expiry state lives in cfg's data
// folder.
func accountFor(cfg *config.Config, spec delegator.Spec, as delegator.AccountSpec) *account.Account {
	o := account.Options{Backend: spec.Name, Spec: as}
	switch {
	case spec.Account == nil:
		// One login for all the backend's calls.
		o.Spend = backendSpend(spec.Name)
	case as.Dir != "":
		// The calls whose transcript is in the login's folder (#3291): what
		// its cap in its agents' usage pace is learned from.
		o.Spend = folderSpend(as.Dir)
	}
	if spec.Supports(delegator.CapRelogin) {
		if as.Host {
			o.Login = relogin.G
			o.LoginSession = relogin.DefaultSession
			o.ExpiryWarner = relogin.NewExpiryWarner(cfg.DataPath(hostLoginExpiryFile))
		} else {
			o.LoginSession = relogin.DefaultSession + "-" + as.ID
			o.ExpiryWarner = relogin.NewExpiryWarner(cfg.DataPath("cc-login-expiry-" + as.ID + ".json"))
		}
	}
	return gwAccounts.Get(o)
}

// backendSpend is the account.Options.Spend of backend's one login: its calls
// in the cost ledger (#3269). With no ledger there are none, so no rate is
// learned from them and the login has no usage pace.
func backendSpend(backend string) func(from, to time.Time) ([]account.Spend, error) {
	return ledgerSpend(func(l *accounting.Ledger, from, to time.Time) ([]accounting.CallCost, error) {
		return l.BackendCosts(backend, from, to)
	})
}

// folderSpend is the account.Options.Spend of the Claude Code login whose
// config folder is dir: the calls whose transcript is in it (#3291). With no
// ledger there are none, so no cap is learned from them.
func folderSpend(dir string) func(from, to time.Time) ([]account.Spend, error) {
	return ledgerSpend(func(l *accounting.Ledger, from, to time.Time) ([]accounting.CallCost, error) {
		return l.FolderCosts(dir, from, to)
	})
}

// ledgerSpend is an account.Options.Spend of the calls read reads from the
// live cost ledger; none with no ledger.
func ledgerSpend(read func(l *accounting.Ledger, from, to time.Time) ([]accounting.CallCost, error)) func(from, to time.Time) ([]account.Spend, error) {
	return func(from, to time.Time) ([]account.Spend, error) {
		l := accounting.Live()
		if l == nil {
			return nil, nil
		}
		costs, err := read(l, from, to)
		out := make([]account.Spend, len(costs))
		for i, c := range costs {
			out[i] = account.Spend{At: c.BilledAt, Priced: c.USD != nil}
			if c.USD != nil {
				out[i].USD = *c.USD
			}
		}
		return out, err
	}
}

// agentPool is the pool of agentID's accounts, keeping which session is on
// which in idx's session metadata (memory only with no index). Every session
// of the agent with a backend session (a resume id) and no account stored is
// first recorded on the first account: it was made before sessions had
// accounts, when the agent had one, which an agent that adds accounts lists
// first (docs/AUTH.md). Without that, its files would be looked for on
// whatever account a new session would take.
func agentPool(idx *session.SessionIndex, agentID string, accounts []*account.Account) *account.Pool {
	if idx == nil {
		return account.NewPool(nil, accounts...).ForAgent(agentID)
	}
	if len(accounts) > 0 {
		n, err := idx.BackfillSessionMetadata(agentID, session.MetaKeyCCResumeID, session.MetaKeyAccount, accounts[0].Key())
		switch {
		case err != nil:
			log.NewComponentLogger("account/"+agentID).Warnf("record the account of the sessions made before accounts: %v", err)
		case n > 0:
			log.NewComponentLogger("account/"+agentID).Infof("recorded %d session(s) made before accounts on account %s", n, accounts[0].Key())
		}
	}
	return account.NewPool(idx, accounts...).ForAgent(agentID)
}

// masterOnAccount returns cfg's master agent when acct is one of its
// accounts, else "". A sign-in expiry notice goes to the master only then,
// and otherwise to an agent on that account (Dick 2026-11-14, Q16). A master
// that does not run acct's backend counts as on that backend's host account:
// it is where the API agents' CC token fallback reads, and an install that
// lists no folder then sends the notice to the master as before.
func masterOnAccount(cfg *config.Config, acct *account.Account) string {
	if cfg.MasterAgent == "" {
		return ""
	}
	spec, ok := delegator.SpecFor(acct.Backend())
	if !ok {
		return ""
	}
	master := gwAgentRenames.resolve(cfg.MasterAgent)
	folders := []string{""}
	for _, a := range cfg.Agents {
		if a.ID == master && a.Backend == acct.Backend() && len(a.BackendConfig.Accounts) > 0 {
			folders = a.BackendConfig.Accounts
		}
	}
	for _, folder := range folders {
		as, err := delegator.AccountOf(spec, folder)
		if err != nil {
			log.NewComponentLogger("relogin").Warnf("resolve the master agent's %s account: %v", spec.DisplayName, err)
			continue
		}
		if as.Key == acct.Key() {
			return cfg.MasterAgent
		}
	}
	return ""
}

// usageHistory is the usage history store (#3103): every usage figure the
// gateway receives from a backend with /usage is written to it, under the
// account it belongs to (account.Account.Readings). Opened at startup
// (openUsageHistory); nil when it could not be opened (then nothing is
// stored, and /mana cached says it holds nothing).
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
	migrateUsageHistoryKeys(st)
	setUsageHistory(st)
	return st
}

// migrateUsageHistoryKeys moves the figures of every backend with one login
// for all its agents (Spec.Account nil: codex) from the per-agent keys they
// were stored under ("<backend>/<agent id>") to the backend's one account
// (#3186), so the history an upgrade finds stays readable. Once moved,
// nothing matches, and a later start writes nothing.
func migrateUsageHistoryKeys(st *usagelog.Store) {
	for _, spec := range delegator.Specs() {
		if spec.Account != nil || spec.UsageQuery == nil {
			continue
		}
		n, err := st.MergeAgentAccounts(spec.Name)
		switch {
		case err != nil:
			mainLog.Warnf("usage history: move the per-agent %s figures to account %s: %v", spec.Name, spec.Name, err)
		case n > 0:
			mainLog.Infof("usage history: moved %d per-agent %s report(s) to account %s", n, spec.Name, spec.Name)
		}
	}
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

// agentBackendConfig is acfg's backend_config with the global section of
// its backend folded in, as the backend itself resolves it.
func agentBackendConfig(cfg *config.Config, acfg config.AgentConfig) config.BackendConfig {
	bc := acfg.BackendConfig
	if spec, ok := delegator.SpecFor(acfg.Backend); ok {
		bc = foldGlobalBackendConfig(cfg, spec, bc)
	}
	return bc
}

// usageTarget is what a usage read of acct by agent acfg needs: the agent's
// id, the launch env of a session on acct and the agent's binary.
func usageTarget(cfg *config.Config, acfg config.AgentConfig, acct *account.Account) delegator.UsageTarget {
	bc := agentBackendConfig(cfg, acfg)
	return delegator.UsageTarget{AgentID: acfg.ID, Env: acct.LaunchEnv(bc.Env), Binary: config.DerefStr(bc.Binary)}
}
