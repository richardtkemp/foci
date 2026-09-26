package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/testtemp"
	"foci/internal/tools"
)

// NewBrowserTool creates the browser tool driving one fixed manager, whatever
// session calls it, for tests that exercise actions rather than sessions.
// Production registers NewSessionBrowserTool.
func NewBrowserTool(mgr *BrowserManager) *tools.Tool {
	return newBrowserTool(func(_ context.Context, p browserParams) (tools.ToolResult, error) {
		return dispatchBrowserAction(mgr, p)
	})
}

// poolLen reports how many sessions currently hold a manager.
func poolLen(p *SessionPool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func testPoolConfig() *config.ResolvedBrowser {
	return &config.ResolvedBrowser{
		Headless:      true,
		TimeoutSec:    10,
		DOMStableSec:  0.1,
		DOMStableDiff: 0.5,
	}
}

// TestSessionPoolManagerPerSession locks the #1600 ruling at the pool level:
// one session key always gets the same manager, and distinct keys (main chat,
// fork, branch, another chat) never share one.
func TestSessionPoolManagerPerSession(t *testing.T) {
	t.Parallel()
	pool := NewSessionPool(testPoolConfig(), 0o640)

	keys := []string{"clutch/c1", "clutch/c1/b2", "clutch/c1/f3", "clutch/c9"}
	seen := map[*BrowserManager]string{}
	for _, k := range keys {
		m := pool.acquire(k)
		pool.release(k)
		if other, dup := seen[m]; dup {
			t.Fatalf("sessions %q and %q share one browser manager", other, k)
		}
		seen[m] = k
		again := pool.acquire(k)
		pool.release(k)
		if again != m {
			t.Errorf("session %q got a different manager on its second call", k)
		}
	}
	if got := poolLen(pool); got != len(keys) {
		t.Errorf("pool holds %d managers, want %d", got, len(keys))
	}
	for _, e := range pool.entries {
		e.timer.Stop()
	}
}

// TestSessionPoolIdleReap verifies an idle session's manager is dropped after
// the TTL, and that a call still in flight keeps it alive.
func TestSessionPoolIdleReap(t *testing.T) {
	t.Parallel()
	pool := NewSessionPool(testPoolConfig(), 0o640)
	pool.idleTTL = 20 * time.Millisecond

	pool.acquire("busy") // never released: in flight throughout
	idle := pool.acquire("idle")
	pool.release("idle")

	deadline := time.Now().Add(2 * time.Second)
	for poolLen(pool) > 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if poolLen(pool) != 1 {
		t.Fatalf("pool holds %d managers after idle TTL, want 1 (the busy one)", poolLen(pool))
	}
	if _, ok := pool.entries["busy"]; !ok {
		t.Fatal("in-flight session was reaped")
	}
	// A later call from the reaped session starts over with a new manager.
	if pool.acquire("idle") == idle {
		t.Error("reaped session got its old manager back")
	}
}

// TestProfileLockExclusive verifies only one manager at a time may hold the
// persistent profile, and that dropping it frees it for another.
func TestProfileLockExclusive(t *testing.T) {
	t.Parallel()
	l := &profileLock{}
	a, b := &BrowserManager{}, &BrowserManager{}
	if err := l.take(a); err != nil {
		t.Fatalf("first take: %v", err)
	}
	if err := l.take(a); err != nil {
		t.Errorf("owner re-take: %v", err)
	}
	if err := l.take(b); err == nil {
		t.Fatal("second manager took a held profile")
	}
	l.drop(b) // a non-owner's drop must not release it
	if err := l.take(b); err == nil {
		t.Fatal("non-owner drop released the profile")
	}
	l.drop(a)
	if err := l.take(b); err != nil {
		t.Errorf("take after owner dropped: %v", err)
	}
}

// TestSessionBrowserToolIsolatesSessions drives the real tool with a real
// browser: two sessions navigate to different pages, and each one's snapshot
// must still show its own page.
func TestSessionBrowserToolIsolatesSessions(t *testing.T) {
	skipIfNoBrowser(t)

	srvA := testHTMLServer(t, `<html><head><title>Page Alpha</title></head><body><h1>Alpha</h1></body></html>`)
	srvB := testHTMLServer(t, `<html><head><title>Page Bravo</title></head><body><h1>Bravo</h1></body></html>`)

	pool := NewSessionPool(testPoolConfig(), 0o640)
	t.Cleanup(func() {
		for _, e := range pool.entries {
			_ = e.mgr.Stop()
		}
	})
	tool := NewSessionBrowserTool(pool)
	ctxA := tools.WithSessionKey(context.Background(), "test/c1")
	ctxB := tools.WithSessionKey(context.Background(), "test/c1/b2")

	if _, err := tool.Execute(ctxA, marshalParams(t, map[string]any{"action": "navigate", "url": srvA.URL})); err != nil {
		t.Fatalf("navigate A: %v", err)
	}
	if _, err := tool.Execute(ctxB, marshalParams(t, map[string]any{"action": "navigate", "url": srvB.URL})); err != nil {
		t.Fatalf("navigate B: %v", err)
	}

	snap := marshalParams(t, map[string]any{"action": "snapshot"})
	resA, err := tool.Execute(ctxA, snap)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	if !strings.Contains(resA.Text, "Page Alpha") || strings.Contains(resA.Text, "Page Bravo") {
		t.Errorf("session A's snapshot does not show its own page:\n%s", resA.Text)
	}
	resB, err := tool.Execute(ctxB, snap)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	if !strings.Contains(resB.Text, "Page Bravo") {
		t.Errorf("session B's snapshot does not show its own page:\n%s", resB.Text)
	}
}

// TestSessionPoolPersistentProfileExclusive verifies a second session cannot
// open the persistent profile while the first holds it, and gets told why.
func TestSessionPoolPersistentProfileExclusive(t *testing.T) {
	skipIfNoBrowser(t)

	base, err := os.MkdirTemp(testtemp.Dir(), "foci-pool-profile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testPoolConfig()
	cfg.UserDataDir = filepath.Join(base, "browser-profile")
	pool := NewSessionPool(cfg, 0o640)
	t.Cleanup(func() {
		for _, e := range pool.entries {
			_ = e.mgr.Stop()
		}
		removeAllRetry(base)
	})
	tool := NewSessionBrowserTool(pool)
	start := marshalParams(t, map[string]any{"action": "start", "incognito": false})

	res, err := tool.Execute(tools.WithSessionKey(context.Background(), "test/c1"), start)
	if err != nil || !strings.Contains(res.Text, "incognito: off") {
		t.Fatalf("first persistent start: %v %q", err, res.Text)
	}
	res, err = tool.Execute(tools.WithSessionKey(context.Background(), "test/c2"), start)
	if err != nil {
		t.Fatalf("second persistent start: %v", err)
	}
	if !strings.Contains(res.Text, "persistent browser profile is already open") {
		t.Errorf("second session's persistent start = %q, want the profile-in-use error", res.Text)
	}
}

// TestBrowserToolExecBridgeFunction verifies the tool is exec-exported and its
// generated foci_browser shell function passes the bridge's help/body parity
// check, which would otherwise fail every delegated session's bridge startup.
func TestBrowserToolExecBridgeFunction(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	reg.Register(NewSessionBrowserTool(NewSessionPool(testPoolConfig(), 0o640)))
	bridge, err := tools.NewExecBridge(reg, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()
	funcs, err := os.ReadFile(bridge.FuncsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(funcs), "foci_browser()") {
		t.Error("funcs file does not define foci_browser")
	}
}
