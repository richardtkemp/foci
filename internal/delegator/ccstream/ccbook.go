// ccbook.go — the Claude Code adapter of the cost ledger (#2111 §3.1, §4, §5),
// as a single-threaded state machine. ccshadow.go feeds it events from the
// stream, the main-transcript tail and the subagent tails, in the order they
// happened, and runs its settle clock.
//
// The rules, each pinned by a test in ccbook_test.go:
//
//   - A MAIN-thread call is booked from its main-transcript line, once. The
//     stream decides WHICH calls happened in this process: a transcript line
//     whose id the stream never named is a copy — CC re-appends the whole prior
//     history after every 2nd and later compaction (F1b) — and is dropped
//     without touching the ledger. A named id books on the turn open when the
//     stream named it.
//   - Finality (F1): a main line with a stop_reason is final. A named call seen
//     only without one was INTERRUPTED (P0-e): it is booked from its line at the
//     run's result, PRICED (Dick, 2026-09-29 12:08), and kept out of the
//     remainder subtraction, because CC counts it nowhere.
//   - A SUBAGENT call is booked from its own transcript on the turn that
//     spawned the subagent, completed on its stop_reason line, or — never
//     completed — as `stopless` at its last-seen counts when a later id appears
//     in the same file or the tail closes. Stopless calls ARE counted by CC, so
//     they are in the remainder subtraction.
//   - The REMAINDER — what CC's cumulative modelUsage counts and no call record
//     holds (utility calls, stopless true output, compactions) — is booked only
//     at a QUIET point: a result at which no subagent is running and every call
//     the stream named up to it has been booked, with every tail at rest. There,
//     remainder = modelUsage − baseline − Σ counted calls, per model and class,
//     exact and never negative; a negative class alarms and books nothing. A
//     window holding a compact_boundary books it as a compaction (on the turn
//     open at the boundary, else a compaction turn of its own); any other
//     window's as overhead with no turn.
//   - At process exit the last remainder is flushed from the cost-state record
//     CC appends on a graceful close, else from the last result.
//
// Scope is this CC process: lines billed before it launched are a previous
// process's and are skipped, and the remainder baseline starts at what CC
// restored on --resume (#2012).

package ccstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/modelinfo"
)

// ccLine is one assistant line of a CC transcript, as the ledger reads it.
type ccLine struct {
	id, model  string
	at         time.Time
	stopReason string
	complete   bool
	tokens     modelinfo.Tokens
	method     string // how its cache-write TTLs were decided
}

// ccUsageJSON is a per-message usage object with the fields the ledger prices.
type ccUsageJSON struct {
	InputTokens              int                 `json:"input_tokens"`
	OutputTokens             int                 `json:"output_tokens"`
	CacheReadInputTokens     int                 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int                 `json:"cache_creation_input_tokens"`
	CacheCreation            *CacheCreationSplit `json:"cache_creation"`
	ServerToolUse            *struct {
		WebSearchRequests int `json:"web_search_requests"`
		WebFetchRequests  int `json:"web_fetch_requests"`
	} `json:"server_tool_use"`
}

// ccRecord is one transcript record: an assistant message, or the cost-state
// record CC appends at shutdown.
type ccRecord struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Timestamp   string `json:"timestamp"`
	SessionID   string `json:"sessionId"`
	Message     struct {
		ID         string      `json:"id"`
		Model      string      `json:"model"`
		StopReason *string     `json:"stop_reason"`
		Usage      ccUsageJSON `json:"usage"`
	} `json:"message"`
	ModelUsage map[string]ModelUsage `json:"modelUsage"`
}

// parseCCRecord decodes one transcript line: an assistant call (line non-nil),
// or a cost-state record (costState non-nil), or neither.
func parseCCRecord(raw []byte) (line *ccLine, costState *ccRecord) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, nil
	}
	var r ccRecord
	if json.Unmarshal(raw, &r) != nil {
		return nil, nil
	}
	switch {
	case r.Type == "cost-state":
		return nil, &r
	case r.Type != "assistant" || r.Message.ID == "":
		return nil, nil
	}
	at, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
	l := &ccLine{id: r.Message.ID, model: r.Message.Model, at: at.UTC(), complete: r.Message.StopReason != nil}
	if r.Message.StopReason != nil {
		l.stopReason = *r.Message.StopReason
	}
	l.tokens, l.method = ccTokens(r.Message.Usage)
	return l, nil
}

// ccTokens normalises a per-message usage into disjoint ledger classes: the
// cache-write split CC reports is observed; writes the split does not cover
// are TTL-unknown (and would be for a message that reports no split).
func ccTokens(u ccUsageJSON) (modelinfo.Tokens, string) {
	t := modelinfo.Tokens{
		modelinfo.ClassInput:     u.InputTokens,
		modelinfo.ClassOutput:    u.OutputTokens,
		modelinfo.ClassCacheRead: u.CacheReadInputTokens,
	}
	method := accounting.ClassMethodObserved
	if s := u.CacheCreation; s != nil {
		t[modelinfo.ClassCacheWrite5m] = s.Ephemeral5m
		t[modelinfo.ClassCacheWrite1h] = s.Ephemeral1h
		if rest := u.CacheCreationInputTokens - s.Ephemeral5m - s.Ephemeral1h; rest > 0 {
			t[modelinfo.ClassCacheWrite] = rest
			method = accounting.ClassMethodUnknown
		}
	} else if u.CacheCreationInputTokens > 0 {
		t[modelinfo.ClassCacheWrite] = u.CacheCreationInputTokens
		method = accounting.ClassMethodUnknown
	}
	if s := u.ServerToolUse; s != nil {
		t[modelinfo.ClassWebSearch] = s.WebSearchRequests
		t[modelinfo.ClassWebFetch] = s.WebFetchRequests
	}
	return t, method
}

// modelUsageTokens is a cumulative modelUsage in ledger classes. Its cache
// writes carry no TTL.
func modelUsageTokens(m ModelUsage) modelinfo.Tokens {
	return modelinfo.Tokens{
		modelinfo.ClassInput:      m.InputTokens,
		modelinfo.ClassOutput:     m.OutputTokens,
		modelinfo.ClassCacheRead:  m.CacheReadInputTokens,
		modelinfo.ClassCacheWrite: m.CacheCreationInputTokens,
		modelinfo.ClassWebSearch:  m.WebSearchRequests,
	}
}

// remainderClasses folds a counted call's classes onto modelUsage's: every
// cache-write TTL is one cache write, and web fetches are not in modelUsage.
func remainderClasses(t modelinfo.Tokens) modelinfo.Tokens {
	return modelinfo.Tokens{
		modelinfo.ClassInput:      t[modelinfo.ClassInput],
		modelinfo.ClassOutput:     t[modelinfo.ClassOutput],
		modelinfo.ClassCacheRead:  t[modelinfo.ClassCacheRead],
		modelinfo.ClassCacheWrite: t[modelinfo.ClassCacheWrite5m] + t[modelinfo.ClassCacheWrite1h] + t[modelinfo.ClassCacheWrite],
		modelinfo.ClassWebSearch:  t[modelinfo.ClassWebSearch],
	}
}

// namedCall is a main-thread call the stream named in this process.
type namedCall struct {
	window  int
	turn    string
	pending *ccLine // latest line seen without a stop_reason
}

// ccAgent is one subagent seen in this process.
type ccAgent struct {
	turn    string
	window  int
	open    bool    // its tail is running
	pending *ccLine // latest line of an id not yet completed
}

// ccBoundary is one compact_boundary.
type ccBoundary struct {
	at   time.Time
	turn string // the foci turn open at the boundary, or ""
}

// ccResult is a result awaiting its settle.
type ccResult struct {
	window int
	at     time.Time
	mu     map[string]ModelUsage
	quiet  bool // no subagent was running when it arrived
}

// ccBook is the adapter's state for one CC process.
type ccBook struct {
	l   *accounting.Ledger
	lg  *log.ComponentLogger
	now func() time.Time

	session, agentID, sessionFile string
	scope                         string // this process: "<session>@<launch nanos>"
	launch                        time.Time

	window  int
	named   map[string]*namedCall
	held    map[string]*ccLine
	done    map[string]bool // ids booked, or dropped, in this process
	agents  map[string]*ccAgent
	runTurn map[int]string

	counted    map[int]map[string]modelinfo.Tokens // counted bookings per window and model
	boundaries map[int][]ccBoundary
	results    []ccResult
	baseline   map[string]ModelUsage
	lastMU     map[string]ModelUsage
	costState  map[string]ModelUsage
	copies     int // transcript lines dropped as copies
}

// settle bounds: how long a result waits for the main-transcript lines of the
// calls the stream named (P0-b measured them landing at most ~2ms after the
// result; 250ms is over 100x that), and for the subagent tails of a quiet
// point to come to rest (subagentTailSettle).
const ccLineBound = 250 * time.Millisecond

func newCCBook(l *accounting.Ledger, lg *log.ComponentLogger, session, agentID string, launch time.Time, baseline map[string]ModelUsage) *ccBook {
	if session == "" {
		session = accounting.NoSession
	}
	c := &ccBook{
		l: l, lg: lg, now: time.Now,
		session: session, agentID: agentID, launch: launch.UTC(),
		scope: fmt.Sprintf("%s@%d", session, launch.UnixNano()),
		named: map[string]*namedCall{}, held: map[string]*ccLine{}, done: map[string]bool{},
		agents: map[string]*ccAgent{}, runTurn: map[int]string{},
		counted: map[int]map[string]modelinfo.Tokens{}, boundaries: map[int][]ccBoundary{},
		baseline: maps.Clone(baseline),
	}
	if c.baseline == nil {
		c.baseline = map[string]ModelUsage{}
	}
	// The scope's first report is the totals CC starts this process from, so
	// the process's own cost is always a difference of two reports.
	for _, m := range slices.Sorted(maps.Keys(c.baseline)) {
		c.report(m, c.baseline[m], c.launch)
	}
	return c
}

// streamNamed records a main-thread call the stream delivered, on the turn
// open now. A line already read for it is booked at once.
func (c *ccBook) streamNamed(id, turn string, at time.Time) {
	if id == "" || c.done[id] || c.named[id] != nil {
		return
	}
	if turn == "" {
		turn = c.runTurnFor(c.window, at)
	}
	c.named[id] = &namedCall{window: c.window, turn: turn}
	if l := c.held[id]; l != nil {
		delete(c.held, id)
		c.mainLine(l)
	}
}

// runTurnFor is the turn a run no foci turn opened (an autonomous CC run) is
// booked on: one per window.
func (c *ccBook) runTurnFor(window int, at time.Time) string {
	if t, ok := c.runTurn[window]; ok {
		return t
	}
	t := accounting.MintTurnID(c.session, "run", at)
	c.runTurn[window] = t
	return t
}

// mainLine handles one assistant line of the main transcript.
func (c *ccBook) mainLine(l *ccLine) {
	if c.done[l.id] {
		c.copies++ // a re-appended copy of a call this process already booked
		return
	}
	n := c.named[l.id]
	if n == nil {
		c.held[l.id] = l // not named (yet): a history copy unless the stream names it
		return
	}
	if !l.complete {
		n.pending = l
		return
	}
	c.bookCall(l, n.turn, "", n.window, accounting.FinalityCompleted)
	delete(c.named, l.id)
}

// subLine handles one assistant line of a subagent's transcript.
func (c *ccBook) subLine(agent, turn string, l *ccLine) {
	if !l.at.IsZero() && l.at.Before(c.launch) {
		return // billed by an earlier process: not this scope
	}
	a := c.agent(agent, turn, l.at)
	if c.done[l.id] {
		return
	}
	// An agent's calls are sequential: a new id finalises the one before it.
	if p := a.pending; p != nil && p.id != l.id {
		c.bookCall(p, a.turn, agent, a.window, accounting.FinalityStopless)
		a.pending = nil
	}
	if l.complete {
		c.bookCall(l, a.turn, agent, a.window, accounting.FinalityCompleted)
		a.pending = nil
		return
	}
	a.pending = l
}

// agent is agent's state, created at first sight on the turn open then (the
// turn that spawned it, #1924) and in the current window.
func (c *ccBook) agent(agent, turn string, at time.Time) *ccAgent {
	a := c.agents[agent]
	if a == nil {
		if turn == "" {
			turn = c.runTurnFor(c.window, at)
		}
		a = &ccAgent{turn: turn, window: c.window}
		c.agents[agent] = a
	}
	return a
}

// tailOpened and tailClosed bracket a subagent's transcript tail. A closed
// tail's last uncompleted call is final at its last-seen counts.
func (c *ccBook) tailOpened(agent, turn string) {
	c.agent(agent, turn, c.now()).open = true
}

func (c *ccBook) tailClosed(agent string) {
	a := c.agents[agent]
	if a == nil {
		return
	}
	a.open = false
	if p := a.pending; p != nil {
		c.bookCall(p, a.turn, agent, a.window, accounting.FinalityStopless)
		a.pending = nil
	}
}

// compactBoundary records a compaction in the current window.
func (c *ccBook) compactBoundary(turn string, at time.Time) {
	c.boundaries[c.window] = append(c.boundaries[c.window], ccBoundary{at: at.UTC(), turn: turn})
}

// result records one result: CC's cumulative modelUsage as reports, and the
// window it closes, which settles once its calls are in.
func (c *ccBook) result(mu map[string]ModelUsage, running int, at time.Time) {
	at = at.UTC()
	for _, m := range slices.Sorted(maps.Keys(mu)) {
		c.report(m, mu[m], at)
	}
	c.lastMU = maps.Clone(mu)
	c.results = append(c.results, ccResult{window: c.window, at: at, mu: maps.Clone(mu), quiet: running == 0})
	c.window++
}

// costStateSeen records the cost-state record this process appended at a
// graceful close (it arrives past the launch offset, so it is this process's).
func (c *ccBook) costStateSeen(mu map[string]ModelUsage, at time.Time) {
	c.costState = maps.Clone(mu)
	for _, m := range slices.Sorted(maps.Keys(mu)) {
		c.report(m, mu[m], at.UTC())
	}
}

func (c *ccBook) report(model string, m ModelUsage, at time.Time) {
	if c.l == nil {
		return
	}
	cost := m.CostUSD
	err := c.l.Update(func(tx *accounting.Tx) error {
		return tx.Report(accounting.Report{
			Backend: accounting.BackendCCStream, Session: c.session, ScopeKey: c.scope, Model: model,
			Grain: accounting.GrainCumulative, At: at, CostUSD: &cost, Tokens: modelUsageTokens(m),
		})
	})
	if err != nil {
		c.lg.Warnf("ledger: report %s: %v", model, err)
	}
}

// settle settles every result whose calls are in, oldest first, or whose bound
// has passed; force settles them all (process exit). A quiet result whose
// calls and tails all came in books the remainder; one that hit its bound is
// abandoned, and its remainder carries to the next quiet point.
func (c *ccBook) settle(force bool) {
	now := c.now()
	for len(c.results) > 0 {
		r := c.results[0]
		named, tails := c.outstanding(r.window)
		bound := ccLineBound
		if r.quiet && tails > 0 {
			bound = subagentTailSettle
		}
		ready := named == 0 && (!r.quiet || tails == 0)
		if !ready && !force && now.Before(r.at.Add(bound)) {
			return
		}
		c.results = c.results[1:]
		c.closeWindows(r.window)
		switch {
		case r.quiet && ready:
			c.remainder(r.window, r.mu, fmt.Sprintf("q%d", r.window), r.at)
		case r.quiet:
			c.lg.Infof("ledger: result %d not settled within its bound (named=%d tails=%d); its remainder carries forward",
				r.window, named, tails)
		}
	}
}

// outstanding counts, for windows up to w, the named calls not yet booked (an
// interrupted one waiting at its stopless line counts as in) and the subagent
// tails still open.
func (c *ccBook) outstanding(w int) (named, tails int) {
	for _, n := range c.named {
		if n.window <= w && n.pending == nil {
			named++
		}
	}
	for _, a := range c.agents {
		if a.window <= w && a.open {
			tails++
		}
	}
	return named, tails
}

// closeWindows finalises the main-thread calls of windows up to w: a call seen
// only at a stopless line was interrupted, and is booked — priced — from it; a
// call whose line never came alarms. Held lines no stream named are copies.
func (c *ccBook) closeWindows(w int) {
	for _, id := range slices.Sorted(maps.Keys(c.named)) {
		n := c.named[id]
		if n.window > w {
			continue
		}
		delete(c.named, id)
		if n.pending != nil {
			c.bookCall(n.pending, n.turn, "", n.window, accounting.FinalityInterrupted)
			continue
		}
		c.done[id] = true
		c.l.Alarm(accounting.Alarm{Invariant: accounting.InvStreamIdBooked, Backend: accounting.BackendCCStream,
			Detail: fmt.Sprintf("session %s: the stream named call %s but its main-transcript line never came", c.session, id)})
	}
	for id := range c.held {
		c.done[id] = true
		c.copies++
	}
	clear(c.held)
}

// bookCall books one call and, unless CC counts it nowhere (interrupted),
// adds it to its window's counted sums.
func (c *ccBook) bookCall(l *ccLine, turn, actor string, window int, finality string) {
	c.done[l.id] = true
	b, err := c.l.RecordCall(accounting.Turn{
		TurnID: turn, Session: c.session, AgentID: c.agentID, Backend: accounting.BackendCCStream,
		Source: accounting.SourceUser, StartedAt: l.at,
	}, accounting.Call{
		Key: l.id, Backend: accounting.BackendCCStream, Provider: "anthropic", Model: l.model,
		Session: c.session, AgentID: c.agentID, TurnID: turn, Actor: actor,
		Kind: accounting.KindCall, Finality: finality, ClassMethod: l.method,
		BilledAt: c.billedAt(l.at), Tokens: l.tokens, StopReason: l.stopReason, SessionFile: c.sessionFile,
		Detail: map[string]any{"scope": c.scope, "window": window},
	}, nil)
	if err != nil {
		c.lg.Warnf("ledger: book %s: %v", l.id, err)
		return
	}
	if b.Duplicate || finality == accounting.FinalityInterrupted {
		return
	}
	if c.counted[window] == nil {
		c.counted[window] = map[string]modelinfo.Tokens{}
	}
	sum := c.counted[window][l.model]
	if sum == nil {
		sum = modelinfo.Tokens{}
		c.counted[window][l.model] = sum
	}
	for class, n := range remainderClasses(l.tokens) {
		sum[class] += n
	}
}

func (c *ccBook) billedAt(at time.Time) time.Time {
	if at.IsZero() {
		return c.now().UTC()
	}
	return at
}

// remainder books what mu counts beyond the baseline and every counted call of
// windows up to w: per model, exact, never negative. A negative class means
// the booked set is wrong: it alarms, books nothing, and leaves the windows to
// the next quiet point.
func (c *ccBook) remainder(w int, mu map[string]ModelUsage, label string, at time.Time) {
	counted := map[string]modelinfo.Tokens{}
	for win, byModel := range c.counted {
		if win > w {
			continue
		}
		for m, t := range byModel {
			if counted[m] == nil {
				counted[m] = modelinfo.Tokens{}
			}
			for class, n := range t {
				counted[m][class] += n
			}
		}
	}
	models := map[string]bool{}
	for m := range mu {
		models[m] = true
	}
	for m := range counted {
		models[m] = true
	}
	rem := map[string]modelinfo.Tokens{}
	var negative []string
	for _, m := range slices.Sorted(maps.Keys(models)) {
		cur, base := modelUsageTokens(mu[m]), modelUsageTokens(c.baseline[m])
		r := modelinfo.Tokens{}
		for _, class := range slices.Sorted(maps.Keys(cur)) {
			v := cur[class] - base[class] - counted[m][class]
			if v < 0 {
				negative = append(negative, fmt.Sprintf("%s %s %d (modelUsage %d, baseline %d, booked %d)",
					m, class, v, cur[class], base[class], counted[m][class]))
			}
			r[class] = v
		}
		rem[m] = r
	}
	if len(negative) > 0 {
		c.l.Alarm(accounting.Alarm{Invariant: accounting.InvNegativeRemainder, Backend: accounting.BackendCCStream,
			Detail: fmt.Sprintf("session %s %s: %s — nothing booked", c.session, label, strings.Join(negative, "; "))})
		return
	}
	var bounds []ccBoundary
	for win := range c.boundaries {
		if win <= w {
			bounds = append(bounds, c.boundaries[win]...)
		}
	}
	slices.SortFunc(bounds, func(a, b ccBoundary) int { return a.at.Compare(b.at) })
	for _, m := range slices.Sorted(maps.Keys(rem)) {
		r := rem[m]
		if !billed(r) {
			continue
		}
		call := accounting.Call{
			Backend: accounting.BackendCCStream, Provider: "anthropic", Model: m, Session: c.session,
			AgentID: c.agentID, Finality: accounting.FinalityDerived, ClassMethod: accounting.ClassMethodUnknown,
			BilledAt: at, Tokens: r, Detail: map[string]any{"scope": c.scope, "through_window": w},
		}
		var turn accounting.Turn
		if len(bounds) > 0 {
			first := bounds[0]
			call.Kind = accounting.KindCompaction
			call.Key = fmt.Sprintf("compact:%s:%d:%s", c.session, first.at.UnixNano(), m)
			call.Detail["compactions"] = len(bounds)
			turn = accounting.Turn{TurnID: first.turn, Session: c.session, AgentID: c.agentID,
				Backend: accounting.BackendCCStream, Source: accounting.SourceUser, StartedAt: first.at}
			if turn.TurnID == "" {
				// An idle /compact, between turns: a compaction turn of its own.
				turn.TurnID = accounting.MintTurnID(c.session, accounting.KindCompaction, first.at)
				turn.Source = accounting.SourceCompaction
				turn.EndedAt, turn.ActivityClosedAt = at, at
			}
			call.TurnID = turn.TurnID
		} else {
			call.Kind = accounting.KindOverhead
			call.Key = fmt.Sprintf("%s:%s:%s", c.scope, label, m)
		}
		if _, err := c.l.RecordCall(turn, call, nil); err != nil {
			c.lg.Warnf("ledger: book remainder %s: %v", call.Key, err)
		}
	}
	for m, u := range mu {
		c.baseline[m] = u
	}
	for win := range c.counted {
		if win <= w {
			delete(c.counted, win)
		}
	}
	for win := range c.boundaries {
		if win <= w {
			delete(c.boundaries, win)
		}
	}
}

// billed reports whether any class of t is non-zero.
func billed(t modelinfo.Tokens) bool {
	for _, n := range t {
		if n != 0 {
			return true
		}
	}
	return false
}

// exit flushes the process: every open tail's last call, every result, and the
// last remainder — from the cost-state record CC appended at a graceful close,
// else from the last result, which can only understate.
func (c *ccBook) exit() {
	for _, agent := range slices.Sorted(maps.Keys(c.agents)) {
		c.tailClosed(agent)
	}
	c.settle(true)
	c.closeWindows(c.window)
	final, label := c.costState, "exit"
	if final == nil {
		final, label = c.lastMU, "exit-last-result"
	}
	if final != nil {
		c.remainder(c.window, final, label, c.now().UTC())
	}
	if c.copies > 0 {
		c.lg.Infof("ledger: %d transcript line(s) dropped as copies of calls not made by this process (F1b)", c.copies)
	}
}
