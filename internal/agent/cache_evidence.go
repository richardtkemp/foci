package agent

import (
	"time"

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

// branchRootWarm is a branch's pending root stamp: the branch inherited its
// source's conversation, so its first evidenced response read (or wrote) the
// shared prefix and warms the source and the root too.
type branchRootWarm struct {
	root string
	// source is the session the branch was forked from, and sourceRead the
	// cache-read count of the source's last evidenced call when the branch was
	// made. The branch's first call should read at least that much; less means
	// the fork missed the source's newest cache entry (a WARN, Dick's ruling
	// 2026-11-03) and did not refresh it, so it does not warm the source (#2929).
	source     string
	sourceRead int
	at         time.Time
}

// hasCacheEvidence reports whether a model response's usage proves it read or
// wrote the prompt cache (Dick's ruling, 2026-11-03).
func hasCacheEvidence(cacheRead, cacheWrite int) bool {
	return cacheRead > 0 || cacheWrite > 0
}

// recordCacheEvidence records one model response on sessionKey. With cache
// evidence it stamps the session's last_cache_touch and, for a branch made by
// NoteBranchInheritsRoot that has not stamped its root yet, the root's too.
// A response without cache tokens is not evidence and changes nothing.
func (a *Agent) recordCacheEvidence(sessionKey string, cacheRead, cacheWrite int) {
	if a.SessionIndex == nil || sessionKey == "" || !hasCacheEvidence(cacheRead, cacheWrite) {
		return
	}
	now := time.Now()
	a.SessionIndex.TouchCacheTouch(sessionKey, now)
	a.emitCacheExpiry(sessionKey)
	a.setMetaLocked(sessionKey, func(sm *sessionMeta) { sm.lastCallCacheRead = cacheRead })

	pending, ok := a.takeBranchRootWarm(sessionKey)
	if !ok {
		return
	}
	// A fork that read less than its source's last cached read missed the
	// source's newest cache entry: the entry was gone (2026-11-07 ~09:10, three
	// roots lost theirs inside the 1h TTL with byte-identical prefixes) or the
	// prefix differed. It refreshed nothing of the source's, and a fork cannot
	// re-create that entry (its cache breakpoint sits after its own unique
	// prompt, #2051), so the source stays as cold as it is. Stamping it here
	// kept keepalive forking hourly at a full cache write each (#2929).
	if pending.sourceRead > 0 && cacheRead < pending.sourceRead {
		a.logger().Warnf("cache: fork %s first call read %d cached tokens, less than its source %s's prior read of %d (fork missed part of the source's cache; cache_write=%d); not counted as warming the source",
			sessionKey, cacheRead, pending.source, pending.sourceRead, cacheWrite)
		return
	}
	// The branch carries its source's whole conversation, so its first call
	// read the source's prefix, which starts with the root's. Stamp both: the
	// source differs from the root when it is itself a branch (a facet open as
	// an app chat, which keepalive may warm).
	warmed := []string{pending.root}
	if pending.source != pending.root {
		warmed = append(warmed, pending.source)
	}
	for _, k := range warmed {
		a.SessionIndex.TouchCacheTouch(k, now)
		a.emitCacheExpiry(k)
	}
}

// NoteBranchInheritsRoot records that branchKey was made from parentKey and
// carries the parent's conversation, so the branch's first evidenced model
// response also warms the root's shared prefix. It writes nothing now: the
// root is stamped only when that response arrives (recordCacheEvidence). Call
// it only for a branch that inherits context: a fresh branch reads none of
// the root's prefix. No-op for a root key, a nil index or an empty key.
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
	// The baseline is the source's last evidenced call, not its turn-end
	// usage (prevCacheRead): a compaction or a turn still running moves the
	// source's prefix before its turn ends (#2929).
	a.metaMu.Lock()
	sourceRead := 0
	if m, ok := a.meta[parentKey]; ok {
		sourceRead = m.lastCallCacheRead
	}
	a.metaMu.Unlock()

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
	a.branchRootWarms[branchKey] = branchRootWarm{root: root, source: parentKey, sourceRead: sourceRead, at: now}
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
