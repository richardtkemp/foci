package codex

import (
	"encoding/json"
	"strings"
	"time"
)

// Releasing threads back to the app-server.
//
// One codex app-server serves every session of an agent and lives as long as
// any of them does — on a busy agent, for days. Codex keeps each thread it has
// loaded in memory with two files open: the rollout (rollout-*.jsonl) and the
// writer lock (~/.codex/thread-writer-locks/<id>.lock). Before this file, foci
// never told codex it was done with a thread, so every spawn, batch and closed
// session stayed loaded until the app-server exited. On the work Mac on
// 2026-11-05 one app-server held 101 locks and ~95 rollouts against a soft
// open-file limit of 256; a burst of parallel spawns crossed it, and every new
// thread failed with "Too many open files (os error 24)".
//
// thread/unsubscribe is the release. Verified live against codex app-server
// 0.166.0 with lsof:
//
//   - It returns at once ({"status":"unsubscribed"}), but codex unloads the
//     thread only once it has no subscriber AND is idle, and then only after a
//     60s grace: thread/loaded/list still lists it and both files stay open
//     until a thread/closed notification arrives ~60.0s later. Then both
//     files are closed.
//   - A resume after release works: within the grace it simply resubscribes;
//     after thread/closed codex reloads the thread from its rollout, history
//     intact (the model recalled its previous reply).
//   - A thread that never ran a turn has no rollout on disk. Once unloaded,
//     its resume fails "no rollout found" — the same outcome as resuming it
//     after any app-server restart; DelegatedManager falls back to a fresh
//     thread.
//   - A fork is written to disk at once, so a released fork resumes with
//     its history (depth leaves, character spawn branches).
//
// thread/archive is NOT used: it moves the rollout out of sessions/, which
// is not "done for now" but "gone from the list".

// threadClosingMarker is the substring of codex's thread/resume (and turn)
// error while a released thread is in the middle of unloading: "<id> is
// closing; retry thread/resume after the thread is closed" (codex 0.166.0
// binary). Only a resume that lands in that instant sees it.
const threadClosingMarker = "is closing; retry"

// resumeClosingRetry bounds how long resumeThread waits out that instant.
// Unloading is a file close and a map delete, so a few hundred ms is ample;
// the budget is generous so a slow disk does not turn a resume into a fresh
// thread.
var (
	resumeClosingRetryEvery = 250 * time.Millisecond
	resumeClosingRetryFor   = 10 * time.Second
)

// releaseThread hands this facade's thread back to the app-server and drops
// foci's mapping for it. Called by Close when sibling sessions keep the
// app-server alive (when the last facade closes, the app-server exits and
// every file goes with it).
//
// Subscriptions are per connection, not per facade: foci has ONE connection
// per app-server. So if another facade has since taken this thread over (a
// later session resumed it before this one finished closing), unsubscribing
// would unload it from under that live session, and unregistering would strip
// its routing. In that case this facade no longer owns the thread and leaves
// both alone.
func (b *Backend) releaseThread(threadID string) {
	if threadID == "" {
		return
	}
	p := b.process()
	p.threadMapMu.RLock()
	holder, mapped := p.threadBackends[threadID]
	p.threadMapMu.RUnlock()
	if mapped && holder != b {
		b.logDebugf("close: thread %s now belongs to another session; not releasing it", threadID)
		return
	}
	// Unsubscribe BEFORE unregistering: once unsubscribed, codex stops
	// sending this thread's lines, so none arrives unmapped and is reported
	// lost.
	b.unsubscribeThread(threadID)
	b.unregisterThread(threadID)
}

// unsubscribeThread asks the app-server to unload threadID once it is idle.
// Best effort: a dead app-server has already closed the files, and a failure
// here must never fail the caller's close.
func (b *Backend) unsubscribeThread(threadID string) {
	p := b.process()
	p.mu.Lock()
	ok := p.running && p.writer != nil && !p.closing
	p.mu.Unlock()
	if !ok {
		return
	}
	raw, err := b.sendAndWait("thread/unsubscribe", threadUnsubscribeParams{ThreadID: threadID})
	if err != nil {
		b.logWarnf("thread/unsubscribe %s failed (codex keeps it loaded until the app-server exits): %v", threadID, err)
		return
	}
	var res threadUnsubscribeResult
	_ = json.Unmarshal(raw, &res)
	p.threadMapMu.Lock()
	if p.releasedThreads == nil {
		p.releasedThreads = make(map[string]struct{})
	}
	p.releasedThreads[threadID] = struct{}{}
	p.threadMapMu.Unlock()
	b.logInfof("released thread %s (thread/unsubscribe: %s; codex unloads it once idle, after 60s)", threadID, res.Status)
}

// isReleased reports whether thread id was released by foci and codex has
// not unloaded it yet.
func (b *Backend) isReleased(id string) bool {
	p := b.process()
	p.threadMapMu.RLock()
	defer p.threadMapMu.RUnlock()
	_, ok := p.releasedThreads[id]
	return ok
}

// forgetReleased drops thread id from the released set: codex has unloaded it.
func (b *Backend) forgetReleased(id string) {
	p := b.process()
	p.threadMapMu.Lock()
	delete(p.releasedThreads, id)
	p.threadMapMu.Unlock()
}

// isThreadClosing reports whether err is codex refusing a thread that is in
// the middle of unloading.
func isThreadClosing(err error) bool {
	return err != nil && strings.Contains(err.Error(), threadClosingMarker)
}
