package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// upgradableCodex lays out a Homebrew-cask-like install: link is the path foci
// is configured with, a symlink into one of two versioned directories, each
// holding a stub app-server. upgrade re-points it, as `brew upgrade` does.
type upgradableCodex struct {
	link             string
	v1, v2           string
	v1Capture, v2Cap string
}

func newUpgradableCodex(t *testing.T) *upgradableCodex {
	t.Helper()
	v1, c1 := stubAppServer(t)
	v2, c2 := stubAppServer(t)
	link := filepath.Join(t.TempDir(), "codex")
	if err := os.Symlink(v1, link); err != nil {
		t.Fatal(err)
	}
	return &upgradableCodex{link: link, v1: v1, v2: v2, v1Capture: c1, v2Cap: c2}
}

func (u *upgradableCodex) pointTo(t *testing.T, target string) {
	t.Helper()
	if err := os.Remove(u.link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, u.link); err != nil {
		t.Fatal(err)
	}
}

func (u *upgradableCodex) newBackend(t *testing.T) *Backend {
	t.Helper()
	be, err := newFromConfig(map[string]any{"binary": u.link})
	if err != nil {
		t.Fatal(err)
	}
	return be.(*Backend)
}

// startSession starts a session on b. When b launched the app-server, it also
// waits for that launch's background request to be answered (see
// awaitStartupRequests), so the app-server is idle when the test goes on.
func (u *upgradableCodex) startSession(t *testing.T, b *Backend, agent, key string) {
	t.Helper()
	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: agent, SessionKey: key, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("start %s: %v", key, err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if b.process() == b {
		u.awaitStartupRequests(t, b)
	}
}

// awaitStartupRequests waits until the owner b has had its launch's
// account/rateLimits/read answered (#3258). launchAppServer sends it from a
// goroutine (seedRateLimits), so Start can return with it still waiting for a
// reply, and a waiting request makes busyReason report the app-server busy:
// under load the tests' "idle" app-server was refused a recycle. The stub
// records each request as it reads it, and foci registers a request in
// pendingRPC before it writes it, so "the stub has read it and nothing is
// pending" means it was answered.
func (u *upgradableCodex) awaitStartupRequests(t *testing.T, b *Backend) {
	t.Helper()
	b.mu.Lock()
	launched := b.binStamp.real
	b.mu.Unlock()
	capture := u.v1Capture
	if v2, _ := filepath.EvalSymlinks(u.v2); launched == v2 {
		capture = u.v2Cap
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(capture)
		b.rpcMu.Lock()
		pending := len(b.pendingRPC)
		b.rpcMu.Unlock()
		if strings.Contains(string(data), `"account/rateLimits/read"`) && pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the app-server's startup requests were not answered within 10s (%d pending)", pending)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recycleNow(b *Backend) bool {
	unlock := acquireLocks.Lock(b.agentID)
	defer unlock()
	return b.recycleIfStaleLocked()
}

func poolOwner(agent string) *Backend {
	sharedPool.Lock()
	defer sharedPool.Unlock()
	return sharedPool.servers[agent]
}

func processExited(b *Backend) bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

func setTurnActive(b *Backend, v bool) {
	b.turnMu.Lock()
	b.turnActive = v
	b.turnMu.Unlock()
}

// The work Mac case (#3233): the binary foci launched is upgraded under a
// running app-server. Once idle, the app-server leaves the pool and stops,
// every session on it reports not running (so the manager respawns it), and
// the next Start launches the new binary.
func TestRecycleOnBinaryChangeWhenIdle(t *testing.T) {
	u := newUpgradableCodex(t)
	const agent = "recycle-idle-agent"
	b1, b2 := u.newBackend(t), u.newBackend(t)
	u.startSession(t, b1, agent, "session/a")
	u.startSession(t, b2, agent, "session/b")
	if b2.process() != b1 {
		t.Fatal("premise: the second session should share the first's app-server")
	}

	if recycleNow(b1) {
		t.Fatal("recycled with the binary unchanged")
	}
	u.pointTo(t, u.v2)
	if !recycleNow(b1) {
		t.Fatal("did not recycle an idle app-server whose binary changed")
	}
	if b1.IsRunning() || b2.IsRunning() {
		t.Errorf("IsRunning after recycle: owner %v, facade %v; want false for both", b1.IsRunning(), b2.IsRunning())
	}
	if poolOwner(agent) != nil {
		t.Error("the retired app-server is still the pool's owner")
	}
	if !processExited(b1) {
		t.Error("the retired app-server's process has not exited")
	}

	b3 := u.newBackend(t)
	u.startSession(t, b3, agent, "session/c")
	if b3.process() == b1 || !b3.IsRunning() {
		t.Fatal("the next Start did not launch a new app-server")
	}
	b3.mu.Lock()
	real := b3.binStamp.real
	b3.mu.Unlock()
	if want, _ := filepath.EvalSymlinks(u.v2); real != want {
		t.Errorf("new app-server launched from %q, want the upgraded %q", real, want)
	}
	if data, _ := os.ReadFile(u.v2Cap); len(data) == 0 {
		t.Error("the upgraded binary received no requests")
	}

	// The retired sessions close cleanly and leave the new owner alone.
	_ = b1.Close()
	_ = b2.Close()
	if poolOwner(agent) != b3 {
		t.Error("closing a retired session evicted the new app-server")
	}
}

// Dick's ruling: restart when no codex turn is running, never mid-turn.
func TestRecycleWaitsForRunningTurn(t *testing.T) {
	u := newUpgradableCodex(t)
	const agent = "recycle-busy-agent"
	b1, b2 := u.newBackend(t), u.newBackend(t)
	u.startSession(t, b1, agent, "session/a")
	u.startSession(t, b2, agent, "session/b")
	u.pointTo(t, u.v2)

	setTurnActive(b2, true)
	if recycleNow(b1) {
		t.Fatal("recycled while a facade's turn was running")
	}
	if !b1.IsRunning() || !b2.IsRunning() || poolOwner(agent) != b1 {
		t.Fatal("a refused recycle changed the app-server's state")
	}
	setTurnActive(b2, false)

	b1.compactMu.Lock()
	b1.compactDoneCh = make(chan error, 1)
	b1.compactMu.Unlock()
	if recycleNow(b1) {
		t.Fatal("recycled while a compaction was armed")
	}
	b1.compactMu.Lock()
	b1.compactDoneCh = nil
	b1.compactMu.Unlock()

	if !recycleNow(b1) {
		t.Fatal("did not recycle once the turn ended")
	}
}

// A Start is where a new thread is made, which an upgrade breaks first: it
// restarts an idle stale app-server rather than attaching to it.
func TestStartRecyclesStaleIdleOwner(t *testing.T) {
	u := newUpgradableCodex(t)
	const agent = "recycle-start-agent"
	b1 := u.newBackend(t)
	u.startSession(t, b1, agent, "session/a")
	u.pointTo(t, u.v2)

	b2 := u.newBackend(t)
	u.startSession(t, b2, agent, "session/b")
	if b2.process() == b1 {
		t.Fatal("Start attached to the app-server whose binary had changed")
	}
	if b1.IsRunning() {
		t.Error("the stale app-server still reports running")
	}
}

// With a turn running, Start attaches as before; the restart waits.
func TestStartAttachesToStaleBusyOwner(t *testing.T) {
	u := newUpgradableCodex(t)
	const agent = "recycle-start-busy-agent"
	b1 := u.newBackend(t)
	u.startSession(t, b1, agent, "session/a")
	u.pointTo(t, u.v2)
	setTurnActive(b1, true)

	b2 := u.newBackend(t)
	u.startSession(t, b2, agent, "session/b")
	if b2.process() != b1 || !b1.IsRunning() {
		t.Fatal("Start restarted an app-server with a turn running")
	}
	setTurnActive(b1, false)
}

// A turn that reaches a retired app-server is refused before anything is
// claimed or sent, with the error the agent retries on.
func TestBeginTurnOnRetiredAppServerRefuses(t *testing.T) {
	u := newUpgradableCodex(t)
	const agent = "recycle-begin-agent"
	b1, b2 := u.newBackend(t), u.newBackend(t)
	u.startSession(t, b1, agent, "session/a")
	u.startSession(t, b2, agent, "session/b")
	u.pointTo(t, u.v2)
	if !recycleNow(b1) {
		t.Fatal("premise: recycle")
	}
	err := b2.ImmediateInject(context.Background(), delegator.Inject{Source: delegator.SourceUser, Text: "hi", Turn: &delegator.TurnEvents{}})
	if !errors.Is(err, delegator.ErrBackendClosed) {
		t.Fatalf("ImmediateInject on a retired app-server: %v, want ErrBackendClosed", err)
	}
	if b2.IsTurnInFlight() {
		t.Error("the refused turn was claimed")
	}
}

// Mid-upgrade or uninstalled, there is nothing to relaunch: leave it running.
func TestRecycleSkipsWhenBinaryMissing(t *testing.T) {
	u := newUpgradableCodex(t)
	const agent = "recycle-missing-agent"
	b1 := u.newBackend(t)
	u.startSession(t, b1, agent, "session/a")
	u.pointTo(t, filepath.Join(t.TempDir(), "gone"))
	if recycleNow(b1) {
		t.Fatal("recycled with no binary to relaunch")
	}
	if !b1.IsRunning() {
		t.Error("app-server stopped")
	}
}

// The periodic check retires an idle stale app-server with no Start needed.
func TestWatchBinaryRecyclesIdleOwner(t *testing.T) {
	saved := binaryCheckEvery
	binaryCheckEvery = 20 * time.Millisecond
	t.Cleanup(func() { binaryCheckEvery = saved })
	u := newUpgradableCodex(t)
	const agent = "recycle-watch-agent"
	b1 := u.newBackend(t)
	u.startSession(t, b1, agent, "session/a")
	time.Sleep(100 * time.Millisecond)
	if !b1.IsRunning() {
		t.Fatal("the watcher stopped an app-server whose binary had not changed")
	}
	u.pointTo(t, u.v2)
	deadline := time.Now().Add(5 * time.Second)
	for b1.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b1.IsRunning() {
		t.Fatal("the watcher did not recycle the app-server after the binary changed")
	}
}

func TestBinaryStampChangedTo(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	write := func(content string) {
		tmp := bin + ".new"
		if err := os.WriteFile(tmp, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, bin); err != nil {
			t.Fatal(err)
		}
	}
	write("#!/bin/sh\necho v1\n")
	s1, err := stampBinary(bin)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := stampBinary(bin)
	if why, changed := s1.changedTo(same); changed {
		t.Errorf("unchanged binary reported changed: %s", why)
	}
	write("#!/bin/sh\necho v2 longer\n") // replaced in place: same path, new file
	s2, _ := stampBinary(bin)
	if _, changed := s1.changedTo(s2); !changed {
		t.Error("a binary replaced at the same path was not detected")
	}
	if _, changed := (binaryStamp{}).changedTo(s2); changed {
		t.Error("a missing launch stamp reported a change")
	}
}
