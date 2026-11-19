package account

import (
	"sync"

	"foci/internal/log"
	"foci/internal/session"
)

// BindingStore is where a Pool keeps which account each session is on
// (session.SessionIndex's session metadata).
type BindingStore interface {
	GetSessionMetadata(sessionKey, key string) (string, error)
	SetSessionMetadata(sessionKey, key, value string) error
}

// Pool is one agent's accounts, and which of its sessions is on which.
//
// A session's account is fixed the first time the session is asked about
// (For) and stored with it (session.MetaKeyAccount), so a restart and a
// resume find the same account. It is bound to the session's ROOT key: a
// child key (<root>/b<ts>, <root>/i<ts>) names its root, so every descendant
// of a session is on that session's account without a record of its own. A
// session stored before accounts existed has no record, and takes the pick.
//
// A nil *Pool is the pool of an agent with no account: For returns nil.
type Pool struct {
	accounts []*Account
	store    BindingStore

	mu    sync.Mutex
	bound map[string]*Account // by root session key
}

// NewPool returns the pool of accounts (in the agent's configured order),
// keeping its bindings in store; a nil store keeps them in memory only.
func NewPool(store BindingStore, accounts ...*Account) *Pool {
	return &Pool{accounts: accounts, store: store, bound: map[string]*Account{}}
}

// Accounts returns the pool's accounts, in order.
func (p *Pool) Accounts() []*Account {
	if p == nil {
		return nil
	}
	return p.accounts
}

// For returns the account sessionKey is on, binding the session first when
// it is on none (or on one that left the pool: its files are then out of
// reach, and it starts on the pick as a new session would).
func (p *Pool) For(sessionKey string) *Account {
	if p == nil || len(p.accounts) == 0 {
		return nil
	}
	root := rootKey(sessionKey)
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.bound[root]; a != nil {
		return a
	}
	stored := ""
	if p.store != nil && root != "" {
		stored, _ = p.store.GetSessionMetadata(root, session.MetaKeyAccount)
	}
	a := p.byKey(stored)
	if a == nil {
		a = p.pick()
		if stored != "" {
			poolLog.Infof("session %s was on account %s, which the agent no longer has: it is now on %s", root, stored, a.Key())
		}
		if p.store != nil && root != "" {
			if err := p.store.SetSessionMetadata(root, session.MetaKeyAccount, a.Key()); err != nil {
				poolLog.Warnf("store the account of session %s: %v", root, err)
			}
		}
	}
	p.bound[root] = a
	return a
}

// pick chooses the account of a new session: the pool's first. (#3186 phase
// 2 puts the usage rule here.)
func (p *Pool) pick() *Account { return p.accounts[0] }

func (p *Pool) byKey(key string) *Account {
	for _, a := range p.accounts {
		if key != "" && a.Key() == key {
			return a
		}
	}
	return nil
}

// Forget ends the binding of sessionKey when it is a root key whose stored
// state was cleared (a reset): the next For binds it as a new session.
func (p *Pool) Forget(sessionKey string) {
	if p == nil || rootKey(sessionKey) != sessionKey {
		return
	}
	p.mu.Lock()
	delete(p.bound, sessionKey)
	p.mu.Unlock()
}

// Moved carries the binding of oldKey over to newKey when a session is
// re-keyed (DelegatedManager.RemapSession), so the session stays on its
// account. A no-op when both keys have one root.
func (p *Pool) Moved(oldKey, newKey string) {
	if p == nil || len(p.accounts) == 0 {
		return
	}
	a := p.For(oldKey)
	from, to := rootKey(oldKey), rootKey(newKey)
	if from == to {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bound[to] = a
	if p.store != nil && to != "" {
		if err := p.store.SetSessionMetadata(to, session.MetaKeyAccount, a.Key()); err != nil {
			poolLog.Warnf("store the account of session %s: %v", to, err)
		}
	}
}

var poolLog = log.NewComponentLogger("account")

// rootKey is the key a session's account is bound to: its root, or the key
// itself when it does not parse as a session key.
func rootKey(sessionKey string) string {
	sk, err := session.ParseSessionKey(sessionKey)
	if err != nil {
		return sessionKey
	}
	return sk.Root().String()
}
