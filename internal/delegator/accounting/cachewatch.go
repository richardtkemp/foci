package accounting

import (
	"fmt"
	"sync"
	"time"

	"foci/internal/modelinfo"
)

// Cache watch (#2930): every booked call, on every backend, is checked for a
// cache read below what its request should have read.
//
// A call's request carries everything the previous call in its lineage sent,
// plus new content, so its cache read should be at least the previous call's
// read plus its write. The ledger shows that holds exactly for nearly every
// call (2026-11-01..07: 15,111 of 15,133 same-model calls under 5 minutes
// apart). A read well below it means the prefix was not in the cache: the
// check keys on the read, never on a large write, because a large write can be
// genuine (Dick, 2026-11-07 14:54).
//
// A lineage is one (session, actor): a subagent has its own conversation and
// its own cache. A compaction or a deliberate reset starts a new lineage, which
// is not checked on its first call. A fork's first call is checked against its
// source: it carries the source's whole conversation. A model switch or a
// prompt change that drops the read is a bust, reported with that cause (Dick,
// 2026-11-07). A read lost because the cache's TTL elapsed since the lineage
// last touched it is an expiry, reported apart from a bust.

// Cache report classes and causes.
const (
	CacheClassBust   = "bust"
	CacheClassExpiry = "expiry"

	// CacheCauseExpiry: the TTL elapsed since the prefix was last touched.
	CacheCauseExpiry = "expiry"
	// CacheCauseModelSwitch: the call ran a different model from the previous
	// call; the cache is per model.
	CacheCauseModelSwitch = "model_switch"
	// CacheCausePromptChanged: the session's backend was launched with a
	// different system prompt from the previous call's (or a fork's from its
	// source's).
	CacheCausePromptChanged = "prompt_changed"
	// CacheCauseForkMiss: a fork's first call read less of its source's
	// prefix than the source had cached, for a reason not named above.
	CacheCauseForkMiss = "fork_miss"
	// CacheCausePrefixChanged: the request's prefix differed from the previous
	// call's after Read tokens (tools, system prompt or history), for a reason
	// not named above.
	CacheCausePrefixChanged = "prefix_changed"
)

// cacheSlackFrac and cacheSlackMin bound the shortfall that is not reported:
// a fork's first call can re-render the last few thousand tokens of its
// source's tail (03:43 7 Nov: forks read 85,876 of a source's 88,465, 2.9%
// short), and that is not worth a report. A shortfall above both is.
const (
	cacheSlackFrac = 0.05
	cacheSlackMin  = 2048
)

// cacheDefaultTTL is the TTL assumed for a lineage whose writes have not
// named one (no write seen yet, or a provider's untyped cache_write).
const cacheDefaultTTL = 5 * time.Minute

// CacheReport is one call whose cache read fell short of what its request
// should have read.
type CacheReport struct {
	Session, Actor, Backend, Model string
	// AgentID is the agent the call ran as.
	AgentID string
	At      time.Time
	// Class is CacheClassBust or CacheClassExpiry; Cause names the likely
	// cause (a CacheCause* constant).
	Class, Cause string
	// Expected is what the call should have read (the baseline's read plus
	// write); Read is what it read; Lost is the difference.
	Expected, Read, Lost int
	// Write is what the call wrote.
	Write int
	// Since is the time since the prefix was last touched; TTL the cache TTL
	// it was written with.
	Since, TTL time.Duration
	// Source is the session a fork was made from, on a fork's first call.
	Source string
	// PrevModel is the baseline call's model.
	PrevModel string
	// RewriteUSD is what writing the lost tokens cost, and ReadUSD what
	// reading them would have cost. Priced is false when the model has no
	// rate for either class.
	RewriteUSD, ReadUSD float64
	Priced              bool
}

// String is the report as one log line.
func (r CacheReport) String() string {
	cost := "unpriced"
	if r.Priced {
		cost = fmt.Sprintf("rewrite $%.2f (a hit would have cost $%.2f)", r.RewriteUSD, r.ReadUSD)
	}
	s := fmt.Sprintf("cache %s: session %s", r.Class, r.Session)
	if r.Actor != "" {
		s += " actor " + r.Actor
	}
	s += fmt.Sprintf(" read %d of an expected %d cached tokens (lost %d, wrote %d), %s; cause %s",
		r.Read, r.Expected, r.Lost, r.Write, cost, r.Cause)
	switch r.Cause {
	case CacheCauseModelSwitch:
		s += fmt.Sprintf(" (%s -> %s)", r.PrevModel, r.Model)
	case CacheCauseForkMiss, CacheCausePrefixChanged:
		s += fmt.Sprintf(" (prefix diverged after %d tokens)", r.Read)
	}
	if r.Source != "" {
		s += " ; fork of " + r.Source
	}
	return s + fmt.Sprintf("; %s since last touch, ttl %s; model %s, backend %s",
		r.Since.Round(time.Second), r.TTL, r.Model, r.Backend)
}

// CacheReportHook, when set, receives every cache report. The gateway logs
// them and posts each bust to a chat (#2933). Called synchronously after a
// booking commits: it must be quick.
var CacheReportHook func(CacheReport)

// cacheLineage is the last checked call of one (session, actor).
type cacheLineage struct {
	model       string
	read, write int
	// prompt is the session's launch prompt fingerprint at the call, "" when
	// unknown (a direct-API session, a lineage seeded from api.db).
	prompt string
	at     time.Time
	// touched is when the prefix was last read or written: this lineage's
	// own call, or a fork's first call carrying it.
	touched time.Time
	ttl     time.Duration
	id      int64
	// fresh marks a lineage a compaction or reset started: its next call has
	// nothing to be checked against.
	fresh bool
}

type cacheWatch struct {
	mu       sync.Mutex
	lineages map[string]*cacheLineage
	// forks is each fork not yet seen to call: its source, and a snapshot of
	// the source's lineage when the fork was made.
	forks map[string]cacheFork
	// prompts is each session's current launch prompt fingerprint.
	prompts map[string]string
	// observed counts observations, to prune every cachePruneEvery.
	observed int
}

// A lineage untouched for cachePruneAge is forgotten (every subagent is a
// lineage, so the map would otherwise grow for the process's life). That
// loses nothing a check needs: past every TTL its next call is an expiry, and
// its baseline is re-seeded from api.db.
const (
	cachePruneAge   = 2 * time.Hour
	cachePruneEvery = 512
)

// pruneLocked forgets stale lineages, and the prompt of a session with
// neither a lineage nor a pending fork.
func (w *cacheWatch) pruneLocked(now time.Time) {
	live := make(map[string]bool)
	for k, ln := range w.lineages {
		if now.Sub(ln.touched) > cachePruneAge && now.Sub(ln.at) > cachePruneAge {
			delete(w.lineages, k)
			continue
		}
		for i := 0; i < len(k); i++ {
			if k[i] == 0 {
				live[k[:i]] = true
				break
			}
		}
	}
	for s := range w.prompts {
		if _, fork := w.forks[s]; !live[s] && !fork {
			delete(w.prompts, s)
		}
	}
}

type cacheFork struct {
	source string
	base   *cacheLineage
	at     time.Time
}

// cacheForkTTL bounds how long a noted fork waits for its first call.
const cacheForkTTL = time.Hour

// watch is the process's cache watch: one, like the live ledger.
var watch = newCacheWatch()

func newCacheWatch() *cacheWatch {
	return &cacheWatch{
		lineages: make(map[string]*cacheLineage),
		forks:    make(map[string]cacheFork),
		prompts:  make(map[string]string),
	}
}

func lineageKey(session, actor string) string { return session + "\x00" + actor }

// NoteCacheFork records that branch was made from source and carries its
// conversation, so the branch's first call is checked against the source's
// last call. Call it when the fork is made: a source that keeps working
// afterwards must not raise what the fork is expected to read.
//
// It returns the cache read of the source's last call, seeded from api.db
// when this process has not seen one (a restart), or 0 when there is none or
// the source was just compacted or reset. The agent's warm decision uses it
// as its fork-miss baseline when it holds none of its own (#2932).
func NoteCacheFork(branch, source string) int { return watch.noteFork(branch, source, time.Now()) }

func (w *cacheWatch) noteFork(branch, source string, now time.Time) int {
	if branch == "" || source == "" || branch == source {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, f := range w.forks {
		if now.Sub(f.at) > cacheForkTTL {
			delete(w.forks, k)
		}
	}
	base := w.lineageLocked(source, "", 0, now)
	var snap *cacheLineage
	sourceRead := 0
	if base != nil && !base.fresh {
		c := *base
		snap = &c
		sourceRead = c.read
	}
	// A fork replaces whatever its key held: it is a new conversation.
	w.dropSessionLocked(branch)
	w.forks[branch] = cacheFork{source: source, base: snap, at: now}
	return sourceRead
}

// NoteCacheReset records that session's conversation was reset or compacted:
// its next call starts a new lineage and is not checked.
func NoteCacheReset(session string) { watch.noteReset(session, time.Now()) }

func (w *cacheWatch) noteReset(session string, now time.Time) {
	if session == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropSessionLocked(session)
	delete(w.forks, session)
	w.lineages[lineageKey(session, "")] = &cacheLineage{fresh: true, at: now}
}

// NoteCachePrompt records the fingerprint of the system prompt session's
// backend was launched with. A later call under a different fingerprint that
// drops the read is reported as a prompt change.
func NoteCachePrompt(session, hash string) { watch.notePrompt(session, hash) }

func (w *cacheWatch) notePrompt(session, hash string) {
	if session == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prompts[session] = hash
}

// dropSessionLocked forgets every lineage of session.
func (w *cacheWatch) dropSessionLocked(session string) {
	prefix := session + "\x00"
	for k := range w.lineages {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(w.lineages, k)
		}
	}
}

// lineageLocked returns the lineage of (session, actor), seeding it from the
// live ledger's last call before 'before' when the process has not seen one
// (a restart). Returns nil when there is none. exclude is a call id the seed
// must skip (the call being checked, already committed).
func (w *cacheWatch) lineageLocked(session, actor string, exclude int64, before time.Time) *cacheLineage {
	k := lineageKey(session, actor)
	if ln, ok := w.lineages[k]; ok {
		return ln
	}
	ln := seedLineage(Live(), session, actor, exclude, before)
	if ln != nil {
		w.lineages[k] = ln
	}
	return ln
}

// seedLineage reads (session, actor)'s last call with cache tokens from l.
func seedLineage(l *Ledger, session, actor string, exclude int64, before time.Time) *cacheLineage {
	if l == nil {
		return nil
	}
	// The candidates come off the (session, billed_at) index first, so a long
	// session's history is not grouped whole.
	row := l.db.QueryRow(`WITH c AS (
			SELECT id, billed_at, model, kind FROM api_calls
			WHERE session = ? AND actor = ? AND kind IN ('call', 'compaction')
			  AND billed_at <= ? AND id <> ?
			ORDER BY billed_at DESC, id DESC LIMIT 20)
		SELECT c.id, c.billed_at, c.model, c.kind,
			COALESCE(SUM(CASE WHEN t.class = 'cache_read' THEN t.count END), 0) AS r,
			COALESCE(SUM(CASE WHEN t.class LIKE 'cache_write%' THEN t.count END), 0) AS w,
			COALESCE(MAX(t.class = 'cache_write_1h'), 0),
			COALESCE(MAX(t.class = 'cache_write_5m'), 0)
		FROM c LEFT JOIN call_tokens t ON t.call_id = c.id
		GROUP BY c.id HAVING r + w > 0
		ORDER BY c.billed_at DESC, c.id DESC LIMIT 1`,
		session, actor, formatTime(before), exclude)
	var (
		id                  int64
		at, model, kind     string
		r, wr, has1h, has5m int
	)
	if err := row.Scan(&id, &at, &model, &kind, &r, &wr, &has1h, &has5m); err != nil {
		return nil // sql.ErrNoRows: nothing to check against
	}
	t, err := time.Parse(timeLayout, at)
	if err != nil {
		return nil
	}
	ln := &cacheLineage{model: model, read: r, write: wr, at: t, touched: t, id: id,
		ttl: writeTTL(has1h > 0, has5m > 0, cacheDefaultTTL)}
	if kind == KindCompaction {
		ln.fresh = true
	}
	return ln
}

// writeTTL is the TTL of a cache written with the given classes; prev when
// the call wrote neither (it refreshed an entry, keeping its TTL).
func writeTTL(has1h, has5m bool, prev time.Duration) time.Duration {
	switch {
	case has1h:
		return time.Hour
	case has5m:
		return 5 * time.Minute
	}
	return prev
}

// cacheSlack is the shortfall below expected that is not reported.
func cacheSlack(expected int) int {
	s := int(float64(expected) * cacheSlackFrac)
	if s < cacheSlackMin {
		s = cacheSlackMin
	}
	return s
}

// observeCache checks one committed booking and advances its lineage.
func observeCache(b Booking) {
	if r, ok := watch.observe(b); ok && CacheReportHook != nil {
		CacheReportHook(r)
	}
}

func (w *cacheWatch) observe(b Booking) (CacheReport, bool) {
	if b.Kind != KindCall && b.Kind != KindCompaction {
		return CacheReport{}, false
	}
	read := b.Tokens[modelinfo.ClassCacheRead]
	write := b.Tokens[modelinfo.ClassCacheWrite5m] + b.Tokens[modelinfo.ClassCacheWrite1h] + b.Tokens[modelinfo.ClassCacheWrite]
	// A call with no cache tokens is no evidence either way (a provider that
	// reports none, a usage-less row): it is neither checked nor a baseline.
	if read == 0 && write == 0 {
		return CacheReport{}, false
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.observed++; w.observed%cachePruneEvery == 0 {
		w.pruneLocked(b.BilledAt)
	}
	k := lineageKey(b.Session, b.Actor)
	prompt := w.prompts[b.Session]

	var (
		base   *cacheLineage
		source string
	)
	if f, isFork := w.forks[b.Session]; isFork && b.Actor == "" {
		if _, seen := w.lineages[k]; !seen {
			delete(w.forks, b.Session)
			base, source = f.base, f.source
			// The source may have been touched since the fork was made
			// (another fork's first call): the TTL runs from the latest.
			if src, ok := w.lineages[lineageKey(source, "")]; ok && base != nil && src.touched.After(base.touched) {
				c := *base
				c.touched = src.touched
				base = &c
			}
		}
	}
	if source == "" {
		base = w.lineageLocked(b.Session, b.Actor, b.ID, b.BilledAt)
		// A late or repeated booking (an extended subagent share) is not a
		// new call of the lineage.
		if base != nil && (b.ID != 0 && b.ID == base.id || b.BilledAt.Before(base.at)) {
			return CacheReport{}, false
		}
	}

	var (
		report CacheReport
		found  bool
	)
	if base != nil && !base.fresh {
		report, found = checkCache(b, base, source, prompt, read, write)
	}

	ttl := cacheDefaultTTL
	if base != nil && base.ttl > 0 {
		ttl = base.ttl
	}
	next := &cacheLineage{
		model: b.Model, read: read, write: write, prompt: prompt,
		at: b.BilledAt, touched: b.BilledAt, id: b.ID,
		ttl: writeTTL(b.Tokens[modelinfo.ClassCacheWrite1h] > 0, b.Tokens[modelinfo.ClassCacheWrite5m] > 0, ttl),
	}
	// After a compaction the conversation is a new one: its next call reads
	// only what the summary shares with it.
	next.fresh = b.Kind == KindCompaction
	w.lineages[k] = next

	// A fork's first call carries its source's prefix, so it touched the
	// source's cache entry: it read it, or rewrote it (#2509 stamps the same).
	if source != "" {
		if src, ok := w.lineages[lineageKey(source, "")]; ok && src.touched.Before(b.BilledAt) {
			src.touched = b.BilledAt
		}
	}
	return report, found
}

// checkCache compares one call with its lineage's baseline.
func checkCache(b Booking, base *cacheLineage, source, prompt string, read, write int) (CacheReport, bool) {
	expected := base.read + base.write
	lost := expected - read
	if lost <= cacheSlack(expected) {
		return CacheReport{}, false
	}
	since := b.BilledAt.Sub(base.touched)
	r := CacheReport{
		Session: b.Session, Actor: b.Actor, Backend: b.Backend, Model: b.Model, AgentID: b.AgentID, At: b.BilledAt,
		Class: CacheClassBust, Expected: expected, Read: read, Lost: lost, Write: write,
		Since: since, TTL: base.ttl, Source: source, PrevModel: base.model,
	}
	switch {
	case since > base.ttl:
		r.Class, r.Cause = CacheClassExpiry, CacheCauseExpiry
	case b.Model != base.model:
		r.Cause = CacheCauseModelSwitch
	case prompt != "" && base.prompt != "" && prompt != base.prompt:
		r.Cause = CacheCausePromptChanged
	case source != "":
		r.Cause = CacheCauseForkMiss
	default:
		r.Cause = CacheCausePrefixChanged
	}
	writeClass := modelinfo.ClassCacheWrite5m
	switch {
	case b.Tokens[modelinfo.ClassCacheWrite1h] > 0:
		writeClass = modelinfo.ClassCacheWrite1h
	case b.Tokens[modelinfo.ClassCacheWrite] > 0:
		writeClass = modelinfo.ClassCacheWrite
	}
	rw, ok1 := modelinfo.CostAsOf(b.Model, b.BilledAt, modelinfo.Tokens{writeClass: lost})
	rd, ok2 := modelinfo.CostAsOf(b.Model, b.BilledAt, modelinfo.Tokens{modelinfo.ClassCacheRead: lost})
	r.RewriteUSD, r.ReadUSD, r.Priced = rw, rd, ok1 && ok2
	return r, true
}
