package agent

import (
	"foci/internal/account"
	"foci/internal/account/accounttest"
	"foci/internal/relogin"
	"foci/internal/usagehold"
)

// putOnAccount puts every session of a (a delegated agent: its
// DelegatedManager is set) on one account (accounttest.One, which documents
// the zero arguments), and returns it.
func putOnAccount(a *Agent, key string, reg *usagehold.Registry, gate *relogin.Gate) *account.Account {
	a.DelegatedManager.Accounts = accounttest.Pool(a.DelegatedManager.BackendType, key, reg, gate)
	return testAcct(a)
}

// testAcct is the one account of a test agent (nil when it has none).
func testAcct(a *Agent) *account.Account { return a.AccountFor("") }
