package codex

import (
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
		subagents: newSubagentTracker(),
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

// TestChildBindingClaimsEarlyLines: a subagent child's lines that came before
// the child was bound are not lost once registerChild binds it.
func TestChildBindingClaimsEarlyLines(t *testing.T) {
	owner, peer, _, _, _ := newSharedPair(t)
	stopUnclaimedTimer(t, owner)
	warns := captureWarns(t)

	owner.dispatch([]byte(`{"method":"thread/goal/cleared","params":{"threadId":"child-early"}}`))
	owner.registerChild("child-early", peer)

	owner.flushUnclaimed(time.Now().Add(10*unclaimedGrace), false)
	if got := lostWarns(warns()); len(got) != 0 {
		t.Errorf("a child bound after its early line was reported lost: %q", got)
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
