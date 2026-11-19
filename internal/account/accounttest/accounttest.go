// Package accounttest builds accounts for tests.
package accounttest

import (
	"foci/internal/account"
	"foci/internal/delegator"
	"foci/internal/relogin"
	"foci/internal/usagehold"
)

// One returns one account of backend, as the gateway builds it for an agent
// with one login (#3186). The zero arguments are the gateway's values for the
// backend's host login: key = the backend's name, holds = usagehold.Default,
// login = relogin.G. A key given by the test is a login with a name of its
// own (a Claude Code login): its name is login's, else the key. Accounts
// given equal keys and one holds share the hold, as agents on one login do.
func One(backend, key string, holds *usagehold.Registry, login *relogin.Gate) *account.Account {
	spec := delegator.AccountSpec{Key: key, Host: key == ""}
	if key == "" {
		spec.Key = backend
		if spec.Key == "" {
			spec.Key = "test-backend"
		}
	} else {
		spec.Name = key
		if login != nil && login.Name() != "" {
			spec.Name = login.Name()
		}
	}
	if login == nil {
		login = relogin.G
	}
	return account.New(account.Options{Backend: backend, Spec: spec, Login: login, Holds: holds})
}

// Pool returns the pool of an agent whose sessions are all on One(...), with
// its bindings in memory.
func Pool(backend, key string, holds *usagehold.Registry, login *relogin.Gate) *account.Pool {
	return account.NewPool(nil, One(backend, key, holds, login))
}
