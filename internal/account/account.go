// Package account is foci's model of a login (#3186).
//
// An Account is ONE login of a delegated backend, and it owns everything
// that belongs to a login: where the backend keeps that login's files (for
// Claude Code, its config folder), the interactive login (its gate, the tmux
// session of its pane and what that pane is told), the sign-in expiry
// warner, the usage hold, the usage readings and the name people read.
//
// A session is on exactly one account, fixed when the session is first asked
// about and stored with it; every descendant of a session (spawn, branch,
// fork, batch, summary, keepalive or reflection fork) is on its account
// (Pool). An agent has a Pool and no account of its own: every question that
// depends on an account (may this turn run, is a login owed, where is this
// transcript) is asked of a session.
//
// A nil *Account is "no account": the state of a session of an agent that
// runs on no delegated backend (an API agent). Its hold is always open, its
// login is never owed and it has no folder, so callers need no branch for it.
package account

import (
	"strings"
	"sync"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/relogin"
	"foci/internal/usagehold"
	"foci/internal/usagelog"
)

// Account is one login. Make one with New (tests) or Registry.Get.
type Account struct {
	spec    delegator.AccountSpec
	backend string

	login   *relogin.Gate
	session string
	warner  *relogin.ExpiryWarner

	holds   *usagehold.Registry
	history func() *usagelog.Store
	spend   func(from, to time.Time) ([]Spend, error)
}

// Options is what New builds an Account from.
type Options struct {
	// Backend is the Spec name of the backend the login belongs to.
	Backend string
	// Spec is what the backend says about the login (Spec.Account).
	Spec delegator.AccountSpec
	// Login is the account's interactive-login gate; nil = a new one.
	Login *relogin.Gate
	// LoginSession names the tmux session of the login pane.
	LoginSession string
	// ExpiryWarner warns before the sign-in ends; nil = none.
	ExpiryWarner *relogin.ExpiryWarner
	// Holds is the usage hold engine; nil = usagehold.Default.
	Holds *usagehold.Registry
	// History returns the usage history store, nil when there is none; a nil
	// func = no store.
	History func() *usagelog.Store
	// Spend lists the account's API calls billed in (from, to], oldest first
	// (pace_spend.go; the 7d percent per dollar learned from them is also the
	// login's cap in its pool's usage pace, #3291); nil when the calls of this
	// one account are not known.
	Spend func(from, to time.Time) ([]Spend, error)
}

// New returns the account o describes.
func New(o Options) *Account {
	a := &Account{
		spec: o.Spec, backend: o.Backend,
		login: o.Login, session: o.LoginSession, warner: o.ExpiryWarner,
		holds: o.Holds, history: o.History, spend: o.Spend,
	}
	if a.login == nil {
		a.login = &relogin.Gate{}
	}
	if a.holds == nil {
		a.holds = usagehold.Default
	}
	if o.Spec.Name != "" {
		a.login.SetName(o.Spec.Name)
	}
	if a.warner != nil {
		a.warner.SetAccountName(a.login.Name)
	}
	return a
}

// noLogin is the gate of "no account": nothing ever starts a login on it.
var noLogin = &relogin.Gate{}

// Key identifies the account: equal keys are one account. The usage hold and
// the usage readings are stored under it. "" for no account.
func (a *Account) Key() string {
	if a == nil {
		return ""
	}
	return a.spec.Key
}

// Backend is the Spec name of the account's backend ("" for no account).
func (a *Account) Backend() string {
	if a == nil {
		return ""
	}
	return a.backend
}

// Spec is what the backend says about the login: handed to a backend
// instance that runs on this account (delegator.AccountAcceptor).
func (a *Account) Spec() delegator.AccountSpec {
	if a == nil {
		return delegator.AccountSpec{}
	}
	return a.spec
}

// Dir is the folder where the backend keeps this login's files (Claude
// Code: its config folder, with projects/ and settings.json), "" when the
// backend has no such folder.
func (a *Account) Dir() string { return a.Spec().Dir }

// LaunchEnv is base (an agent's backend_config.env) with the account's own
// part on top: the launch env of a session on this account. base is not
// changed.
func (a *Account) LaunchEnv(base map[string]string) map[string]string {
	env := a.Spec().Env
	if len(env) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(env))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range env {
		out[k] = v
	}
	return out
}

// Name names the account for a person: its email once the backend reported
// one, else its location; "" when the account has no name of its own (a
// backend whose agents all share one login, or no account).
func (a *Account) Name() string {
	if a == nil || a.spec.Name == "" {
		return ""
	}
	return a.login.Name()
}

// WithName appends " (account: <name>)" to text when the account has a name.
func (a *Account) WithName(text string) string {
	if name := a.Name(); name != "" {
		return text + " (account: " + name + ")"
	}
	return text
}

// Login is the gate of the account's interactive login. It is never nil; on
// an account whose backend has no interactive login nothing starts it.
func (a *Account) Login() *relogin.Gate {
	if a == nil {
		return noLogin
	}
	return a.login
}

// LoginSession names the tmux session of the account's login pane, and
// LoginEnv is what that pane is told (relogin.Config.Env).
func (a *Account) LoginSession() string {
	if a == nil {
		return ""
	}
	return a.session
}

// LoginEnv: see LoginSession.
func (a *Account) LoginEnv() []string { return a.Spec().LoginEnv }

// ExpiryWarner warns before the account's sign-in ends; nil when the
// backend reports no end date.
func (a *Account) ExpiryWarner() *relogin.ExpiryWarner {
	if a == nil {
		return nil
	}
	return a.warner
}

// --- usage hold ---

// Hold is the account's usage hold state now (open for no account).
func (a *Account) Hold() usagehold.State {
	if a == nil {
		return usagehold.State{}
	}
	return a.holds.State(a.spec.Key, time.Now())
}

// ObserveUsage records one usage window the backend reported.
func (a *Account) ObserveUsage(ev usagehold.Event) {
	if a == nil {
		return
	}
	a.holds.Observe(a.spec.Key, ev, time.Now())
}

// ClearHold lifts the account's hold at once (usagehold.Registry.Clear) and
// returns the phase before and the state after.
func (a *Account) ClearHold() (usagehold.Phase, usagehold.State) {
	if a == nil {
		return usagehold.Open, usagehold.State{}
	}
	return a.holds.Clear(a.spec.Key, time.Now())
}

// OnHoldChange calls fn on every phase change of the account's hold, and
// OnLimitReset on every ClearHold; each returns the function that ends the
// subscription (usagehold.Registry.Subscribe, SubscribeReset).
func (a *Account) OnHoldChange(fn func(usagehold.Change)) func() {
	if a == nil {
		return func() {}
	}
	return a.holds.Subscribe(a.spec.Key, fn)
}

// OnLimitReset: see OnHoldChange.
func (a *Account) OnLimitReset(fn func()) func() {
	if a == nil {
		return func() {}
	}
	return a.holds.SubscribeReset(a.spec.Key, fn)
}

// HoldSettings is the hold engine's configuration (threshold, resume idle
// limit) and its clock tick; never nil.
func (a *Account) HoldSettings() *usagehold.Registry {
	if a == nil {
		return usagehold.Default
	}
	return a.holds
}

// --- usage readings ---

// Readings is the account's usage history as agentID writes and reads it:
// every figure is stored under the account, stamped with the agent that
// received it. Nil when the backend has no usage query or there is no store
// (a nil Binding stores nothing and reads as empty).
func (a *Account) Readings(agentID string) *usagelog.Binding {
	if a == nil || a.history == nil {
		return nil
	}
	st := a.history()
	if st == nil {
		return nil
	}
	if _, ok := delegator.UsageQueryFor(a.backend); !ok {
		return nil
	}
	agentID = strings.TrimSpace(agentID)
	lg := log.NewComponentLogger("usagelog/" + agentID)
	return &usagelog.Binding{
		Store:   st,
		Backend: a.backend,
		Account: a.spec.Key,
		AgentID: agentID,
		OnError: func(err error) { lg.Warnf("store a usage report: %v", err) },
	}
}

// Registry holds every account of the process by Key, so that agents on one
// login share one Account. Holds and History are what its accounts get when
// Get's Options leave them unset (nil Holds = usagehold.Default).
type Registry struct {
	Holds   *usagehold.Registry
	History func() *usagelog.Store

	mu sync.Mutex
	m  map[string]*Account
}

// Get returns the account that o.Spec describes, making it from o the first
// time its key is asked for (later calls return the same Account and ignore
// the rest of o).
func (r *Registry) Get(o Options) *Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.m[o.Spec.Key]; a != nil {
		return a
	}
	if o.Holds == nil {
		o.Holds = r.Holds
	}
	if o.History == nil {
		o.History = r.History
	}
	a := New(o)
	if r.m == nil {
		r.m = map[string]*Account{}
	}
	r.m[o.Spec.Key] = a
	return a
}
