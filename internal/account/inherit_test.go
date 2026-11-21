package account

import (
	"testing"

	"foci/internal/session"
)

// The #3246 rule (Dick 2026-11-21 11:47): "if we want to re-use an existing
// warm cache (i.e. if we fork/copy transcript) then we must use the same
// account." A child that copies a transcript is bound to its source's
// account (Inherit); every other child takes the pick, as a new top-level
// session does.

// TestPool_FreshChildTakesThePick: a child no one bound with Inherit (a
// character spawn, a raw spawn, a batch: none copies a transcript) takes the
// pick, not its root's account, and stores it on itself.
func TestPool_FreshChildTakesThePick(t *testing.T) {
	a, b := testAccount("a"), testAccount("b")
	const root = "agent/c1"
	st := memStore{root + "\x00" + session.MetaKeyAccount: "b"}
	p := NewPool(st, a, b)
	if got := p.For(root); got != b {
		t.Fatalf("setup: root on %v, want b", got.Key())
	}
	for _, child := range []string{root + "/b1709596800", root + "/i1709596801"} {
		if got := p.For(child); got != a {
			t.Errorf("fresh child %s on %v, want the pick (a), not its root's b", child, got.Key())
		}
		if v := st[child+"\x00"+session.MetaKeyAccount]; v != "a" {
			t.Errorf("fresh child %s stored on %q, want a", child, v)
		}
	}
}

// TestPool_InheritBindsTheSourcesAccount: a fork (a child whose transcript is
// a copy of its source's) is on its source's account, stored on itself, so
// a restart finds it there too. The source may itself be a child on an
// account other than its root's (a clone spawn of a character spawn): the
// fork follows the source, not the root.
func TestPool_InheritBindsTheSourcesAccount(t *testing.T) {
	a, b := testAccount("a"), testAccount("b")
	const root = "agent/c1"
	st := memStore{root + "\x00" + session.MetaKeyAccount: "b"}
	p := NewPool(st, a, b)

	fork := root + "/b1709596800"
	if got, err := p.Inherit(fork, root); err != nil || got != b {
		t.Fatalf("Inherit(fork, root) = %v, %v; want b", got.Key(), err)
	}
	if got := p.For(fork); got != b {
		t.Errorf("fork on %v, want its source's b", got.Key())
	}
	if v := st[fork+"\x00"+session.MetaKeyAccount]; v != "b" {
		t.Errorf("fork stored on %q, want b", v)
	}
	if got := NewPool(st, a, b).For(fork); got != b {
		t.Errorf("after a restart the fork is on %v, want b", got.Key())
	}

	// A fresh child (on the pick, a) forked again: the fork is on a.
	fresh := root + "/b1709596801"
	if got := p.For(fresh); got != a {
		t.Fatalf("setup: fresh child on %v, want a", got.Key())
	}
	forkOfFresh := root + "/b1709596802"
	if got, err := p.Inherit(forkOfFresh, fresh); err != nil || got != a {
		t.Errorf("Inherit(forkOfFresh, fresh) = %v, %v; want the source's a, not the root's b", got.Key(), err)
	}
}

// TestPool_InheritOverridesAnEarlierBinding: the fork's transcript is in its
// source's folder whatever the key was bound to before, so Inherit binds and
// stores the source's account over it.
func TestPool_InheritOverridesAnEarlierBinding(t *testing.T) {
	a, b := testAccount("a"), testAccount("b")
	const root = "agent/c1"
	st := memStore{root + "\x00" + session.MetaKeyAccount: "b"}
	p := NewPool(st, a, b)
	fork := root + "/b1709596800"
	p.For(fork) // bound to the pick, a
	if got, err := p.Inherit(fork, root); err != nil || got != b {
		t.Fatalf("Inherit = %v, %v; want b", got.Key(), err)
	}
	if got := p.For(fork); got != b {
		t.Errorf("fork on %v, want b", got.Key())
	}
	if v := st[fork+"\x00"+session.MetaKeyAccount]; v != "b" {
		t.Errorf("fork stored on %q, want b", v)
	}
}

// TestPool_InheritErrors: a source whose account cannot be read (another
// agent's session) binds nothing; a nil pool has no account.
func TestPool_InheritErrors(t *testing.T) {
	a := testAccount("a")
	st := memStore{}
	p := NewPool(st, a).ForAgent("agent")
	if got, err := p.Inherit("agent/c1/b5", "other/c1"); err == nil || got != nil {
		t.Errorf("Inherit from another agent's session = %v, %v; want an error", got.Key(), err)
	}
	if v, ok := st["agent/c1/b5\x00"+session.MetaKeyAccount]; ok {
		t.Errorf("stored %q after a failed Inherit", v)
	}
	var nilPool *Pool
	if got, err := nilPool.Inherit("agent/c1/b5", "agent/c1"); got != nil || err != nil {
		t.Errorf("nil pool Inherit = %v, %v; want nil, nil", got.Key(), err)
	}
}
