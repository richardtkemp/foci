package account

import (
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/usagehold"
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
// resume find the same account (Dick 2026-11-19: "one session is tied to one
// account"). A new top-level session takes the pick (pick: the account with
// the most usage left). A child session (a branch, fork, spawn or batch: a
// child key <root>/b<ts> or <root>/i<ts>) takes its root's account the first
// time it is asked about, and stores it with itself: a child made before its
// root was reset stays on the account its files are in when the reset root
// moves to a new pick.
//
// A session stored before accounts existed has no record; the gateway records
// the pool's first account on those at startup (session.SessionIndex
// BackfillSessionMetadata), so only a new session takes the pick.
//
// A nil *Pool is the pool of an agent with no account: For returns nil.
type Pool struct {
	accounts []*Account
	store    BindingStore
	// OnLeft is told when a session stored on an account the agent no
	// longer has is moved to the pick (its files are out of reach, so it
	// starts again on now). Set before the first For; nil = log only.
	OnLeft func(sessionKey, was string, now *Account)

	mu    sync.Mutex
	bound map[string]*Account // by session key
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
// it is on none (see Pool). It is nil when the pool is nil or empty, and
// when the session's stored account cannot be read: the session is then on
// no known account for this call (open, no login owed) and nothing is bound
// or stored, so the next call reads again. Code that reaches a session's
// files or starts its backend uses Resolve, which returns that error.
func (p *Pool) For(sessionKey string) *Account {
	a, err := p.Resolve(sessionKey)
	if err != nil {
		poolLog.Warnf("read the account of session %s: %v", sessionKey, err)
	}
	return a
}

// Resolve is For with the read error returned: an account is never picked
// for a session whose stored account could not be read, since that would
// move it off its transcript.
func (p *Pool) Resolve(sessionKey string) (*Account, error) {
	if p == nil || len(p.accounts) == 0 {
		return nil, nil
	}
	p.mu.Lock()
	a := p.bound[sessionKey]
	p.mu.Unlock()
	if a != nil {
		return a, nil
	}
	stored := ""
	if p.store != nil && sessionKey != "" {
		var err error
		if stored, err = p.store.GetSessionMetadata(sessionKey, session.MetaKeyAccount); err != nil {
			return nil, err
		}
	}
	if a = p.byKey(stored); a != nil {
		return p.bind(sessionKey, a, false), nil
	}
	if parent := parentKey(sessionKey); parent != "" {
		pa, err := p.Resolve(parent)
		if err != nil || pa == nil {
			return pa, err
		}
		a = pa
	} else {
		// Asked outside every lock: an account's hold state can call its
		// subscribers, which ask this pool about their sessions.
		a = p.pick(time.Now())
	}
	a = p.bind(sessionKey, a, true)
	if stored != "" && a.Key() != stored {
		poolLog.Infof("session %s was on account %s, which the agent no longer has: it is now on %s", sessionKey, stored, a.Key())
		if p.OnLeft != nil {
			p.OnLeft(sessionKey, stored, a)
		}
	}
	return a, nil
}

// bind binds sessionKey to a unless a concurrent call bound it first, stores
// a new binding when store is set, and returns the session's account.
func (p *Pool) bind(sessionKey string, a *Account, store bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev := p.bound[sessionKey]; prev != nil {
		return prev
	}
	p.bound[sessionKey] = a
	if store && p.store != nil && sessionKey != "" {
		if err := p.store.SetSessionMetadata(sessionKey, session.MetaKeyAccount, a.Key()); err != nil {
			poolLog.Warnf("store the account of session %s: %v", sessionKey, err)
		}
	}
	return a
}

// Next is the account a new top-level session would take now (the pick),
// without binding anything; nil for a nil or empty pool.
func (p *Pool) Next() *Account {
	if p == nil || len(p.accounts) == 0 {
		return nil
	}
	return p.pick(time.Now())
}

// pick chooses the account of a new top-level session (#3186, Dick
// 2026-11-19: "each new session chooses the account with the most usage
// remaining"):
//
//   - an account that owes a login is passed over while another does not;
//     when every account owes one, the first is taken (its login starts as
//     for any session, RULINGS 4 Q7);
//   - of the rest, an account that holds or stops is passed over while
//     another is open; of the open ones the least used wins (Account.Used,
//     the last figures its sessions reported, RULINGS 3 Q2 and Q3; an
//     account with no figure counts as unused);
//   - when every one holds or stops, the one whose hold lifts first wins
//     (Q4; a hold with no known lift time comes last);
//   - a tie goes to the first in the agent's list.
func (p *Pool) pick(now time.Time) *Account {
	if len(p.accounts) == 1 {
		return p.accounts[0]
	}
	var cands []*Account
	for _, a := range p.accounts {
		if !a.Login().Pending() {
			cands = append(cands, a)
		}
	}
	if len(cands) == 0 {
		return p.accounts[0]
	}
	var (
		best     *Account
		bestOpen bool
		bestUsed float64
		bestLift time.Time
	)
	for _, a := range cands {
		st := a.Hold()
		open := st.Phase == usagehold.Open
		var used float64
		if open {
			used, _ = a.Used(now)
		}
		if best != nil {
			switch {
			case open != bestOpen:
				if !open {
					continue
				}
			case open:
				if used >= bestUsed {
					continue
				}
			case !liftsBefore(st.LiftAt, bestLift):
				continue
			}
		}
		best, bestOpen, bestUsed, bestLift = a, open, used, st.LiftAt
	}
	return best
}

// liftsBefore reports whether a hold lifting at x lifts strictly before one
// lifting at y; a zero time (unknown) lifts after every known one.
func liftsBefore(x, y time.Time) bool {
	switch {
	case x.IsZero():
		return false
	case y.IsZero():
		return true
	}
	return x.Before(y)
}

func (p *Pool) byKey(key string) *Account {
	for _, a := range p.accounts {
		if key != "" && a.Key() == key {
			return a
		}
	}
	return nil
}

// Forget ends the binding of sessionKey whose stored state was cleared (a
// reset of a top-level session): the next For binds it as a new session, on
// the pick. Its children keep the accounts they are on.
func (p *Pool) Forget(sessionKey string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.bound, sessionKey)
	p.mu.Unlock()
}

// Moved carries the binding of oldKey over to newKey when a session is
// re-keyed (DelegatedManager.RemapSession), so the session stays on its
// account.
func (p *Pool) Moved(oldKey, newKey string) {
	if p == nil || len(p.accounts) == 0 || oldKey == newKey {
		return
	}
	a, err := p.Resolve(oldKey)
	if err != nil || a == nil {
		poolLog.Warnf("carry the account of session %s over to %s: %v", oldKey, newKey, err)
		return
	}
	p.mu.Lock()
	delete(p.bound, newKey)
	p.mu.Unlock()
	p.bind(newKey, a, true)
}

var poolLog = log.NewComponentLogger("account")

// parentKey is the key of the session sessionKey is a child of (its root),
// or "" when it is a top-level session or does not parse as a session key.
func parentKey(sessionKey string) string {
	sk, err := session.ParseSessionKey(sessionKey)
	if err != nil || sk.IsRoot() {
		return ""
	}
	return sk.Root().String()
}
