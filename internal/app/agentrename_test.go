package app

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/session"
)

// TestStopAndRenameAgent pins the hub's half of the live rename (#2787):
// StopAgent forgets the agent's connection and order slot but keeps its
// conversation bindings; RenameAgent re-points those bindings — live sockets
// attached, sequence kept — to the new id under rewritten session keys, puts
// the new id in the old one's slot, and leaves every other agent's bindings
// alone.
func TestStopAndRenameAgent(t *testing.T) {
	h, _ := sweepHub(t, nil)
	now := time.Now()
	for _, id := range []string{"first", "clutch", "cam"} {
		h.agents[id] = &appConn{hub: h, agentID: id}
		h.agentOrder = append(h.agentOrder, id)
	}
	a := sweepBinding(h, "a", time.Minute, now)
	b := sweepBinding(h, "b", time.Minute, now)
	other := sweepBinding(h, "other", time.Minute, now)
	other.setIdent("cam", other.key())
	other.setIdent(other.agent(), session.NewChatSessionKey("cam", other.chatID))
	for _, x := range []*convBinding{a, b, other} {
		h.bySession[x.key()] = x
	}
	a.seq = 41
	c := fakeClient()
	a.attach(c)
	oldKeyA := a.key()

	h.StopAgent("clutch")
	if _, ok := h.agents["clutch"]; ok {
		t.Error("clutch's connection survived StopAgent")
	}
	if !slices.Equal(h.agentOrder, []string{"first", "cam"}) {
		t.Errorf("order after stop = %v", h.agentOrder)
	}
	if h.convs["a"] != a || h.convs["b"] != b {
		t.Fatal("StopAgent dropped clutch's conversation bindings")
	}

	rewrite := func(k string) string {
		if strings.HasPrefix(k, "clutch/") {
			return "rob/" + strings.TrimPrefix(k, "clutch/")
		}
		return k
	}
	h.RenameAgent("clutch", "rob", rewrite)

	for _, x := range []*convBinding{a, b} {
		if x.agent() != "rob" || !strings.HasPrefix(x.key(), "rob/") {
			t.Errorf("binding %s: agent %q key %q, want rob's", x.convID, x.agent(), x.key())
		}
		if h.bySession[x.key()] != x {
			t.Errorf("binding %s not found under its new key", x.convID)
		}
	}
	if _, ok := h.bySession[oldKeyA]; ok {
		t.Errorf("old key %s still maps to a binding", oldKeyA)
	}
	if _, attached := a.clients[c]; !attached || a.seq != 41 {
		t.Errorf("binding a lost its socket (%v) or sequence (%d)", attached, a.seq)
	}
	if other.agent() != "cam" || !strings.HasPrefix(other.key(), "cam/") {
		t.Errorf("cam's binding changed: %q %q", other.agent(), other.key())
	}
	if !slices.Equal(h.agentOrder, []string{"first", "rob", "cam"}) {
		t.Errorf("order after rename = %v, want rob in clutch's slot", h.agentOrder)
	}
}

// TestRenameAgent_ConcurrentReaders: the live rename re-points bindings while
// roster pushes and dispatch read them from other goroutines. Run under the
// race detector, a field written in place under b.mu while a reader reads it
// unlocked is reported; the ident pair swapped whole is not. Each reader also
// checks the pair is never mixed (an agent with the other agent's key).
func TestRenameAgent_ConcurrentReaders(t *testing.T) {
	h, _ := sweepHub(t, nil)
	now := time.Now()
	h.agents["clutch"] = &appConn{hub: h, agentID: "clutch"}
	h.agentOrder = []string{"clutch"}
	var bs []*convBinding
	for _, id := range []string{"a", "b", "c"} {
		b := sweepBinding(h, id, time.Minute, now)
		h.bySession[b.key()] = b
		bs = append(bs, b)
	}
	rewrite := func(from, to string) func(string) string {
		return func(k string) string { return to + strings.TrimPrefix(k, from) }
	}
	stop := make(chan struct{})
	var mixed atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = h.agentRoster()
				for _, b := range bs {
					ag, k := b.identity()
					if !strings.HasPrefix(k, ag+"/") {
						mixed.Store(true)
					}
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		h.RenameAgent("clutch", "rob", rewrite("clutch", "rob"))
		h.RenameAgent("rob", "clutch", rewrite("rob", "clutch"))
	}
	close(stop)
	wg.Wait()
	if mixed.Load() {
		t.Error("a reader saw an agent id paired with another agent's session key")
	}
}

// withIdent sets a test binding's owning agent and session key (the pair is
// one atomic value, convBinding.ident) and returns it.
func withIdent(b *convBinding, agentID, sessionKey string) *convBinding {
	b.setIdent(agentID, sessionKey)
	return b
}
