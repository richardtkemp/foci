package account

import (
	"testing"

	"foci/internal/delegator"
	"foci/internal/session"
	"foci/internal/usagehold"
)

// memStore is a BindingStore in memory, as the session index's metadata is.
type memStore map[string]string

func (m memStore) GetSessionMetadata(key, name string) (string, error) {
	return m[key+"\x00"+name], nil
}

func (m memStore) SetSessionMetadata(key, name, value string) error {
	m[key+"\x00"+name] = value
	return nil
}

func testAccount(key string) *Account {
	return New(Options{Backend: "claude-code", Spec: delegator.AccountSpec{Key: key, Name: key}, Holds: usagehold.New()})
}

// TestPool_SessionAndChildrenShareTheStoredAccount (#3186 P2): a session is
// bound to one account the first time it is asked about, the binding is
// stored on its root key, and every descendant (branch, spawn, batch: a
// child key of the root) is on the same account.
func TestPool_SessionAndChildrenShareTheStoredAccount(t *testing.T) {
	a, b := testAccount("a"), testAccount("b")
	st := memStore{}
	p := NewPool(st, a, b)
	const root = "agent/c1"
	if got := p.For(root); got != a {
		t.Fatalf("new session on %v, want the pool's first", got.Key())
	}
	if v := st[root+"\x00"+session.MetaKeyAccount]; v != "a" {
		t.Errorf("stored account = %q, want a", v)
	}
	for _, child := range []string{root + "/b1709596800", root + "/i1709596801"} {
		if got := p.For(child); got != a {
			t.Errorf("child %s on %v, want its root's account", child, got.Key())
		}
	}
	if len(st) != 1 {
		t.Errorf("store holds %v, want the root's binding only", st)
	}
}

// TestPool_StoredBindingSurvivesARestart (#3186 P2, the #3104 F1 risk): a
// session stored on an account other than the pool's first is on that
// account in a new pool (a restart) too, and so are its children. Were it
// taken to the first account instead, its transcript, forks and cleanup
// would resolve in another account's folder.
func TestPool_StoredBindingSurvivesARestart(t *testing.T) {
	a, b := testAccount("a"), testAccount("b")
	st := memStore{"agent/c1\x00" + session.MetaKeyAccount: "b"}
	p := NewPool(st, a, b)
	if got := p.For("agent/c1"); got != b {
		t.Errorf("stored session on %v, want b", got.Key())
	}
	if got := p.For("agent/c1/b17"); got != b {
		t.Errorf("its child on %v, want b", got.Key())
	}
	if got := p.For("agent/c2"); got != a {
		t.Errorf("new session on %v, want a", got.Key())
	}
}

// TestPool_AccountLeftThePool: a session stored on an account the agent no
// longer has starts on the pick, and the new binding is stored.
func TestPool_AccountLeftThePool(t *testing.T) {
	a := testAccount("a")
	st := memStore{"agent/c1\x00" + session.MetaKeyAccount: "gone"}
	p := NewPool(st, a)
	if got := p.For("agent/c1"); got != a {
		t.Fatalf("session on %v, want a", got.Key())
	}
	if v := st["agent/c1\x00"+session.MetaKeyAccount]; v != "a" {
		t.Errorf("stored account = %q, want a", v)
	}
}

// TestPool_ForgetAndMoved: a reset root forgets its binding (a child key
// does not: its root's stays), and a re-keyed session keeps its account.
func TestPool_ForgetAndMoved(t *testing.T) {
	a, b := testAccount("a"), testAccount("b")
	st := memStore{"agent/c1\x00" + session.MetaKeyAccount: "b"}
	p := NewPool(st, a, b)
	p.For("agent/c1")

	p.Forget("agent/c1/b5")
	if got := p.For("agent/c1"); got != b {
		t.Errorf("after a child's Forget the root is on %v, want b", got.Key())
	}

	p.Moved("agent/c1", "agent/c9")
	if got := p.For("agent/c9"); got != b {
		t.Errorf("re-keyed session on %v, want b", got.Key())
	}
	if v := st["agent/c9\x00"+session.MetaKeyAccount]; v != "b" {
		t.Errorf("stored account of the new key = %q, want b", v)
	}

	// Reset: the stored state is cleared (ClearSessionState), then Forget.
	delete(st, "agent/c1\x00"+session.MetaKeyAccount)
	p.Forget("agent/c1")
	if got := p.For("agent/c1"); got != a {
		t.Errorf("reset session on %v, want the pick (a)", got.Key())
	}
}

// TestNilPoolAndNilAccount: an agent with no account (an API agent) has a
// nil pool; its sessions are on the nil account, which is always open and
// owes no login.
func TestNilPoolAndNilAccount(t *testing.T) {
	var p *Pool
	acct := p.For("agent/c1")
	if acct != nil {
		t.Fatalf("nil pool gave %v", acct)
	}
	if acct.Key() != "" || acct.Hold().Phase != usagehold.Open || acct.Login().Active() || acct.Dir() != "" || acct.Readings("x") != nil {
		t.Errorf("nil account is not the open, login-free no-account")
	}
	if got := acct.WithName("text"); got != "text" {
		t.Errorf("WithName = %q", got)
	}
	base := map[string]string{"K": "v"}
	if env := acct.LaunchEnv(base); len(env) != 1 || env["K"] != "v" {
		t.Errorf("LaunchEnv = %v, want base", env)
	}
}

// TestLaunchEnvPutsTheAccountOnTop: a session's launch env is the agent's
// env with its account's part on top, and the agent's map is not changed.
func TestLaunchEnvPutsTheAccountOnTop(t *testing.T) {
	acct := New(Options{Spec: delegator.AccountSpec{Key: "k", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/acct"}}})
	base := map[string]string{"CLAUDE_CONFIG_DIR": "/agent", "X": "1"}
	env := acct.LaunchEnv(base)
	if env["CLAUDE_CONFIG_DIR"] != "/acct" || env["X"] != "1" {
		t.Errorf("LaunchEnv = %v", env)
	}
	if base["CLAUDE_CONFIG_DIR"] != "/agent" {
		t.Errorf("base changed: %v", base)
	}
}

// TestRegistryOneAccountPerKey: agents on one login share one Account.
func TestRegistryOneAccountPerKey(t *testing.T) {
	r := &Registry{Holds: usagehold.New()}
	x := r.Get(Options{Spec: delegator.AccountSpec{Key: "k"}})
	y := r.Get(Options{Spec: delegator.AccountSpec{Key: "k"}})
	z := r.Get(Options{Spec: delegator.AccountSpec{Key: "other"}})
	if x != y || x == z {
		t.Errorf("Get: same key gave distinct accounts, or distinct keys one")
	}
}
