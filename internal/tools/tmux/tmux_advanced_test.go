package tmux

import (
	"context"
	"encoding/json"
	"foci/internal/tools"
	"strings"
	"sync"
	"testing"
	"time"
)

// limiterClock is a fake clock for the send limiter's time seam
// (tmuxInstance.now / tmuxInstance.sleep, see paceSend). Virtual time only
// moves when the code under test sleeps or the test advances it — never when
// the host is slow — so the limiter's gap arithmetic is deterministic on a
// loaded host (#2222).
type limiterClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

// Now is the tmuxInstance.now seam.
func (c *limiterClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Sleep is the tmuxInstance.sleep seam: it records the requested wait and
// advances virtual time by it, so the admission stamp taken after the sleep
// sees elapsed time exactly as time.Now would after time.Sleep in production.
// The wait itself costs no real time — the test measures the decision, not
// the nap.
func (c *limiterClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
}

// Advance moves virtual time forward without recording a sleep.
func (c *limiterClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Sleeps returns every wait the limiter requested, in order.
func (c *limiterClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

func TestTmuxSendRateLimit(t *testing.T) {
	// Verifies the send limiter itself (paceSend): a first send to a session
	// is admitted without delay, sends to different sessions do not pace each
	// other, and consecutive sends to the same session are held to exactly
	// sendMinGap — measured as the limiter sees time (its admission stamps on
	// the injected clock), never as the wall-clock duration of a whole send
	// call, which also covers the tmux subprocesses and host slowness the
	// limiter does not control (#2222).
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	clk := &limiterClock{}
	inst := &tmuxInstance{
		watched:    make(map[string]*watchedSession),
		owned:      make(map[string]string),
		lastSend:   make(map[string]time.Time),
		lastAccess: make(map[string]time.Time),
		socketPath: sock,
		now:        clk.Now,
		sleep:      clk.Sleep,
	}

	nameA, nameB := "foci-test-ratelimit-a", "foci-test-ratelimit-b"
	for _, name := range []string{nameA, nameB} {
		if _, err := runTmuxWithSocket(context.Background(), sock, "new-session", "-d", "-s", name, "cat"); err != nil {
			t.Fatalf("create session %s: %v", name, err)
		}
		inst.owned[name] = "" // register ownership, as start() would
	}

	send := func(name string) {
		t.Helper()
		if _, err := inst.send(context.Background(), name, "x", false); err != nil {
			t.Fatalf("send to %s: %v", name, err)
		}
	}

	// A first send has no previous admission to wait out.
	send(nameA)
	if sleeps := clk.Sleeps(); len(sleeps) != 0 {
		t.Errorf("first send waited %v, want no wait", sleeps)
	}

	// A send to a DIFFERENT session at the same instant is not paced: the
	// limiter keys per session, so a global limiter fails here.
	send(nameB)
	if sleeps := clk.Sleeps(); len(sleeps) != 0 {
		t.Errorf("send to another session waited %v, want no wait (limiter must be per-session)", sleeps)
	}

	// 250ms after A's admission, the next send to A waits out exactly the
	// remainder of the gap. No wait means the limiter is gone; the full gap
	// means the wait ignores time elapsed since the previous admission.
	clk.Advance(250 * time.Millisecond)
	send(nameA)
	if sleeps := clk.Sleeps(); len(sleeps) != 1 || sleeps[0] != sendMinGap-250*time.Millisecond {
		t.Errorf("send 250ms after admission waited %v, want [%v]", sleeps, sendMinGap-250*time.Millisecond)
	}

	// Exactly sendMinGap after the previous admission there is nothing left
	// to wait — the boundary case the old wall-clock test misread as the
	// limiter failing under load: a send whose predecessor already outlasted
	// the gap is admitted at once (gap runs start-to-start, see paceSend).
	clk.Advance(sendMinGap)
	send(nameA)
	if sleeps := clk.Sleeps(); len(sleeps) != 1 {
		t.Errorf("send exactly one gap after admission waited %v, want no wait", sleeps)
	}

	// An immediate follow-up send waits out the full gap again: the rule
	// holds for every consecutive pair, not just the first.
	send(nameA)
	if sleeps := clk.Sleeps(); len(sleeps) != 2 || sleeps[1] != sendMinGap {
		t.Errorf("immediate consecutive send waited %v, want waits [%v, %v]", sleeps, sendMinGap-250*time.Millisecond, sendMinGap)
	}
}

func TestTmuxSessionKeyIsolation(t *testing.T) {
	// Verifies that session key isolation is enforced: one session context cannot read, send to, or kill sessions owned by a different context.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	// Single tool instance, two different session keys
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

	nameA := "foci-test-skiso-a"
	nameB := "foci-test-skiso-b"

	ctxA := tools.WithSessionKey(context.Background(), "test/c111")
	ctxB := tools.WithSessionKey(context.Background(), "test/c222")

	// Session A starts
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameA,
		"command":   testSessionCmd,
	})
	if _, err := tool.Execute(ctxA, params); err != nil {
		t.Fatalf("session A start: %v", err)
	}

	// Session B starts
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameB,
		"command":   testSessionCmd,
	})
	if _, err := tool.Execute(ctxB, params); err != nil {
		t.Fatalf("session B start: %v", err)
	}

	// Session B cannot read session A's tmux session
	readParams, _ := json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      nameA,
	})
	_, err := tool.Execute(ctxB, readParams)
	if err == nil {
		t.Fatal("session B should not be able to read session A's tmux session")
	}
	if !strings.Contains(err.Error(), "not owned") {
		t.Errorf("error = %q, want 'not owned'", err.Error())
	}

	// Session A can read its own tmux session
	_, err = tool.Execute(ctxA, readParams)
	if err != nil {
		t.Fatalf("session A should be able to read its own session: %v", err)
	}

	// Session B cannot send to session A's tmux session
	sendParams, _ := json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      nameA,
		"keys":      "hello",
	})
	_, err = tool.Execute(ctxB, sendParams)
	if err == nil {
		t.Fatal("session B should not be able to send to session A's session")
	}

	// Session B cannot kill session A's tmux session
	killParams, _ := json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      nameA,
	})
	_, err = tool.Execute(ctxB, killParams)
	if err == nil {
		t.Fatal("session B should not be able to kill session A's session")
	}

	// List from session A: should show owner "test" for its own, "-" for B's
	listParams, _ := json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err := tool.Execute(ctxA, listParams)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, line := range strings.Split(result.Text, "\n") {
		if strings.Contains(line, nameA) && !strings.Contains(line, "test") {
			t.Errorf("session A should show %q with owner 'test': %q", nameA, line)
		}
	}
}

func TestTmuxReapExpiredSessions(t *testing.T) {
	// Verifies that the reaper removes sessions whose lastAccess time is past the TTL, killing both the internal tracking state and the actual tmux session.
	t.Parallel()
	// Isolated tmux server so the reaper's maybeKillTmuxServer
	// can't race with other parallel tests on the shared server.
	sock := tmuxIsolatedSocket(t)

	name := "foci-test-reap"

	inst := &tmuxInstance{
		watched:    make(map[string]*watchedSession),
		owned:      make(map[string]string),
		lastSend:   make(map[string]time.Time),
		lastAccess: make(map[string]time.Time),
		sessionTTL: 100 * time.Millisecond,
		socketPath: sock,
	}

	// Create a real tmux session on the isolated server
	_, err := runTmuxWithSocket(context.Background(), sock, "new-session", "-d", "-s", name, testSessionCmd)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Register it as owned with an old lastAccess time
	inst.owned[name] = ""
	inst.lastAccess[name] = time.Now().Add(-1 * time.Second) // well past TTL

	// Reap
	inst.reapExpiredSessions()

	// Verify session was removed from owned
	if _, ok := inst.owned[name]; ok {
		t.Error("session should have been removed from owned map")
	}
	if _, ok := inst.lastAccess[name]; ok {
		t.Error("session should have been removed from lastAccess map")
	}

	// Verify tmux session was killed
	_, err = runTmuxWithSocket(context.Background(), sock, "has-session", "-t", name)
	if err == nil {
		t.Error("tmux session should have been killed by reaper")
	}
}

func TestTmuxReapPreservesActiveSession(t *testing.T) {
	// Verifies that recently-accessed sessions are not reaped even when the reaper runs, distinguishing active from expired by TTL comparison.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	name := "foci-test-reap-active"

	inst := &tmuxInstance{
		watched:    make(map[string]*watchedSession),
		owned:      make(map[string]string),
		lastSend:   make(map[string]time.Time),
		lastAccess: make(map[string]time.Time),
		sessionTTL: 1 * time.Hour,
		socketPath: sock,
	}

	// Create a real tmux session
	_, err := runTmuxWithSocket(context.Background(), sock, "new-session", "-d", "-s", name, testSessionCmd)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Register with recent access
	inst.owned[name] = ""
	inst.lastAccess[name] = time.Now()

	// Reap — should not kill
	inst.reapExpiredSessions()

	// Verify session is still owned
	if _, ok := inst.owned[name]; !ok {
		t.Error("active session should not have been reaped")
	}

	// Verify tmux session still exists
	_, err = runTmuxWithSocket(context.Background(), sock, "has-session", "-t", name)
	if err != nil {
		t.Error("active tmux session should still exist after reap")
	}
}
