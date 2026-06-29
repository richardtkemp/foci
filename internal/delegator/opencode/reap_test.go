package opencode

import "testing"

// TestCloseAllServers_DrainsPool proves the shutdown backstop empties the pool
// (ignoring refcounts) and returns the number closed, and is idempotent. Uses
// bare Servers (cmd == nil) whose Close is a safe no-op — the real subprocess
// SIGTERM/SIGKILL ladder is covered by Server.Close's own tests; this verifies
// the drain itself (#948). Not parallel: it touches the package-level pool.
func TestCloseAllServers_DrainsPool(t *testing.T) {
	// Start from a clean pool regardless of any prior test's leftovers.
	serverPoolMu.Lock()
	for id := range serverPool {
		delete(serverPool, id)
	}
	serverPool["a"] = &Server{}
	serverPool["b"] = &Server{}
	serverPool["c"] = &Server{}
	serverPoolMu.Unlock()

	if n := CloseAllServers(); n != 3 {
		t.Fatalf("CloseAllServers closed %d, want 3", n)
	}

	serverPoolMu.Lock()
	remaining := len(serverPool)
	serverPoolMu.Unlock()
	if remaining != 0 {
		t.Fatalf("pool not drained: %d server(s) remain", remaining)
	}

	// Idempotent: a second call on the empty pool closes nothing.
	if n := CloseAllServers(); n != 0 {
		t.Fatalf("second CloseAllServers closed %d, want 0", n)
	}
}
