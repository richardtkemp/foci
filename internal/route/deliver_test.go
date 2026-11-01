package route

import (
	"testing"

	"foci/internal/platform"
)

// fakeConn is a minimal platform.Connection stand-in for routing tests; only
// identity matters, so the methods are inert. drops models the one
// broadcast-relevant capability: true is a connection that can only drop
// agent-wide sends (platform.AgentWideDropper — Slack's unbound conn).
type fakeConn struct {
	platform.Connection
	name  string
	drops bool
}

func (f *fakeConn) DropsAgentWideSends() bool { return f.drops }

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
// under every policy; with none, PolicyStrict stops and PolicyFallback falls
// back to the primary (for any session, branch or root).
func TestConnFor(t *testing.T) {
	own := &fakeConn{name: "own"}
	primary := &fakeConn{name: "primary"}
	cm := &fakeConnMgr{
		bySession: map[string]platform.Connection{"a/c1": own},
		primary:   primary,
	}

	// Session's own connection wins regardless of policy.
	for _, p := range []Policy{PolicyStrict, PolicyFallback} {
		if c, o := ConnFor(cm, "a", "a/c1", p); c != own || o != DeliveredToSession {
			t.Errorf("policy %s with live session conn: got (%v, %s)", p, c, o)
		}
	}

	// No session connection: strict stops.
	if c, o := ConnFor(cm, "a", "a/c2", PolicyStrict); c != nil || o != DeliveryNone {
		t.Errorf("strict without conn: got (%v, %s), want (nil, none)", c, o)
	}

	// Plain fallback reaches the primary for both roots and branches.
	if c, o := ConnFor(cm, "a", "a/c2", PolicyFallback); c != primary || o != DeliveredViaPrimary {
		t.Errorf("fallback root: got (%v, %s), want (primary, primary)", c, o)
	}
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
	a := &fakeConn{name: "a"}
	b := &fakeConn{name: "b"}
	cm := &fakeConnMgr{all: []platform.Connection{a, b}}
	conns := Broadcast(cm, "agent")
	if len(conns) != 2 || conns[0] != a || conns[1] != b {
		t.Errorf("Broadcast = %v, want [a b]", conns)
	}
	if got := Broadcast(nil, "agent"); got != nil {
		t.Errorf("Broadcast(nil) = %v, want nil", got)
	}
}

// TestBroadcast_SkipsAgentWideDroppers proves a connection that declares it
// drops agent-wide sends (Slack's unbound conn) is not a broadcast target:
// addressing it would be a certain drop, so its presence in AllForAgent must
// neither deliver to it nor log a delivery failure for it. Its position in
// the set must not matter.
func TestBroadcast_SkipsAgentWideDroppers(t *testing.T) {
	telegram := &fakeConn{name: "telegram"}
	slack := &fakeConn{name: "slack", drops: true}
	for _, tc := range []struct {
		name string
		all  []platform.Connection
		want []platform.Connection
	}{
		{"dropper last", []platform.Connection{telegram, slack}, []platform.Connection{telegram}},
		{"dropper first", []platform.Connection{slack, telegram}, []platform.Connection{telegram}},
		{"only dropper", []platform.Connection{slack}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conns := Broadcast(&fakeConnMgr{all: tc.all}, "agent")
			if len(conns) != len(tc.want) {
				t.Fatalf("Broadcast = %d conns, want %d", len(conns), len(tc.want))
			}
			for i, want := range tc.want {
				if conns[i] != want {
					t.Errorf("conn %d = %v, want %v", i, conns[i], want)
				}
			}
		})
	}
}
