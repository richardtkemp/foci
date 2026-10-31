package codex

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// unclaimedGrace is how long a thread that matches no session and no known
// subagent may go unregistered before its lines are reported as lost.
//
// codex sends a thread's first notifications before foci can register the
// thread (#2700). A fork's mcpServer/startupStatus/updated and
// thread/tokenUsage/updated arrive during thread/fork, before its response
// names the new thread; a resumed thread's tokenUsage, thread/goal/cleared and
// thread/status/changed arrive during thread/resume, before the response that
// registerThread follows. On the work Mac on 2026-10-31 that was 106 WARNs over
// 22 threads, every one of them registered within the same second. Live, fork
// plus resume takes well under a second; the grace is generous so that a slow
// host never reports a normal fork, and short enough that a real orphan is
// still reported while it is current.
const unclaimedGrace = 30 * time.Second

// unclaimedThread is what the reader has seen from one thread that matched no
// session and no known subagent and has not been claimed since.
type unclaimedThread struct {
	firstSeen time.Time
	methods   map[string]int
}

// unclaimedLines holds the lost-line WARNs that are waiting out unclaimedGrace.
// It lives on the app-server owner (the reader's Backend). Its lock is never
// taken while threadMapMu is held.
type unclaimedLines struct {
	mu      sync.Mutex
	threads map[string]*unclaimedThread
	timer   *time.Timer
	stopped bool
}

// noteUnclaimed records that a line for thread id arrived when the thread
// matched no session and no known subagent. The WARN for it is deferred: it is
// logged by flushUnclaimed only if the thread is still unclaimed after
// unclaimedGrace, and never if registerThread or registerChild claims it first.
// The line itself is not held back: dispatch hands it to the owner at once
// (dispatchLost), exactly as before.
func (b *Backend) noteUnclaimed(id, method string, now time.Time) {
	p := b.process()
	u := &p.unclaimed
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stopped {
		// The reader has stopped, so nothing can claim the thread any more.
		// Not reached in practice (only the reader calls this), but a line
		// after the stop must not be held forever.
		p.logWarnf("%s", p.lostWarning(id, map[string]int{method: 1}))
		return
	}
	if u.threads == nil {
		u.threads = make(map[string]*unclaimedThread)
	}
	t := u.threads[id]
	if t == nil {
		t = &unclaimedThread{firstSeen: now, methods: make(map[string]int)}
		u.threads[id] = t
	}
	t.methods[method]++
	if u.timer == nil {
		u.timer = time.AfterFunc(unclaimedGrace, func() { p.flushUnclaimed(time.Now(), false) })
	}
}

// claimUnclaimed forgets thread id's deferred lost-line WARN: the thread has
// been registered to a session or bound as a subagent, so its early lines were
// not lost, only early. Called by registerThread and registerChild after they
// release threadMapMu.
func (b *Backend) claimUnclaimed(id string) {
	p := b.process()
	u := &p.unclaimed
	u.mu.Lock()
	delete(u.threads, id)
	u.mu.Unlock()
}

// flushUnclaimed logs the lost-line WARN for every thread that has stayed
// unclaimed for unclaimedGrace, one WARN per thread naming each method and how
// many lines of it arrived. With all set (the reader stopped, so no thread can
// be claimed any more) it logs every remaining thread regardless of age and
// arms nothing further. A thread found registered here is dropped silently, as
// a backstop to claimUnclaimed. The timer re-arms while threads are waiting.
func (b *Backend) flushUnclaimed(now time.Time, all bool) {
	p := b.process()
	u := &p.unclaimed
	u.mu.Lock()
	u.timer = nil
	if all {
		u.stopped = true
	}
	var warn []string
	var next time.Duration
	ids := make([]string, 0, len(u.threads))
	for id := range u.threads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := u.threads[id]
		if p.threadClaimed(id) {
			delete(u.threads, id)
			continue
		}
		age := now.Sub(t.firstSeen)
		if all || age >= unclaimedGrace {
			warn = append(warn, p.lostWarning(id, t.methods))
			delete(u.threads, id)
			continue
		}
		if wait := unclaimedGrace - age; next == 0 || wait < next {
			next = wait
		}
	}
	if !all && len(u.threads) > 0 {
		u.timer = time.AfterFunc(next, func() { p.flushUnclaimed(time.Now(), false) })
	}
	u.mu.Unlock()
	for _, w := range warn {
		p.logWarnf("%s", w)
	}
}

// stopUnclaimed reports every still-unclaimed thread at once and stops the
// timer. Called when the reader stops.
func (b *Backend) stopUnclaimed() {
	p := b.process()
	p.unclaimed.mu.Lock()
	if p.unclaimed.timer != nil {
		p.unclaimed.timer.Stop()
	}
	p.unclaimed.mu.Unlock()
	p.flushUnclaimed(time.Now(), true)
}

// threadClaimed reports whether thread id now belongs to a session or a known
// subagent. Takes threadMapMu (read) under unclaimed.mu, the permitted order.
func (b *Backend) threadClaimed(id string) bool {
	if b.facadeForThread(id) != nil || b.parentOfChild(id) != nil {
		return true
	}
	return b.subagents != nil && b.subagents.isChild(id)
}

// lostWarning is the lost-line WARN text for thread id: every method seen
// from it, in name order, with a count where more than one line arrived.
func (b *Backend) lostWarning(id string, methods map[string]int) string {
	names := make([]string, 0, len(methods))
	for m := range methods {
		names = append(names, m)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, m := range names {
		parts[i] = m
		if n := methods[m]; n > 1 {
			parts[i] = fmt.Sprintf("%s (x%d)", m, n)
		}
	}
	return fmt.Sprintf("lost message: %s for thread %s matches no session and no known subagent; "+
		"handed it to the app-server owner (session %s), whose chat may show it",
		strings.Join(parts, ", "), id, b.startOpts.SessionKey)
}
