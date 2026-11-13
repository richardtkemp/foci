package agent

import (
	"slices"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/session"
)

// Cache warmth is evidence, not assumption (#2509).
//
// last_cache_touch is the time of the most recent model response that read or
// wrote this session's cached prompt prefix. It is written ONLY here, from a
// model response's usage: a response with cache-read or cache-write tokens
// above zero (Dick's ruling, 2026-11-03). Nothing stamps it when a turn starts,
// when a backend accepts input, or when a branch is created, so a turn whose
// model call fails, or never happens, leaves the old value in place and every
// warmth decision (keepalive's warm window, the quiet-hours, usage-hold and
// pre-cold guards, the app indicator) sees the cache age honestly.
//
// Evidence sources:
//   - API transport: every response, in processAPIResponse.
//   - Delegated transport: every top-level call, through
//     TurnEvents.OnModelUsage, with the turn's final usage at OnTurnComplete
//     as the fallback for a backend that reports no per-call usage.

// branchRootWarmTTL bounds how long a branch's pending root stamp waits for
// its first evidenced response. A branch that never produces one (its model
// call failed) leaves nothing to stamp; the entry is pruned after this.
const branchRootWarmTTL = time.Hour

// branchRootWarm is a branch's pending ancestor stamp: the branch inherited
// its source's conversation, so its first evidenced response read the cache
// entry that conversation ends at, and warms the sessions that entry belongs
// to.
type branchRootWarm struct {
	// warms is the sessions whose newest cache entry the branch's prefix ends
	// at, nearest first: the source, then, while the session just listed had
	// made no model call of its own since it was forked (it still ended at its
	// own parent's entry), that session's parent, and so on (#3089). A session
	// that had a call of its own has its own, longer entry: a fork of it reads
	// that entry, not its parent's, so the walk stops there (Dick, 2026-11-13:
	// "Parent -> forks -> fork1 activity -> forks -> runs keepalive in fork2
	// -> parent cache is not warmed"). A key is "" when that session was reset
	// after the branch was made (forgetBranchRootWarmsFor, #2967).
	warms []string
	// source is the session the branch was forked from, and sourceCached the
	// cache read plus write of the source's last evidenced call when the
	// branch was made: the prefix that call left cached. The branch's first
	// call should read about that much; well short of it means the fork missed
	// the source's newest cache entry (a WARN, Dick's ruling 2026-11-03) and
	// did not refresh it, so it warms nothing (#2929), and keepalive stops for
	// the source until its next own call (#3089). 0 = no baseline to check.
	source       string
	sourceCached int
	at           time.Time
}

// hasCacheEvidence reports whether a model response's usage proves it read or
// wrote the prompt cache (Dick's ruling, 2026-11-03).
func hasCacheEvidence(cacheRead, cacheWrite int) bool {
	return cacheRead > 0 || cacheWrite > 0
}

// recordCacheEvidence records one model response on sessionKey. With cache
// evidence it stamps the session's last_cache_touch and, for a branch made by
// NoteBranchInheritsRoot that has not stamped its ancestors yet, theirs too.
// A response without cache tokens is not evidence and changes nothing.
func (a *Agent) recordCacheEvidence(sessionKey string, cacheRead, cacheWrite int) {
	if a.SessionIndex == nil || sessionKey == "" || !hasCacheEvidence(cacheRead, cacheWrite) {
		return
	}
	now := time.Now()
	a.SessionIndex.TouchCacheTouch(sessionKey, now)
	a.emitCacheExpiry(sessionKey)
	a.setMetaLocked(sessionKey, func(sm *sessionMeta) { sm.lastCallCached = cacheRead + cacheWrite })
	// The session's own call is the activity that ends a keepalive stop
	// (#3089): it wrote the session's newest prefix, which a fork can read.
	a.resumeKeepalive(sessionKey)

	pending, ok := a.takeBranchRootWarm(sessionKey)
	if !ok {
		return
	}
	// A fork that read well short of its source's last cached prefix missed
	// the source's newest cache entry: the entry was gone (2026-11-07 ~09:10,
	// three roots lost theirs inside the 1h TTL with byte-identical prefixes)
	// or the prefix differed (13 Nov, every fork of a just-compacted session
	// read 51,752 of 65,882, #3086). It refreshed nothing of the source's, and
	// a fork cannot re-create that entry (its cache breakpoint sits after its
	// own unique prompt, #2051), so the source stays as cold as it is.
	// Stamping it kept keepalive forking hourly at a full cache write each
	// (#2929). A later fork would miss the same way, so keepalive stops for
	// the source until the source's own next call (Dick, 2026-11-13, #3089).
	if accounting.ForkReadShort(pending.sourceCached, cacheRead) {
		// INFO: the per-call detector (accounting/cachewatch.go) reports the miss at WARN (#2930).
		a.logger().Infof("cache: fork %s first call read %d cached tokens, short of its source %s's cached prefix of %d (fork missed part of the source's cache; cache_write=%d); not counted as warming the source; keepalive stops for %s until its next own call",
			sessionKey, cacheRead, pending.source, pending.sourceCached, cacheWrite, pending.source)
		a.stopKeepalive(pending.source)
		return
	}
	// The branch's first call read the cache entry its prefix ends at, which
	// belongs to every session in warms.
	for _, k := range pending.warms {
		if k == "" {
			continue
		}
		a.SessionIndex.TouchCacheTouch(k, now)
		a.emitCacheExpiry(k)
		a.resumeKeepalive(k)
	}
}

// KeepaliveStopped reports whether keepalive is stopped for sessionKey (#3089):
// a fork of it missed its cache, and it has made no model call of its own
// since. A further keepalive fork would miss the same way and warm nothing.
func (a *Agent) KeepaliveStopped(sessionKey string) bool {
	a.branchRootMu.Lock()
	defer a.branchRootMu.Unlock()
	return a.keepaliveStopped[sessionKey]
}

// stopKeepalive records that a fork of sessionKey missed its cache (#3089).
func (a *Agent) stopKeepalive(sessionKey string) {
	if sessionKey == "" {
		return
	}
	a.branchRootMu.Lock()
	defer a.branchRootMu.Unlock()
	if a.keepaliveStopped == nil {
		a.keepaliveStopped = make(map[string]bool)
	}
	a.keepaliveStopped[sessionKey] = true
}

// resumeKeepalive lifts sessionKey's keepalive stop: its cache entry was just
// read or written, so a fork of it can refresh that entry again.
func (a *Agent) resumeKeepalive(sessionKey string) {
	a.branchRootMu.Lock()
	defer a.branchRootMu.Unlock()
	if a.keepaliveStopped[sessionKey] {
		delete(a.keepaliveStopped, sessionKey)
		a.logger().Infof("cache: keepalive resumes for %s (its cache entry was refreshed)", sessionKey)
	}
}

// forgetBranchRootWarmsFor stops every pending branch stamp from warming
// sessionKey, which was just reset (#2967). A branch made before the reset
// (the reset's own session-end-memory branch, or a keepalive fork in flight)
// carries the discarded conversation, so its first call reads that
// conversation's cache. The reset session has no conversation and no cache
// until its next real turn writes one; stamping it would make keepalive warm
// a session that does not exist. The branch itself and any other session the
// pending stamp names (a facet source under a reset root) are still stamped.
// A keepalive stop on the reset session goes too: it was about the discarded
// conversation (#3089).
func (a *Agent) forgetBranchRootWarmsFor(sessionKey string) {
	a.branchRootMu.Lock()
	defer a.branchRootMu.Unlock()
	delete(a.keepaliveStopped, sessionKey)
	for k, v := range a.branchRootWarms {
		warms := slices.Clone(v.warms)
		for i, w := range warms {
			if w == sessionKey {
				warms[i] = ""
			}
		}
		v.warms = warms
		if v.source == sessionKey {
			v.source = ""
		}
		a.branchRootWarms[k] = v
	}
}

// NoteBranchInheritsRoot records that branchKey was made from parentKey and
// carries the parent's conversation, so the branch's first evidenced model
// response also warms the cache entry that conversation ends at: the
// parent's, and the parent's own ancestors' while the parent had made no call
// of its own (branchRootWarm.warms, #3089). It writes nothing now: they are
// stamped only when that response arrives (recordCacheEvidence). Call it only
// for a branch that inherits context: a fresh branch reads none of the
// parent's prefix. No-op for a root key, a nil index or an empty key.
func (a *Agent) NoteBranchInheritsRoot(branchKey, parentKey string) {
	if a.SessionIndex == nil || branchKey == "" {
		return
	}
	sk, err := session.ParseSessionKey(branchKey)
	if err != nil {
		return
	}
	root := sk.Root().String()
	if root == branchKey {
		return
	}
	if parentKey == "" {
		parentKey = root
	}
	// The ledger's cache watch checks the fork's first call against the
	// source's last call (read + write) as of now, and reports a miss with its
	// cost and cause (#2930). Its figure is the baseline here too, so the two
	// agree on what a miss is (#3089): on 13 Nov the source's last call read
	// 10,461 and wrote 55,421, every fork read 51,752, the watch reported a
	// miss and this check, comparing reads alone, counted a hit. It survives
	// a restart (#2932), and says "nothing to check" after a compaction or
	// reset, where the source's last call was over a conversation that is gone.
	sourceCached, known := accounting.NoteCacheFork(branchKey, parentKey)
	if !known {
		// No booked call to go by (a backend that books nothing, or a test):
		// the source's last evidenced call, not its turn-end usage
		// (prevCacheRead): a compaction or a turn still running moves the
		// source's prefix before its turn ends (#2929).
		a.metaMu.Lock()
		if m, ok := a.meta[parentKey]; ok {
			sourceCached = m.lastCallCached
		}
		a.metaMu.Unlock()
	}

	now := time.Now()
	a.branchRootMu.Lock()
	defer a.branchRootMu.Unlock()
	if a.branchRootWarms == nil {
		a.branchRootWarms = make(map[string]branchRootWarm)
	}
	for k, v := range a.branchRootWarms {
		if now.Sub(v.at) > branchRootWarmTTL {
			delete(a.branchRootWarms, k)
		}
	}
	// A parent still pending has made no call of its own since it was forked:
	// its conversation ends at its own parent's entry, so the new branch's
	// does too. A parent with none pending has had its own call (or is a root,
	// or its stamp was pruned or lost to a restart: then the walk stops early
	// and an ancestor is stamped less, never wrongly).
	warms := []string{parentKey}
	if p, ok := a.branchRootWarms[parentKey]; ok {
		warms = append(warms, p.warms...)
	}
	a.branchRootWarms[branchKey] = branchRootWarm{warms: warms, source: parentKey, sourceCached: sourceCached, at: now}
}

// takeBranchRootWarm removes and returns branchKey's pending root stamp. The
// root is stamped once per branch: only the branch's first call reads the
// root's own cached prefix; later calls read the branch's.
func (a *Agent) takeBranchRootWarm(branchKey string) (branchRootWarm, bool) {
	a.branchRootMu.Lock()
	defer a.branchRootMu.Unlock()
	p, ok := a.branchRootWarms[branchKey]
	if ok {
		delete(a.branchRootWarms, branchKey)
	}
	return p, ok
}
