package main

import (
	"sync"

	"foci/internal/ccaccount"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/relogin"
)

// Per-account Claude Code login (#3104 phase 2). An account is a login
// location (Dick 2026-11-14, Q11), resolved by ccaccount from an agent's
// backend_config.env. Each account in use has one loginAccount: one re-login
// gate (Q13: a login holds only that account's agents), one tmux session for
// its login pane (two accounts' logins can run at once) and one sign-in expiry
// warner (Q16). The host account (agents with no CC folder of their own) keeps
// relogin.G, the tmux session "foci-cc-login" and data/cc-login-expiry.json,
// so an install with no override behaves as before (requirement 8) and an
// upgrade keeps the stages it already sent (H9).

// loginAccount is one Claude Code account's login state.
type loginAccount struct {
	account ccaccount.Account
	gate    *relogin.Gate
	session string // tmux session of its login pane
	warner  *relogin.ExpiryWarner
}

// loginAccounts holds the process's accounts by ccaccount.Account.Key.
type loginAccounts struct {
	mu sync.Mutex
	m  map[string]*loginAccount
}

var gwLoginAccounts = &loginAccounts{}

// hostCCAccount is the account of an agent with no override.
func hostCCAccount() (ccaccount.Account, error) {
	loc, err := ccaccount.ForAgent(nil)
	return loc.Account, err
}

// forAgent returns the login state of the account an agent with
// backend_config.env agentEnv uses, creating it on first use. The expiry
// state lives in cfg's data folder.
func (l *loginAccounts) forAgent(cfg *config.Config, agentEnv map[string]string) (*loginAccount, error) {
	loc, err := ccaccount.ForAgent(agentEnv)
	if err != nil {
		return nil, err
	}
	host, err := hostCCAccount()
	if err != nil {
		return nil, err
	}
	key := loc.Account.Key()
	l.mu.Lock()
	defer l.mu.Unlock()
	if la := l.m[key]; la != nil {
		return la, nil
	}
	la := &loginAccount{account: loc.Account}
	if loc.Account == host {
		la.gate = relogin.G
		la.session = relogin.DefaultSession
		la.warner = relogin.NewExpiryWarner(cfg.DataPath(hostLoginExpiryFile))
	} else {
		la.gate = &relogin.Gate{}
		la.session = relogin.DefaultSession + "-" + loc.Account.ShortKey()
		la.warner = relogin.NewExpiryWarner(cfg.DataPath("cc-login-expiry-" + loc.Account.ShortKey() + ".json"))
	}
	la.gate.SetName(loc.Account.String())
	la.warner.SetAccountName(la.gate.Name)
	if l.m == nil {
		l.m = map[string]*loginAccount{}
	}
	l.m[key] = la
	return la, nil
}

// masterOnAccount returns cfg's master agent when it is on account acct, else
// "". A sign-in expiry notice goes to the master only then, and otherwise to
// an agent on that account (Dick 2026-11-14, Q16). A master that does not run
// Claude Code counts as on the host account: it is where the API agents' CC
// token fallback reads, and an install with no override then sends the notice
// to the master as before.
func masterOnAccount(cfg *config.Config, acct ccaccount.Account) string {
	if cfg.MasterAgent == "" {
		return ""
	}
	master := gwAgentRenames.resolve(cfg.MasterAgent)
	var env map[string]string
	for _, a := range cfg.Agents {
		if a.ID != master {
			continue
		}
		if spec, ok := delegator.SpecFor(a.Backend); ok && spec.Supports(delegator.CapRelogin) {
			env = a.BackendConfig.Env
		}
	}
	loc, err := ccaccount.ForAgent(env)
	if err != nil {
		log.NewComponentLogger("relogin").Warnf("resolve the master agent's Claude Code account: %v", err)
		return ""
	}
	if loc.Account != acct {
		return ""
	}
	return cfg.MasterAgent
}
