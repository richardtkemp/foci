package platform

// #2043: BotManager.ConnectBeforeRun — the one background-connect mechanism
// Telegram and Discord share. The platform packages test it end to end with
// their real connects; these tests pin the manager's own contract.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// connectFakeBot is a PooledBot + Connection whose Run records that it ran.
type connectFakeBot struct {
	Connection // nil: only the methods below are exercised
	name       string
	mu         sync.Mutex
	sessionKey string
	ran        atomic.Bool
}

func (b *connectFakeBot) SessionKey() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionKey
}

func (b *connectFakeBot) SetSessionKey(k string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessionKey = k
}

func (b *connectFakeBot) SetSecondary(*Pool[*connectFakeBot]) {}
func (b *connectFakeBot) PlatformName() string                { return "fake" }
func (b *connectFakeBot) Run(ctx context.Context) {
	b.ran.Store(true)
	<-ctx.Done()
}

// gate is a connect func that blocks until released (nil = success) or ctx ends.
type gate struct {
	release chan error
	calls   atomic.Int32
}

func newGate() *gate { return &gate{release: make(chan error, 1)} }

func (g *gate) connect(ctx context.Context) error {
	g.calls.Add(1)
	select {
	case err := <-g.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second) // hang guard only
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestConnectBeforeRun_PendingBotIsHiddenUntilConnected(t *testing.T) {
	m := NewBotManager[*connectFakeBot]("fake")
	primary := &connectFakeBot{name: "primary"}
	facet := &connectFakeBot{name: "facet"}
	m.AddPrimary("a", primary)
	m.AddFacet("a", facet)
	pg, fg := newGate(), newGate()
	m.ConnectBeforeRun(primary, "primary", pg.connect)
	m.ConnectBeforeRun(facet, "facet", fg.connect)
	facet.SetSessionKey("a/facet-session")

	cm := NewConnectionManagerAdapter[*connectFakeBot](m)
	if cm.Primary("a") != nil || len(cm.AllForAgent("a")) != 0 {
		t.Fatal("pending primary handed out as a connection")
	}
	if m.RegisteredPrimary("a") != primary {
		t.Fatal("RegisteredPrimary must reach the pending bot for setup-time wiring")
	}
	if cm.ForSession("a/facet-session") != nil || cm.HasFacet("a") {
		t.Fatal("pending facet visible to session lookup or HasFacet")
	}
	facet.SetSessionKey("")
	if _, ok := cm.AcquireFacet("a"); ok {
		t.Fatal("pending facet acquired")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); m.Wait() }()
	m.StartAll(ctx) // must not wait on the gated connects
	waitUntil(t, "both connects to start", func() bool { return pg.calls.Load() == 1 && fg.calls.Load() == 1 })
	if primary.ran.Load() {
		t.Fatal("Run started before connect succeeded")
	}

	pg.release <- nil
	waitUntil(t, "primary to go live and run", func() bool { return cm.Primary("a") != nil && primary.ran.Load() })
	if cm.HasFacet("a") {
		t.Fatal("facet went live with the primary; it has its own connect")
	}
	fg.release <- nil
	waitUntil(t, "facet to go live", func() bool { return cm.HasFacet("a") })
	if f, ok := cm.AcquireFacet("a"); !ok || f != Connection(facet) {
		t.Fatalf("AcquireFacet = %v, %v; want the now-live facet", f, ok)
	}
}

func TestConnectBeforeRun_FailedConnectNeverRuns(t *testing.T) {
	m := NewBotManager[*connectFakeBot]("fake")
	b := &connectFakeBot{}
	m.AddPrimary("a", b)
	g := newGate()
	m.ConnectBeforeRun(b, "primary", g.connect)
	g.release <- errors.New("Unauthorized")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartAll(ctx)
	m.Wait() // the failed connect's goroutine ends on its own
	if b.ran.Load() || m.PrimaryBot("a") != nil {
		t.Fatal("a bot whose connect failed was run or made live")
	}
}

func TestConnectBeforeRun_ShutdownEndsPendingConnect(t *testing.T) {
	m := NewBotManager[*connectFakeBot]("fake")
	b := &connectFakeBot{}
	m.AddPrimary("a", b)
	g := newGate()
	m.ConnectBeforeRun(b, "primary", g.connect)

	ctx, cancel := context.WithCancel(context.Background())
	m.StartAll(ctx)
	waitUntil(t, "connect to start", func() bool { return g.calls.Load() == 1 })
	cancel()
	done := make(chan struct{})
	go func() { m.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return: shutdown did not end the pending connect")
	}
	if b.ran.Load() {
		t.Fatal("bot ran after its connect was cancelled")
	}
}

// WhenPrimaryConnected fires exactly once per primary: at once for a live
// one, on attach for a pending one — including a callback registered while
// the connect is in flight.
func TestWhenPrimaryConnected_FiresOncePerPrimary(t *testing.T) {
	m := NewBotManager[*connectFakeBot]("fake")
	live := &connectFakeBot{name: "live"}
	pending := &connectFakeBot{name: "pending"}
	m.AddPrimary("live", live)
	m.AddPrimary("pending", pending)
	g := newGate()
	m.ConnectBeforeRun(pending, "pending", g.connect)
	cm := NewConnectionManagerAdapter[*connectFakeBot](m)

	var mu sync.Mutex
	got := map[string]int{}
	record := func(c Connection) {
		mu.Lock()
		defer mu.Unlock()
		got[c.(*connectFakeBot).name]++
	}
	count := func(name string) int {
		mu.Lock()
		defer mu.Unlock()
		return got[name]
	}

	cm.WhenPrimaryConnected("live", record)
	cm.WhenPrimaryConnected("pending", record)
	cm.WhenPrimaryConnected("nobody", record)
	if count("live") != 1 || count("pending") != 0 {
		t.Fatalf("before attach: calls = %v, want live=1 pending=0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); m.Wait() }()
	m.StartAll(ctx)
	waitUntil(t, "connect in flight", func() bool { return g.calls.Load() == 1 })
	cm.WhenPrimaryConnected("pending", record) // registered mid-connect
	g.release <- nil
	waitUntil(t, "pending primary to attach", func() bool { return count("pending") == 2 })
	cm.WhenPrimaryConnected("pending", record) // after attach: immediate
	if count("pending") != 3 || count("live") != 1 {
		t.Fatalf("calls = %v, want live=1 pending=3 (each callback exactly once)", got)
	}
}

// A connection manager with nothing to wait for (the app hub, the noop
// manager) gets the facade's fallback: its current primaries, at once.
func TestMessagingWhenPrimaryConnected_FallsBackToCurrentConnections(t *testing.T) {
	c := &namedConn{name: "app-conn"}
	m := &Messaging{providers: []MessagingProvider{&connMgrOnlyProvider{cm: &allForAgentConnMgr{conn: c}}}}
	var got []Connection
	m.WhenPrimaryConnected("a", func(conn Connection) { got = append(got, conn) })
	if len(got) != 1 || got[0] != Connection(c) {
		t.Fatalf("got %v, want the provider's current connection", got)
	}
}

// allForAgentConnMgr reports one connection for every agent and does not
// implement PrimaryConnectNotifier.
type allForAgentConnMgr struct {
	testConnMgr
	conn Connection
}

func (m *allForAgentConnMgr) AllForAgent(string) []Connection { return []Connection{m.conn} }

// connMgrOnlyProvider is a MessagingProvider whose only live method is
// ConnectionManager.
type connMgrOnlyProvider struct {
	MessagingProvider
	cm ConnectionManager
}

func (p *connMgrOnlyProvider) ConnectionManager() ConnectionManager { return p.cm }

// A connect attempt already in flight at shutdown (a getMe or gateway dial
// that only ends on its own network timeout) must not hold up Wait.
func TestConnectBeforeRun_ShutdownDoesNotWaitForInFlightAttempt(t *testing.T) {
	m := NewBotManager[*connectFakeBot]("fake")
	b := &connectFakeBot{}
	m.AddPrimary("a", b)
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	m.ConnectBeforeRun(b, "primary", func(context.Context) error {
		calls.Add(1)
		<-release // deaf to ctx, like a network call mid-flight
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	m.StartAll(ctx)
	waitUntil(t, "connect to start", func() bool { return calls.Load() == 1 })
	cancel()
	done := make(chan struct{})
	go func() { m.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait blocked on a connect attempt still in flight at shutdown")
	}
	if b.ran.Load() || m.PrimaryBot("a") != nil {
		t.Fatal("bot ran or went live after its connect was abandoned")
	}
}
