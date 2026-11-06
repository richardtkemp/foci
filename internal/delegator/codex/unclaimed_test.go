package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// stopUnclaimedTimer stops owner's deferred lost-line timer at test end, so a
// timer armed by one test cannot fire a WARN into a later test's hook.
func stopUnclaimedTimer(t *testing.T, owner *Backend) {
	t.Helper()
	t.Cleanup(func() {
		owner.unclaimed.mu.Lock()
		if owner.unclaimed.timer != nil {
			owner.unclaimed.timer.Stop()
		}
		owner.unclaimed.stopped = true
		owner.unclaimed.mu.Unlock()
	})
}

func lostWarns(warns []string) []string {
	var out []string
	for _, w := range warns {
		if strings.Contains(w, "lost message") {
			out = append(out, w)
		}
	}
	return out
}

// earlyForkLines are the notifications codex sent for a fresh fork (and then
// its resume) before foci registered the thread, as seen in the work Mac log
// on 2026-10-31 (#2700): two mcpServer/startupStatus/updated and a
// thread/tokenUsage/updated during thread/fork, then a tokenUsage,
// thread/goal/cleared and thread/status/changed during thread/resume.
func earlyForkLines(thread string) [][]byte {
	return [][]byte{
		[]byte(`{"method":"mcpServer/startupStatus/updated","params":{"threadId":"` + thread + `","name":"foci","status":"starting"}}`),
		[]byte(`{"method":"mcpServer/startupStatus/updated","params":{"threadId":"` + thread + `","name":"foci","status":"ready"}}`),
		[]byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"` + thread + `","turnId":"t-old","tokenUsage":{"total":{"inputTokens":9000,"cachedInputTokens":8000,"outputTokens":300,"totalTokens":9300},"last":{"inputTokens":3000,"cachedInputTokens":2500,"outputTokens":100,"totalTokens":3100},"modelContextWindow":272000}}}`),
		[]byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"` + thread + `","turnId":"t-old","tokenUsage":{"total":{"inputTokens":9000,"cachedInputTokens":8000,"outputTokens":300,"totalTokens":9300},"last":{"inputTokens":3000,"cachedInputTokens":2500,"outputTokens":100,"totalTokens":3100},"modelContextWindow":272000}}}`),
		[]byte(`{"method":"thread/goal/cleared","params":{"threadId":"` + thread + `"}}`),
		[]byte(`{"method":"thread/status/changed","params":{"threadId":"` + thread + `","status":{"type":"idle"}}}`),
	}
}

// TestForkEarlyLinesAreNotReportedLost is #2700's regression: a fork's and a
// resumed thread's first notifications arrive before the thread is registered.
// Once its branch facade registers the thread, none of them may be reported
// as lost, even after the grace has long passed. They still go to the owner,
// which consumes them without touching its own turn or accounting.
func TestForkEarlyLinesAreNotReportedLost(t *testing.T) {
	owner, _, oc, _, _ := newSharedPair(t)
	stopUnclaimedTimer(t, owner)
	warns := captureWarns(t)

	for _, line := range earlyForkLines("fork-thread") {
		owner.dispatch(line)
	}
	branch := &Backend{
		cfg:       map[string]any{},
		lg:        log.NewComponentLogger("codex"),
		startOpts: delegator.StartOptions{SessionKey: "codex/owner/b1"},
		shared:    owner,
	}
	setTestThread(branch, "fork-thread") // what resumeThread does once the response arrives

	owner.flushUnclaimed(time.Now().Add(10*unclaimedGrace), false)
	if got := lostWarns(warns()); len(got) != 0 {
		t.Errorf("a fork registered after its early lines was reported lost: %q", got)
	}
	if len(oc.chatText)+len(oc.deltas)+len(oc.prompts) != 0 {
		t.Errorf("owner chat showed the fork's early lines: chat=%q deltas=%q prompts=%v", oc.chatText, oc.deltas, oc.prompts)
	}
	owner.turnMu.Lock()
	stashed := owner.stashedUsage
	owner.turnMu.Unlock()
	if stashed != nil {
		t.Errorf("the fork's replayed token usage was stashed as the owner's: %+v", stashed)
	}
}

// TestUnclaimedThreadIsReportedAfterGrace: a thread that never registers is
// still reported, once it has been unclaimed for unclaimedGrace — one WARN
// for the thread naming every method and its count, and only once.
func TestUnclaimedThreadIsReportedAfterGrace(t *testing.T) {
	owner, _, _, _, _ := newSharedPair(t)
	stopUnclaimedTimer(t, owner)
	warns := captureWarns(t)

	start := time.Now()
	for _, line := range earlyForkLines("orphan-thread") {
		owner.dispatch(line)
	}

	owner.flushUnclaimed(start, false)
	if got := lostWarns(warns()); len(got) != 0 {
		t.Fatalf("an orphan was reported before the grace ran out: %q", got)
	}

	owner.flushUnclaimed(start.Add(unclaimedGrace+time.Second), false)
	got := lostWarns(warns())
	if len(got) != 1 {
		t.Fatalf("lost WARNs after the grace = %q, want exactly one for the orphan", got)
	}
	for _, want := range []string{
		"orphan-thread",
		"mcpServer/startupStatus/updated (x2)",
		"thread/tokenUsage/updated (x2)",
		"thread/goal/cleared",
		"thread/status/changed",
		"codex/owner",
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("lost WARN = %q, want it to contain %q", got[0], want)
		}
	}

	owner.flushUnclaimed(start.Add(10*unclaimedGrace), false)
	if again := lostWarns(warns()); len(again) != 1 {
		t.Errorf("the orphan was reported again: %q", again[1:])
	}
}

// TestReaderStopReportsUnclaimedAtOnce: once the reader stops nothing can
// register a thread, so every waiting orphan is reported without waiting out
// the grace.
func TestReaderStopReportsUnclaimedAtOnce(t *testing.T) {
	owner, _, _, _, _ := newSharedPair(t)
	stopUnclaimedTimer(t, owner)
	warns := captureWarns(t)

	owner.dispatch([]byte(`{"method":"thread/goal/cleared","params":{"threadId":"orphan-at-stop"}}`))
	owner.stopUnclaimed()

	got := lostWarns(warns())
	if len(got) != 1 || !strings.Contains(got[0], "orphan-at-stop") {
		t.Errorf("lost WARNs at reader stop = %q, want one for orphan-at-stop", got)
	}
}

// TestUnclaimedTimerFires drives the real timer path: noteUnclaimed arms it,
// and when it fires flushUnclaimed runs. The grace is far longer than a test
// can wait, so the timer is reset to fire at once and the entry back-dated.
func TestUnclaimedTimerFires(t *testing.T) {
	owner, _, _, _, _ := newSharedPair(t)
	stopUnclaimedTimer(t, owner)
	got := make(chan string, 4)
	log.SetWarnHook(func(level log.Level, _ string, msg string) {
		if level.String() == "WARN" && strings.Contains(msg, "lost message") {
			got <- msg
		}
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	owner.dispatch([]byte(`{"method":"thread/goal/cleared","params":{"threadId":"timer-orphan"}}`))
	owner.unclaimed.mu.Lock()
	if owner.unclaimed.timer == nil {
		owner.unclaimed.mu.Unlock()
		t.Fatal("noteUnclaimed armed no timer")
	}
	owner.unclaimed.threads["timer-orphan"].firstSeen = time.Now().Add(-2 * unclaimedGrace)
	owner.unclaimed.timer.Reset(0)
	owner.unclaimed.mu.Unlock()

	select {
	case w := <-got:
		if !strings.Contains(w, "timer-orphan") {
			t.Errorf("timer WARN = %q, want it to name timer-orphan", w)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the unclaimed timer never reported the orphan")
	}
}

// TestForkedThreadIsClaimedUntilItsLeafRuns is #2720's regression: a depth
// fan-out forks one thread per leaf, but a leaf's branch facade resumes its
// thread only when the leaf runs (the user opens or answers it), minutes or
// hours later. codex sends the fork's mcpServer/startupStatus/updated and
// thread/tokenUsage/updated during and right after thread/fork, so before the
// fix every leaf's thread stayed unclaimed past the grace and logged one WARN
// (14 for a 14-leaf fan-out on the work Mac, 2026-10-31). A thread foci forked
// is known from the fork onwards: none of its lines may be reported lost, and
// none may reach the owner's chat or accounting.
func TestForkedThreadIsClaimedUntilItsLeafRuns(t *testing.T) {
	const forkID = "leaf-thread"
	var b *Backend
	b = setupMockBackend(t, func(method string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
		if method == "thread/fork" {
			// Lines codex sends during thread/fork, before the response names
			// the new thread (the reader dispatches them as they arrive).
			for _, line := range earlyForkLines(forkID)[:3] {
				b.dispatch(line)
			}
			return json.RawMessage(`{"thread":{"id":"` + forkID + `"}}`), nil
		}
		return nil, nil
	})
	stopUnclaimedTimer(t, b)
	warns := captureWarns(t)

	res, err := b.ForkSession(context.Background(), delegator.ForkRequest{ParentSessionID: "thread-1", FociSessionKey: "test/session"})
	if err != nil || res.SessionID != forkID {
		t.Fatalf("ForkSession = %+v, %v", res, err)
	}
	// Lines after the fork, while the leaf has not run yet.
	for _, line := range earlyForkLines(forkID)[3:] {
		b.dispatch(line)
	}

	b.flushUnclaimed(time.Now().Add(10*unclaimedGrace), false)
	if got := lostWarns(warns()); len(got) != 0 {
		t.Errorf("a forked thread whose leaf has not run yet was reported lost: %q", got)
	}
	b.turnMu.Lock()
	stashed := b.stashedUsage
	b.turnMu.Unlock()
	if stashed != nil {
		t.Errorf("the fork's token usage was stashed as the owner's: %+v", stashed)
	}
	if got := b.threadForSession("test/session"); got != "thread-1" {
		t.Errorf("the fork repointed the parent's session to thread %q, want thread-1", got)
	}

	// A real orphan on the same owner is still reported.
	b.dispatch([]byte(`{"method":"thread/goal/cleared","params":{"threadId":"orphan-thread"}}`))
	b.flushUnclaimed(time.Now().Add(10*unclaimedGrace), false)
	if got := lostWarns(warns()); len(got) != 1 || !strings.Contains(got[0], "orphan-thread") {
		t.Errorf("want one lost WARN for the orphan, got %q", got)
	}
}

// TestDeletedForkIsNoLongerClaimed: once foci deletes a forked thread
// (CleanupSession), a later line for it is an orphan again and is reported.
func TestDeletedForkIsNoLongerClaimed(t *testing.T) {
	const forkID = "deleted-leaf"
	b := setupMockBackend(t, func(method string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
		if method == "thread/fork" {
			return json.RawMessage(`{"thread":{"id":"` + forkID + `"}}`), nil
		}
		return nil, nil
	})
	stopUnclaimedTimer(t, b)
	warns := captureWarns(t)

	if _, err := b.ForkSession(context.Background(), delegator.ForkRequest{ParentSessionID: "thread-1"}); err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{SessionID: forkID}); err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	b.dispatch([]byte(`{"method":"thread/goal/cleared","params":{"threadId":"` + forkID + `"}}`))
	b.flushUnclaimed(time.Now().Add(10*unclaimedGrace), false)
	if got := lostWarns(warns()); len(got) != 1 {
		t.Errorf("want one lost WARN for a line after the fork was deleted, got %q", got)
	}
}

// threadDeletedLine is codex's notification for a deleted thread, as sent by
// the live app-server (0.145.0, #2867).
func threadDeletedLine(thread string) []byte {
	return []byte(`{"method":"thread/deleted","params":{"threadId":"` + thread + `"},"emittedAtMs":1793923572017}`)
}

// TestOwnThreadDeletedIsNotLost is #2867's regression. foci's transcript sweep
// deletes threads that have no session, and codex confirms each with
// thread/deleted. That confirmation is not a lost message: no WARN, and nothing
// reaches the owner. Live, the response comes first and the reader handles the
// notification right behind it, racing the caller's return from sendAndWait, so
// the "before the response" order must hold as well.
func TestOwnThreadDeletedIsNotLost(t *testing.T) {
	for _, tc := range []struct {
		name           string
		beforeResponse bool
	}{
		{"after the response", false},
		{"before the response", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const swept = "swept-thread"
			var b *Backend
			b = setupMockBackend(t, func(method string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
				if method == "thread/delete" && tc.beforeResponse {
					b.dispatch(threadDeletedLine(swept))
				}
				return nil, nil
			})
			stopUnclaimedTimer(t, b)
			warns := captureWarns(t)
			var shown []string
			b.sessionEvents.Store(&delegator.SessionEvents{OnText: func(s string) { shown = append(shown, s) }})

			if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{SessionID: swept}); err != nil {
				t.Fatalf("CleanupSession: %v", err)
			}
			if !tc.beforeResponse {
				b.dispatch(threadDeletedLine(swept))
			}
			b.flushUnclaimed(time.Now().Add(10*unclaimedGrace), false)

			if got := lostWarns(warns()); len(got) != 0 {
				t.Errorf("foci's own delete was reported lost: %q", got)
			}
			if len(shown) != 0 {
				t.Errorf("foci's own delete reached the owner's chat: %q", shown)
			}
		})
	}
}

// TestSweepThreadDeletedOnOwnerIsNotLost is the path the sweep takes: the
// sweeping Backend has no connection of its own, so CleanupSession uses the
// agent's pooled app-server, and the reader that sees thread/deleted is that
// owner's.
func TestSweepThreadDeletedOnOwnerIsNotLost(t *testing.T) {
	const agent, swept = "agent-2867", "swept-thread"
	owner := setupMockBackend(t, nil)
	owner.startOpts.AgentID = agent
	stopUnclaimedTimer(t, owner)
	warns := captureWarns(t)
	sharedPool.Lock()
	sharedPool.servers[agent] = owner
	sharedPool.Unlock()
	t.Cleanup(func() {
		sharedPool.Lock()
		delete(sharedPool.servers, agent)
		sharedPool.Unlock()
	})

	sweeper := &Backend{lg: log.NewComponentLogger("test")}
	if err := sweeper.CleanupSession(context.Background(), delegator.CleanupRequest{AgentID: agent, SessionID: swept}); err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	owner.dispatch(threadDeletedLine(swept))
	owner.flushUnclaimed(time.Now().Add(10*unclaimedGrace), false)
	if got := lostWarns(warns()); len(got) != 0 {
		t.Errorf("the sweep's delete was reported lost: %q", got)
	}
}

// TestForeignThreadDeletedIsStillReported: a thread/deleted that no delete of
// foci's explains keeps the lost-message report. That covers a thread foci never
// asked to delete, a second thread/deleted for one it did, and a thread codex
// said was already gone (live, that reply sends no thread/deleted, so a later
// one is somebody else's).
func TestForeignThreadDeletedIsStillReported(t *testing.T) {
	const asked, absent, foreign = "asked-thread", "absent-thread", "foreign-thread"
	b := setupMockBackend(t, func(method string, params json.RawMessage, _ int64) (json.RawMessage, error) {
		if method == "thread/delete" && strings.Contains(string(params), absent) {
			return nil, rpcErrorReply{code: -32600, message: "no rollout found for thread id " + absent}
		}
		return nil, nil
	})
	stopUnclaimedTimer(t, b)
	warns := captureWarns(t)
	for _, id := range []string{asked, absent} {
		if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{SessionID: id}); err != nil {
			t.Fatalf("CleanupSession(%s): %v", id, err)
		}
	}

	b.dispatch(threadDeletedLine(foreign))
	b.dispatch(threadDeletedLine(absent))
	b.dispatch(threadDeletedLine(asked)) // foci's own: consumed
	b.dispatch(threadDeletedLine(asked)) // nothing explains this one
	b.flushUnclaimed(time.Now().Add(10*unclaimedGrace), false)

	got := lostWarns(warns())
	if len(got) != 3 {
		t.Fatalf("want one lost WARN each for %s, %s and the repeat of %s, got %q", foreign, absent, asked, got)
	}
	for _, id := range []string{foreign, absent, asked} {
		if !strings.Contains(strings.Join(got, "\n"), "thread/deleted for thread "+id) {
			t.Errorf("no lost WARN for %s in %q", id, got)
		}
	}
}
