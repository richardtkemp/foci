package route

import (
	"testing"

	"foci/internal/platform"
)

// fakeConn is a minimal platform.Connection stand-in for routing tests; only
// identity matters, so the methods are inert.
type fakeConn struct {
	platform.Connection
	name string
}

// fakeConnMgr implements the ConnectionManager lookups ConnFor/Broadcast use.
type fakeConnMgr struct {
	platform.ConnectionManager
	bySession map[string]platform.Connection
	primary   platform.Connection
	all       []platform.Connection
}

func (f *fakeConnMgr) ForSession(sk string) platform.Connection { return f.bySession[sk] }
func (f *fakeConnMgr) ForSessionOrPrimary(sk, agentID string) platform.Connection {
	if c := f.bySession[sk]; c != nil {
		return c
	}
	return f.primary
}
func (f *fakeConnMgr) AllForAgent(agentID string) []platform.Connection { return f.all }

// TestConnFor proves the outbound cascade: a session's own connection wins
// under every policy; with none, PolicyStrict stops, PolicyRootFallback
// suppresses branch sessions but falls back for roots, and PolicyFallback
// always falls back to the primary.
func TestConnFor(t *testing.T) {
	own := &fakeConn{name: "own"}
	primary := &fakeConn{name: "primary"}
	cm := &fakeConnMgr{
		bySession: map[string]platform.Connection{"a/c1": own},
		primary:   primary,
	}

	// Session's own connection wins regardless of policy.
	for _, p := range []Policy{PolicyStrict, PolicyFallback, PolicyRootFallback} {
		if c, o := ConnFor(cm, "a", "a/c1", p); c != own || o != DeliveredToSession {
			t.Errorf("policy %s with live session conn: got (%v, %s)", p, c, o)
		}
	}

	// No session connection: strict stops.
	if c, o := ConnFor(cm, "a", "a/c2", PolicyStrict); c != nil || o != DeliveryNone {
		t.Errorf("strict without conn: got (%v, %s), want (nil, none)", c, o)
	}

	// Root-fallback: branch sessions are suppressed, roots fall back.
	if c, o := ConnFor(cm, "a", "a/c2/b1700", PolicyRootFallback); c != nil || o != DeliverySuppressed {
		t.Errorf("root-fallback branch: got (%v, %s), want (nil, suppressed)", c, o)
	}
	if c, o := ConnFor(cm, "a", "a/c2", PolicyRootFallback); c != primary || o != DeliveredViaPrimary {
		t.Errorf("root-fallback root: got (%v, %s), want (primary, primary)", c, o)
	}

	// Plain fallback: even branches reach the primary.
	if c, o := ConnFor(cm, "a", "a/c2/b1700", PolicyFallback); c != primary || o != DeliveredViaPrimary {
		t.Errorf("fallback branch: got (%v, %s), want (primary, primary)", c, o)
	}

	// Nothing live anywhere.
	empty := &fakeConnMgr{bySession: map[string]platform.Connection{}}
	if c, o := ConnFor(empty, "a", "a/c2", PolicyFallback); c != nil || o != DeliveryNone {
		t.Errorf("no conns: got (%v, %s), want (nil, none)", c, o)
	}
	if c, o := ConnFor(nil, "a", "a/c2", PolicyFallback); c != nil || o != DeliveryNone {
		t.Errorf("nil manager: got (%v, %s), want (nil, none)", c, o)
	}
}

// TestBroadcast proves Broadcast returns the agent's full connection set and
// tolerates a nil manager.
func TestBroadcast(t *testing.T) {
	a, b := &fakeConn{name: "a"}, &fakeConn{name: "b"}
	cm := &fakeConnMgr{all: []platform.Connection{a, b}}
	conns := Broadcast(cm, "agent")
	if len(conns) != 2 || conns[0] != a || conns[1] != b {
		t.Errorf("Broadcast = %v, want [a b]", conns)
	}
	if got := Broadcast(nil, "agent"); got != nil {
		t.Errorf("Broadcast(nil) = %v, want nil", got)
	}
}
