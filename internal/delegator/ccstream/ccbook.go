// ccbook.go — the Claude Code adapter of the cost ledger (#2111 §3.1, §4, §5),
// as a single-threaded state machine: this backend's only cost path. ccledger.go
// feeds it events from the stream, the main-transcript tail and the subagent
// tails, in the order they happened, and runs its settle clock.
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
//     they are in the remainder subtraction. A Workflow run's agents are
//     subagents of the turn that invoked the Workflow (#2130): the run is
//     opened as an agent on that turn, and each of its agents under it.
//   - The REMAINDER — what CC's cumulative modelUsage counts and no call record
//     holds (utility calls, stopless true output, compactions) — is booked only
//     at a QUIET point: a result at which no subagent is running and every call
//     the stream named up to it has been booked, with every tail at rest. There,
//     remainder = modelUsage − baseline − Σ counted calls, per model and class,
//     exact and never negative; a negative class alarms and books nothing. A
//     window holding a compact_boundary books it as a compaction (on the turn
//     open at the boundary, else a compaction turn of its own); any other
//     window's as overhead with no turn. modelUsage reports no cache-write
//     TTL, so the remainder's writes are split 5m/1h by solving CC's own cost
//     for the interval (solveRemainderTTL, #2130); one that will not solve
//     stays TTL-unknown, priced at 1h, and alarms.
//   - A MODEL REFUSAL FALLBACK (#2200): the refused attempt is in no
//     transcript, so its spend is in the remainder on the refused model. A
//     window holding one books that model's remainder as the refused call, on
//     the turn open at the refusal, priced by Anthropic's refusal billing
//     (refusalBilled): billed at its counts when it refused mid-stream or in a
//     billed category, else booked with no counts at $0 — and its CC figure is
//     kept off CC's side of the divergence check, as CC's own cost over-counts
//     it. Either way it is a known cause, logged at INFO and never an alarm.
//     A mid-stream refusal also RETRACTS the message CC had already streamed
//     (#2255): the book matches the retracted frame uuids against the named
//     calls' (ccBook.retract), and a retracted call is not waited for — not
//     by a result's settle, not by a turn's flush, not by its turn's
//     activity — and at its window's close it is dropped with no alarm and
//     no interrupted booking, because CC's modelUsage counts the refused
//     attempt and the refusal remainder already holds its spend. A FINAL
//     line that lands anyway still books it, completed, at its counts.
//   - At process exit the last remainder is flushed from the cost-state record
//     CC appends on a graceful close, else from the last result.
//   - CHECKS (#2111 §8), once the booked set is what CC counted — at each
//     quiet point's remainder and at exit: per model, CC's own cost since
//     launch against the ledger's price of the process's counted calls and
//     remainders (invCostDivergence, interrupted calls on neither side); and
//     an overhead remainder larger than both $0.50 and 2% of its window's
//     cost (invOverheadBounded).
//   - A turn's ACTIVITY (R8) closes once it has ended and nothing of it is
//     still spending — no subagent tail of it open, no named call of it
//     unbooked; a subagent resumed on it reopens it.
//   - A call books on its turn by the turn's id alone: the agent layer recorded
//     a foci turn's facts when it began. A run no foci turn opened (an
//     autonomous CC run) books on a turn of its own, recorded here as
//     autonomous.
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
	"math"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"foci/internal/delegator"
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

// ReportClasses folds a counted call's classes onto modelUsage's — the
// classes of this adapter's backend reports: every cache-write TTL is one
// cache write, and web fetches are not in modelUsage.
func ReportClasses(t modelinfo.Tokens) modelinfo.Tokens {
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
	window int
	turn   string
	uuids  []string // the stream frames' uuids (one per content block), for retract matching
	// retracted: a refusal fallback retracted this call (#2255) — not waited
	// for from here on, and dropped at its window's close.
	retracted bool
	pending   *ccLine // latest line seen without a stop_reason
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

// ccRefusal is one model_refusal_fallback: CC's primary model refused and CC
// retried on a fallback model (#2200). The refused attempt has no transcript
// record, so this is all the book learns of it. A mid-stream refusal also
// retracts the message CC had already streamed: retracted holds its frame
// uuids, which ccBook.retract matches against the named calls (#2255).
type ccRefusal struct {
	at        time.Time
	turn      string   // the foci turn open at the refusal, or ""
	model     string   // the model that refused
	fallback  string   // the model CC retried on
	category  string   // the API's refusal category, "" when it gave none
	requestID string   // the refused request's id, "" when CC gave none
	retracted []string // frame uuids CC retracted: output had already streamed
}

// refusalBilledCategories are the refusal categories Anthropic bills when the
// refusal arrives before any output (platform.claude.com/docs/en/build-with-
// claude/refusals-and-fallback, "How refusals are billed", as of 2026-09). A
// pre-output refusal in any other category, or none, is not billed. The list
// may change as Anthropic refines its safeguards.
var refusalBilledCategories = map[string]bool{"bio": true, "frontier_llm": true, "reasoning_extraction": true}

// refusalBilled reports whether a refused attempt is billed: it refused
// mid-stream (output had streamed — CC retracted a message, or the attempt
// counts output tokens) or its category is billed before any output.
func refusalBilled(r ccRefusal, t modelinfo.Tokens) bool {
	return len(r.retracted) > 0 || t[modelinfo.ClassOutput] > 0 || refusalBilledCategories[r.category]
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

	counted    map[int]map[string]*countedSum // counted bookings per window and model
	boundaries map[int][]ccBoundary
	refusals   map[int][]ccRefusal // model refusal fallbacks per window
	results    []ccResult
	baseline   map[string]ModelUsage
	lastMU     map[string]ModelUsage
	costState  map[string]ModelUsage
	copies     int // transcript lines dropped as copies

	// The main-transcript tail, when one runs: a call's line is only known
	// missing once a read that began after its result's bound has finished,
	// so a result is never abandoned on the clock alone (#2134).
	tailing  bool
	readFrom time.Time // when the latest finished read began

	// The divergence check's two sides for this process (#2111 §8.1): the
	// totals CC started from, and the ledger's price of everything CC counts
	// that this process booked — counted calls and remainders — per model,
	// over the windows settled at quiet points so far. interrupted is the
	// price of the calls CC counts nowhere, shown beside.
	launchBase  map[string]ModelUsage
	procBooked  map[string]*countedSum
	interrupted map[string]float64
	// unbilled is CC's figure for the refused attempts not billed (#2200),
	// per model: CC's own cost counts them, the ledger books them at $0, so
	// the divergence check takes them off CC's side.
	unbilled map[string]float64

	// Turn activity (R8): the turns that have ended (a foci turn's idle, a
	// run turn's result), and those whose activity this book has closed —
	// ended, with no subagent tail of theirs open and no named call of theirs
	// unbooked.
	ended  map[string]bool
	closed map[string]bool

	// version is the CC version this process reported at init, carried on
	// every alarm the book raises (#2149). Atomic rather than read from the
	// Backend: turnEnded can run the book outside the adapter goroutine.
	version atomic.Pointer[string]
}

// setVersion records the CC version this process reported at init.
func (c *ccBook) setVersion(v string) {
	if v != "" {
		c.version.Store(&v)
	}
}

// alarm raises an invariant violation on this process, tagged with its CC
// version.
func (c *ccBook) alarm(inv, detail string) {
	c.raise(accounting.Alarm{Invariant: inv, Detail: detail})
}

// raise raises a, stamped with this process's backend and CC version.
func (c *ccBook) raise(a accounting.Alarm) {
	a.Backend = accounting.BackendCCStream
	if v := c.version.Load(); v != nil {
		a.Version = *v
	}
	c.l.Alarm(a)
}

// countedSum is what the counted calls of one window on one model add up to:
// their tokens in report classes, and their cost as the ledger priced them
// (unpriced when any of them has no price).
type countedSum struct {
	tokens   modelinfo.Tokens
	cost     float64
	unpriced bool
}

func (s *countedSum) add(o *countedSum) {
	for class, n := range o.tokens {
		s.tokens[class] += n
	}
	s.cost += o.cost
	s.unpriced = s.unpriced || o.unpriced
}

// settle bounds: how long a result waits for the main-transcript lines of the
// calls the stream named (P0-b measured them landing at most ~2ms after the
// result; 250ms is over 100x that), and for the subagent tails of a quiet
// point to come to rest (subagentTailSettle). Both bound when a line can land
// on disk; with a main tail running, settle also waits for it to read past
// the bound (#2134).
const ccLineBound = 250 * time.Millisecond

func newCCBook(l *accounting.Ledger, lg *log.ComponentLogger, session, agentID string, launch time.Time, baseline map[string]ModelUsage) *ccBook {
	if session == "" {
		session = accounting.NoSession
	}
	c := &ccBook{
		l: l, lg: lg, now: time.Now,
		session: session, agentID: agentID, launch: launch.UTC(),
		scope: fmt.Sprintf("%s@%d", session, launch.UnixNano()), // ScopeLaunch parses it
		named: map[string]*namedCall{}, held: map[string]*ccLine{}, done: map[string]bool{},
		agents: map[string]*ccAgent{}, runTurn: map[int]string{},
		counted: map[int]map[string]*countedSum{}, boundaries: map[int][]ccBoundary{}, refusals: map[int][]ccRefusal{},
		baseline:   maps.Clone(baseline),
		procBooked: map[string]*countedSum{}, interrupted: map[string]float64{}, unbilled: map[string]float64{},
		ended: map[string]bool{}, closed: map[string]bool{},
	}
	if c.baseline == nil {
		c.baseline = map[string]ModelUsage{}
	}
	c.launchBase = maps.Clone(c.baseline)
	// The scope's first report is the totals CC starts this process from, so
	// the process's own cost is always a difference of two reports.
	for _, m := range slices.Sorted(maps.Keys(c.baseline)) {
		c.report(m, c.baseline[m], c.launch)
	}
	return c
}

// ScopeLaunch is the launch time a process scope ("<session>@<launch nanos>")
// names: the time of its baseline reports, when CC restored any.
func ScopeLaunch(scope string) (time.Time, bool) {
	i := strings.LastIndexByte(scope, '@')
	if i < 0 {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(scope[i+1:], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, n).UTC(), true
}

// streamNamed records a main-thread call the stream delivered, on the turn
// open now, collecting the frame's uuid: CC streams one assistant event per
// content block, each with its own uuid, all sharing the message id, and a
// refusal fallback's retraction names those uuids (ccBook.retract, #2255). A
// line already read for it is booked at once.
func (c *ccBook) streamNamed(id, uuid, turn string, at time.Time) {
	if id == "" || c.done[id] {
		return
	}
	if n := c.named[id]; n != nil {
		if uuid != "" && !slices.Contains(n.uuids, uuid) {
			n.uuids = append(n.uuids, uuid)
		}
		return
	}
	if turn == "" {
		turn = c.runTurnFor(c.window, at)
	}
	var uuids []string
	if uuid != "" {
		uuids = []string{uuid}
	}
	c.named[id] = &namedCall{window: c.window, turn: turn, uuids: uuids}
	if l := c.held[id]; l != nil {
		delete(c.held, id)
		c.mainLine(l)
	}
}

// retract marks every named call whose frame uuids share a member with uuids
// as retracted by a refusal fallback (#2255): from here on the book does not
// wait for the call — its spend books with the refused attempt. Unknown uuids
// (a tool result's, a subagent frame's) match nothing; a second call changes
// nothing; a call already booked is no longer named and is not touched. A
// turn whose last named call this retracts can close its activity now.
func (c *ccBook) retract(uuids []string) {
	if len(uuids) == 0 {
		return
	}
	var turns []string
	for _, n := range c.named {
		if n.retracted || !slices.ContainsFunc(n.uuids, func(u string) bool { return slices.Contains(uuids, u) }) {
			continue
		}
		n.retracted = true
		turns = append(turns, n.turn)
	}
	for _, t := range turns {
		c.closeIfIdle(t, c.now())
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

// turnSource is the source a call's turn is recorded with if the ledger does
// not hold it yet: a run turn of this adapter's is autonomous; any other was
// opened by foci, whose agent layer recorded it (with its real source) first,
// so this value only stands when that record is missing.
func (c *ccBook) turnSource(turn string) string {
	for _, t := range c.runTurn {
		if t == turn {
			return accounting.SourceAutonomous
		}
	}
	return accounting.SourceUser
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
	c.closeIfIdle(n.turn, c.now())
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
	a := c.agent(agent, turn, c.now())
	a.open = true
	c.reopenTurn(a.turn)
}

// tailOpenedUnder opens agent's tail as a child of parent, an agent already
// seen: a Workflow run's agents book on the run's turn, and a run a subagent
// invoked on that subagent's (#2130). A child found after a result is still
// counted in its parent's window: the parent was open from that window on,
// so no quiet point since has booked a remainder its calls are in. With no
// known parent it is tailOpened.
func (c *ccBook) tailOpenedUnder(agent, parent, turn string) {
	p := c.agents[parent]
	if parent == "" || p == nil {
		c.tailOpened(agent, turn)
		return
	}
	a := c.agents[agent]
	if a == nil {
		a = &ccAgent{turn: p.turn, window: p.window}
		c.agents[agent] = a
	}
	a.open = true
	c.reopenTurn(a.turn)
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
	c.closeIfIdle(a.turn, c.now())
}

// turnEnded records that turn has ended: a foci turn at its idle, a run turn
// at the result that ends its run. Its activity closes now, or once the last
// of its subagents' tails closes and its named calls are booked.
func (c *ccBook) turnEnded(turn string, at time.Time) {
	if turn == "" {
		return
	}
	c.ended[turn] = true
	c.closeIfIdle(turn, at)
}

// closeIfIdle closes turn's activity at at if it has ended and nothing of it
// is still spending: no subagent tail of it open, no call the stream named on
// it unbooked — a retracted one's spend books with the refused attempt
// (#2255), so it does not hold the turn. A background subagent that outlives
// its turn keeps the turn running until its tail closes (R8).
func (c *ccBook) closeIfIdle(turn string, at time.Time) {
	if turn == "" || !c.ended[turn] || c.closed[turn] {
		return
	}
	for _, a := range c.agents {
		if a.open && a.turn == turn {
			return
		}
	}
	for _, n := range c.named {
		if n.turn == turn && !n.retracted {
			return
		}
	}
	c.setActivity(turn, at)
}

// reopenTurn marks turn running again when spend resumes on it after its
// activity closed: a subagent resumed (SendMessage) books on the turn that
// spawned it.
func (c *ccBook) reopenTurn(turn string) {
	if !c.closed[turn] {
		return
	}
	if err := c.l.SetTurnActivity(turn, time.Time{}); err != nil {
		c.lg.Warnf("ledger: reopen turn %s: %v", turn, err)
		return
	}
	c.closed[turn] = false
}

// setActivity stores turn's activity close at at, and remembers it is closed.
func (c *ccBook) setActivity(turn string, at time.Time) {
	if err := c.l.SetTurnActivity(turn, at.UTC()); err != nil {
		c.lg.Warnf("ledger: close turn %s activity: %v", turn, err)
		return
	}
	c.closed[turn] = true
}

// mainTailRunning records whether a main-transcript tail is feeding the
// book: while one is, settle waits for its reads.
func (c *ccBook) mainTailRunning(on bool) { c.tailing = on }

// mainRead records a finished main-transcript read that began at start; every
// line it found was handed to the book before it.
func (c *ccBook) mainRead(start time.Time) {
	if start.After(c.readFrom) {
		c.readFrom = start
	}
}

// compactBoundary records a compaction in the current window.
func (c *ccBook) compactBoundary(turn string, at time.Time) {
	c.boundaries[c.window] = append(c.boundaries[c.window], ccBoundary{at: at.UTC(), turn: turn})
}

// refusalFallback records a model refusal fallback in the current window: the
// refused attempt's spend is in this window's remainder on r.model.
func (c *ccBook) refusalFallback(r ccRefusal) {
	r.at = r.at.UTC()
	c.refusals[c.window] = append(c.refusals[c.window], r)
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
	// A run no foci turn opened ends with its result.
	if t, ok := c.runTurn[c.window]; ok {
		c.turnEnded(t, at)
	}
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
		// Unready past its bound is only final once the main tail has read
		// the transcript as it stood after the bound: the tail polls on its
		// own clock, so the bound alone can pass before a landed line is read.
		due := r.at.Add(bound)
		if !ready && !force && (now.Before(due) || c.tailing && c.readFrom.Before(due)) {
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
// tails still open. A call a refusal fallback retracted is not waited for
// (#2255): its spend books with the refused attempt.
func (c *ccBook) outstanding(w int) (named, tails int) {
	for _, n := range c.named {
		if n.window <= w && n.pending == nil && !n.retracted {
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

// unseenNamed counts the main-thread calls the stream named whose transcript
// line has not been read yet — what a turn's flush waits for. A retracted
// call's line may never come (#2255), so it is not waited for.
func (c *ccBook) unseenNamed() int {
	n := 0
	for _, nc := range c.named {
		if nc.pending == nil && !nc.retracted {
			n++
		}
	}
	return n
}

// closeWindows finalises the main-thread calls of windows up to w: a call seen
// only at a stopless line was interrupted, and is booked — priced — from it; a
// call whose line never came alarms. A retracted call's line may never come
// (#2255): it is dropped with neither, because CC's modelUsage counts the
// refused attempt and the refusal remainder already holds its spend. Held
// lines no stream named are copies.
func (c *ccBook) closeWindows(w int) {
	var turns []string
	for _, id := range slices.Sorted(maps.Keys(c.named)) {
		n := c.named[id]
		if n.window > w {
			continue
		}
		delete(c.named, id)
		turns = append(turns, n.turn)
		if n.retracted {
			// No alarm, and no interrupted booking either — even with a
			// stopless pending line, whose spend the refusal remainder holds.
			// The interrupted rule is for calls CC counts nowhere. A later
			// line for the id is a copy.
			c.done[id] = true
			c.lg.Infof("ledger: session %s: call %s was retracted by a refusal fallback; its spend books with the refused attempt",
				c.session, id)
			continue
		}
		if n.pending != nil {
			c.bookCall(n.pending, n.turn, "", n.window, accounting.FinalityInterrupted)
			continue
		}
		c.done[id] = true
		c.alarm(accounting.InvStreamIdBooked, fmt.Sprintf("session %s: the stream named call %s but its main-transcript line never came", c.session, id))
	}
	for _, t := range turns {
		c.closeIfIdle(t, c.now())
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
		Source: c.turnSource(turn), StartedAt: c.billedAt(l.at),
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
	if b.Duplicate {
		return
	}
	// Priced as the call_costs view prices it: its counts at its billing time.
	usd, priced := modelinfo.CostAsOf(l.model, c.billedAt(l.at), l.tokens)
	if finality == accounting.FinalityInterrupted {
		c.interrupted[l.model] += usd
		return
	}
	if c.counted[window] == nil {
		c.counted[window] = map[string]*countedSum{}
	}
	sum := c.counted[window][l.model]
	if sum == nil {
		sum = &countedSum{tokens: modelinfo.Tokens{}}
		c.counted[window][l.model] = sum
	}
	sum.add(&countedSum{tokens: ReportClasses(l.tokens), cost: usd, unpriced: !priced})
}

// addProcBooked adds one booking CC counts to the process's running total.
func (c *ccBook) addProcBooked(model string, s *countedSum) {
	p := c.procBooked[model]
	if p == nil {
		p = &countedSum{tokens: modelinfo.Tokens{}}
		c.procBooked[model] = p
	}
	p.add(s)
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
	counted := map[string]*countedSum{}
	for win, byModel := range c.counted {
		if win > w {
			continue
		}
		for m, t := range byModel {
			if counted[m] == nil {
				counted[m] = &countedSum{tokens: modelinfo.Tokens{}}
			}
			counted[m].add(t)
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
		booked := modelinfo.Tokens{}
		if counted[m] != nil {
			booked = counted[m].tokens
		}
		r := modelinfo.Tokens{}
		for _, class := range slices.Sorted(maps.Keys(cur)) {
			v := cur[class] - base[class] - booked[class]
			if v < 0 {
				negative = append(negative, fmt.Sprintf("%s %s %d (modelUsage %d, baseline %d, booked %d)",
					m, class, v, cur[class], base[class], booked[class]))
			}
			r[class] = v
		}
		rem[m] = r
	}
	if len(negative) > 0 {
		c.alarm(accounting.InvNegativeRemainder, fmt.Sprintf("session %s %s: %s — nothing booked", c.session, label, strings.Join(negative, "; ")))
		return
	}
	// The windows' calls are now settled against CC's count: they join the
	// process total the divergence check compares (a later window's calls,
	// already booked, wait for their own quiet point).
	for m, s := range counted {
		c.addProcBooked(m, s)
	}
	var bounds []ccBoundary
	for win := range c.boundaries {
		if win <= w {
			bounds = append(bounds, c.boundaries[win]...)
		}
	}
	slices.SortFunc(bounds, func(a, b ccBoundary) int { return a.at.Compare(b.at) })
	refused := map[string][]ccRefusal{}
	for win, rs := range c.refusals {
		if win <= w {
			for _, r := range rs {
				refused[r.model] = append(refused[r.model], r)
			}
		}
	}
	var overhead float64
	var overheadModels []string
	outputOnly := true
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
		if rs := refused[m]; len(rs) > 0 {
			c.bookRefusal(rs, call, mu[m].CostUSD-c.baseline[m].CostUSD, counted[m], w, label)
			continue
		}
		if r[modelinfo.ClassCacheWrite] > 0 {
			c.splitRemainderTTL(&call, mu[m].CostUSD-c.baseline[m].CostUSD, counted[m], label)
		}
		var turn accounting.Turn
		if len(bounds) > 0 {
			first := bounds[0]
			call.Kind = accounting.KindCompaction
			call.Key = fmt.Sprintf("compact:%s:%d:%s", c.session, first.at.UnixNano(), m)
			call.Detail["compactions"] = len(bounds)
			turn = accounting.Turn{TurnID: first.turn, Session: c.session, AgentID: c.agentID,
				Backend: accounting.BackendCCStream, Source: c.turnSource(first.turn), StartedAt: first.at}
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
		b, err := c.l.RecordCall(turn, call, nil)
		if err != nil {
			c.lg.Warnf("ledger: book remainder %s: %v", call.Key, err)
			continue
		}
		if b.Duplicate {
			continue
		}
		usd, priced := modelinfo.CostAsOfPrompt(m, at, call.Tokens, call.PricingPrompt())
		c.addProcBooked(m, &countedSum{tokens: ReportClasses(call.Tokens), cost: usd, unpriced: !priced})
		if call.Kind == accounting.KindOverhead {
			overhead += usd
			overheadModels = append(overheadModels, fmt.Sprintf("%s $%.4f (%s)", m, usd, formatTokens(call.Tokens)))
			outputOnly = outputOnly && onlyOutput(call.Tokens)
		} else if c.closed[turn.TurnID] {
			// Spend booked on a turn whose activity had closed moves its
			// close to this booking.
			c.setActivity(turn.TurnID, at)
		}
	}
	if len(overheadModels) > 0 {
		c.checkOverhead(overhead, windowCost(mu, c.baseline), overheadModels, outputOnly, label)
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
	for win := range c.refusals {
		if win <= w {
			delete(c.refusals, win)
		}
	}
	c.checkDivergence(mu, label)
}

// bookRefusal books a remainder on a model that refused in its windows as the
// refused call (#2200), on the turn open at the first refusal. call is the
// remainder as remainder built it; ccCost is CC's cost of the interval on the
// model and counted its counted calls there. Any other uncounted spend on the
// refused model in the same windows rides with it: the session's model moved
// to the fallback at the refusal, so there is rarely any.
func (c *ccBook) bookRefusal(rs []ccRefusal, call accounting.Call, ccCost float64, counted *countedSum, w int, label string) {
	r := rs[0]
	refusedTokens := maps.Clone(call.Tokens)
	billed := refusalBilled(r, refusedTokens)
	for _, o := range rs[1:] {
		billed = billed || refusalBilled(o, refusedTokens)
	}
	ccFigure := ccCost
	if counted != nil {
		ccFigure -= counted.cost
	}
	call.Kind = accounting.KindCall
	call.StopReason = "refusal"
	call.Key = "refusal:" + r.requestID
	if r.requestID == "" {
		call.Key = fmt.Sprintf("refusal:%s:%d:%s", c.scope, r.at.UnixNano(), call.Model)
	}
	turn := r.turn
	if turn == "" {
		turn = c.runTurnFor(w, r.at)
	}
	call.TurnID = turn
	call.Detail["refusal_category"] = r.category
	call.Detail["fallback_model"] = r.fallback
	call.Detail["refusal_billed"] = billed
	call.Detail["cc_cost_usd"] = ccFigure
	if len(rs) > 1 {
		call.Detail["refusals"] = len(rs)
	}
	if billed {
		if call.Tokens[modelinfo.ClassCacheWrite] > 0 {
			c.splitRemainderTTL(&call, ccCost, counted, label)
		}
	} else {
		// Not billed: booked with no counts, so it prices at $0; the counts
		// CC reported are kept for forensics.
		call.Detail["refused_tokens"] = formatTokens(refusedTokens)
		call.Tokens = modelinfo.Tokens{}
	}
	b, err := c.l.RecordCall(accounting.Turn{TurnID: turn, Session: c.session, AgentID: c.agentID,
		Backend: accounting.BackendCCStream, Source: c.turnSource(turn), StartedAt: r.at}, call, nil)
	if err != nil {
		c.lg.Warnf("ledger: book refusal %s: %v", call.Key, err)
		return
	}
	if b.Duplicate {
		return
	}
	if billed {
		usd, priced := modelinfo.CostAsOfPrompt(call.Model, call.BilledAt, call.Tokens, call.PricingPrompt())
		c.addProcBooked(call.Model, &countedSum{tokens: ReportClasses(call.Tokens), cost: usd, unpriced: !priced})
		c.lg.Infof("ledger: session %s %s: %s refused (category %q, mid-stream or a billed category) and CC fell back to %s; the refused attempt is booked on turn %s at $%.4f (%s)",
			c.session, label, call.Model, r.category, r.fallback, turn, usd, formatTokens(call.Tokens))
	} else {
		c.unbilled[call.Model] += ccFigure
		c.lg.Infof("ledger: session %s %s: %s refused before any output (category %q, not billed) and CC fell back to %s; the refused attempt is booked on turn %s at $0 — CC's figure $%.4f (%s) is not billed",
			c.session, label, call.Model, r.category, r.fallback, turn, ccFigure, formatTokens(refusedTokens))
	}
	if c.closed[turn] {
		c.setActivity(turn, call.BilledAt)
	}
}

// The overhead bound (#2111 §8.2, invOverheadBounded): a remainder booked as
// overhead may be up to ccOverheadBoundUSD, or ccOverheadBoundShare of CC's
// cost for its window, whichever is larger. Overhead is CC's utility calls and
// stopless calls' true output — measured at ~0.5% of spend (#2110) — so more
// than both means spend the ledger has no source for.
const (
	ccOverheadBoundUSD   = 0.50
	ccOverheadBoundShare = 0.02
)

// ccDivergenceFloorUSD: below this on both sides a process is too cheap for
// its divergence to carry signal (as delegator's per-turn check).
const ccDivergenceFloorUSD = 0.01

// windowCost is CC's own cost from base to mu, over every model.
func windowCost(mu, base map[string]ModelUsage) float64 {
	cost := 0.0
	for m, u := range mu {
		cost += u.CostUSD - base[m].CostUSD
	}
	return cost
}

// ccBugMissingFinalRecord is the CC bug behind output-only overhead: a
// subagent transcript missing its final record, whose output CC still counts
// (#2160, #2175).
const ccBugMissingFinalRecord = "https://github.com/anthropics/claude-code/issues/84223"

// checkOverhead alarms when one remainder's overhead is beyond the bound.
// Overhead that is output tokens only has a known cause, CC bug #84223, so it
// alarms at WARN and names it (Dick, 2026-10-05 on #2160); any other class in
// it is spend with no known source, an ERROR.
func (c *ccBook) checkOverhead(overhead, window float64, models []string, outputOnly bool, label string) {
	bound := max(ccOverheadBoundUSD, ccOverheadBoundShare*window)
	if overhead <= bound {
		return
	}
	cause := "spend no transcript holds (a subagent tail that never opened, a lost resume baseline?)"
	if outputOnly {
		cause = "output-only overhead: subagent transcripts missing their final record, CC bug " + ccBugMissingFinalRecord
	}
	c.raise(accounting.Alarm{Invariant: accounting.InvOverheadBounded, Warn: outputOnly,
		Detail: fmt.Sprintf("session %s %s: overhead $%.4f is over the bound $%.2f (CC's cost for the window $%.4f): %s — %s",
			c.session, label, overhead, bound, window, strings.Join(models, "; "), cause)})
}

// onlyOutput reports whether output is the only class t bills.
func onlyOutput(t modelinfo.Tokens) bool {
	for class, n := range t {
		if n != 0 && class != modelinfo.ClassOutput {
			return false
		}
	}
	return true
}

// checkDivergence compares, per model, CC's own cost since this process
// launched with the ledger's price of everything booked in the process that CC
// counts (#2111 §8.1). Run once the booked set is exactly what CC counted: at
// a quiet point's remainder and at exit. Interrupted calls are in neither
// side (CC counts them nowhere) and are shown beside; an unbilled refused
// attempt (#2200) is taken off CC's side, which counts it though it was not
// billed, and shown beside too. A model with an
// unpriced booking is skipped: invClassNoRate / invModelNotInTable already
// alarmed on it.
func (c *ccBook) checkDivergence(mu map[string]ModelUsage, label string) {
	var off []string
	models := map[string]bool{}
	for m := range mu {
		models[m] = true
	}
	for m := range c.procBooked {
		models[m] = true
	}
	for _, m := range slices.Sorted(maps.Keys(models)) {
		cc := mu[m].CostUSD - c.launchBase[m].CostUSD - c.unbilled[m]
		booked := c.procBooked[m]
		if booked == nil {
			booked = &countedSum{tokens: modelinfo.Tokens{}}
		}
		if booked.unpriced || (cc < ccDivergenceFloorUSD && booked.cost < ccDivergenceFloorUSD) {
			continue
		}
		diff := math.Abs(booked.cost - cc)
		if cc > 0 && diff/cc <= delegator.CostDivergenceTolerance {
			continue
		}
		pct := "n/a"
		if cc > 0 {
			pct = fmt.Sprintf("%.1f%%", 100*diff/cc)
		}
		off = append(off, fmt.Sprintf("%s: ledger $%.4f vs CC $%.4f (%s off; interrupted $%.4f and unbilled refusals $%.4f excluded; booked %s)",
			m, booked.cost, cc, pct, c.interrupted[m], c.unbilled[m], formatTokens(booked.tokens)))
	}
	if len(off) == 0 {
		return
	}
	c.alarm(accounting.InvCostDivergence, fmt.Sprintf("session %s %s: the ledger's price of this process differs from CC's own cost beyond %.0f%%: %s",
		c.session, label, 100*delegator.CostDivergenceTolerance, strings.Join(off, "; ")))
}

// formatTokens renders counts as "class=n, ..." in class order, zeros left out.
func formatTokens(t modelinfo.Tokens) string {
	var parts []string
	for _, class := range slices.Sorted(maps.Keys(t)) {
		if t[class] != 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", class, t[class]))
		}
	}
	return strings.Join(parts, ", ")
}

// splitRemainderTTL gives a remainder's TTL-unknown cache writes their 5m/1h
// split, solved from CC's own cost of the interval on the call's model
// (ccCost: modelUsage costUSD less the baseline's) less the counted calls'
// cost. It books the solved split, or on failure leaves the writes unknown —
// priced at the 1h rate — and alarms, never silently.
func (c *ccBook) splitRemainderTTL(call *accounting.Call, ccCost float64, counted *countedSum, label string) {
	w := call.Tokens[modelinfo.ClassCacheWrite]
	x5m, err := solveRemainderTTL(call.Model, call.BilledAt, call.Tokens, ccCost, counted)
	if err != nil {
		call.Detail["ttl"] = "unsolved"
		c.alarm(accounting.InvRemainderTTLUnsolved, fmt.Sprintf("session %s %s %s: %d remainder cache writes priced at the 1h rate: %v",
			c.session, label, call.Model, w, err))
		return
	}
	delete(call.Tokens, modelinfo.ClassCacheWrite)
	call.Tokens[modelinfo.ClassCacheWrite5m] += x5m
	call.Tokens[modelinfo.ClassCacheWrite1h] += w - x5m
	call.ClassMethod = accounting.ClassMethodSolved
	call.Detail["ttl"] = "solved"
}

// solveRemainderTTL returns how many of r's TTL-unknown cache writes were 5m.
// Every other class of r is priced from its counts, so with the remainder's
// cost known the 5m count x is the one unknown:
//
//	x = (r priced with every unknown write at 1h − its cost) / (rate_1h − rate_5m)
//
// where its cost is ccCost less the counted calls' (the migration's solve,
// migrate.go solveWindow, on a live interval). As there, x is accepted only
// as an integer, to accounting.SolveTolerance, in [0, writes]: modelinfo's
// rates and CC's agree exactly (#2131), so a split that does not reproduce
// CC's cost exactly means the interval's inputs are wrong, not the rates.
func solveRemainderTTL(model string, at time.Time, r modelinfo.Tokens, ccCost float64, counted *countedSum) (int, error) {
	w := r[modelinfo.ClassCacheWrite]
	countedCost := 0.0
	if counted != nil {
		if counted.unpriced {
			return 0, fmt.Errorf("a counted call on %s is unpriced, so the remainder's cost is unknown", model)
		}
		countedCost = counted.cost
	}
	// The remainder is a sum of calls: base rates, never a prompt-size tier.
	rm, ok := modelinfo.ResolveRateModel(model, at, 0)
	if !ok {
		return 0, fmt.Errorf("%s is not in the price table", model)
	}
	rates, _ := modelinfo.RatesAsOf(rm, at)
	r5m, has5m := rates[modelinfo.ClassCacheWrite5m]
	r1h, has1h := rates[modelinfo.ClassCacheWrite1h]
	if !has5m || !has1h || r1h <= r5m {
		return 0, fmt.Errorf("%s has no distinct 5m and 1h write rates", model)
	}
	all1h, priced := modelinfo.CostAsOfPrompt(model, at, r, 0)
	if !priced {
		return 0, fmt.Errorf("the remainder on %s is unpriced", model)
	}
	perToken := (r1h - r5m) / 1_000_000
	x := (all1h - (ccCost - countedCost)) / perToken
	n := math.Round(x)
	if math.Abs(x-n) > accounting.SolveTolerance || n < 0 || n > float64(w) {
		return 0, fmt.Errorf("CC's cost $%.9f (counted calls $%.9f) implies %.6f 5m writes of %d, not an exact split",
			ccCost, countedCost, x, w)
	}
	return int(n), nil
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
	// A run whose result never came ends with its process. A foci turn still
	// open ends when the agent layer completes it (ccLedger.turnEnded).
	for _, t := range c.runTurn {
		c.turnEnded(t, c.now())
	}
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
