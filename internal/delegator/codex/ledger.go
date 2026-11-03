// ledger.go — the codex adapter of the cost ledger (#2111 P2).
//
// codex reports usage once per API cycle (thread/tokenUsage/updated), with the
// cycle's own figures in `last` and the thread's running sum in `total`, and
// no message id and no cost anywhere (#2112 P0-c): codex is token-only. Each
// notification is one call, keyed by its thread and the running total after it
// — unique and monotonic per thread, and the same key if codex replays the
// notification, which the ledger then books once. The running total itself is
// stored as a cumulative backend report, and the conservation check (total
// grows by exactly last, checkTokenUsage) runs on every notification, a child
// thread's included.
//
// Tested against a synthetic fixture built from the documented schema plus
// the #1855 live captures (testdata/tokenusage_synthetic.json). Codex is
// disabled; replace the fixture with a real capture on re-enable.

package codex

import (
	"fmt"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
)

// cycleTokens normalises one tokenUsage breakdown into disjoint ledger
// classes. cachedInputTokens is a SUBSET of inputTokens (checked live,
// invCachedIsSubsetOfInput), so it is taken out of input; cacheWriteInputTokens
// is a cache write with no reported TTL. reasoningOutputTokens is INSIDE
// outputTokens for OpenAI models, so it is not a class of its own — it goes to
// the call's detail only.
func cycleTokens(u tokenUsageBreakdown) modelinfo.Tokens {
	input := u.InputTokens - u.CachedInputTokens
	if input < 0 {
		input = 0
	}
	return modelinfo.Tokens{
		modelinfo.ClassInput:      input,
		modelinfo.ClassCacheRead:  u.CachedInputTokens,
		modelinfo.ClassCacheWrite: u.CacheWriteInputTokens,
		modelinfo.ClassOutput:     u.OutputTokens,
	}
}

// codexBreakdownKeys is every key of a codex TokenUsageBreakdown foci knows,
// and codexUsageKeys every key of the tokenUsage object around two of them
// (invUnseenField, #2111 §8.6).
var (
	codexBreakdownKeys = accounting.UsageKeys{
		"inputTokens": nil, "outputTokens": nil, "cachedInputTokens": nil,
		"cacheWriteInputTokens": nil, "reasoningOutputTokens": nil, "totalTokens": nil,
	}
	codexUsageKeys = accounting.UsageKeys{
		"last": codexBreakdownKeys, "total": codexBreakdownKeys, "modelContextWindow": nil,
	}
)

// bookCycle books one API cycle as a call. actor is "" for the session's own
// thread, else the subagent the child thread belongs to; spawnTurn is that
// subagent's spawning turn ("" for the session's own thread).
func (b *Backend) bookCycle(p *tokenUsageParams, actor, spawnTurn string) {
	last, total := p.TokenUsage.Last, p.TokenUsage.Total
	if last.InputTokens == 0 && last.OutputTokens == 0 && last.CacheWriteInputTokens == 0 {
		return // a notification that reports no cycle
	}
	at := time.Now().UTC()
	key := fmt.Sprintf("%s:%d", p.ThreadID, total.TotalTokens)
	if total.TotalTokens == 0 {
		// No running total to key by: unique by arrival instead.
		key = fmt.Sprintf("%s:@%d", p.ThreadID, at.UnixNano())
	}
	b.mu.Lock()
	model := b.model
	b.mu.Unlock()
	if model != "" {
		model = "codex/" + model
	}
	turn := b.cycleTurn(spawnTurn, at)
	detail := map[string]any{"codex_thread": p.ThreadID}
	if p.TurnID != "" {
		detail["codex_turn"] = p.TurnID
	}
	if last.ReasoningOutputTokens > 0 {
		detail["reasoning_output_tokens"] = last.ReasoningOutputTokens
	}
	accounting.Live().AlarmUnseen(accounting.BackendCodex, b.version(), "codex",
		fmt.Sprintf("thread %s cycle %s", p.ThreadID, key), accounting.UnseenUsageKeys(p.rawUsage, codexUsageKeys))
	if _, err := accounting.Live().RecordCall(turn, accounting.Call{
		Key: key, Backend: accounting.BackendCodex, Provider: "openai", Model: model,
		Session: turn.Session, AgentID: b.agentID, TurnID: turn.TurnID, Actor: actor,
		Kind: accounting.KindCall, Finality: accounting.FinalityCompleted, ClassMethod: accounting.ClassMethodUnknown,
		BilledAt: at, Tokens: cycleTokens(last), Detail: detail,
	}, &accounting.Report{
		Backend: accounting.BackendCodex, Session: turn.Session, ScopeKey: p.ThreadID, Model: model,
		Grain: accounting.GrainCumulative, At: at, Tokens: cycleTokens(total),
	}); err != nil {
		b.logWarnf("book tokenUsage cycle %s: %v", key, err)
	}
}

// openTurnID is the foci turn open on this facade, or "".
func (b *Backend) openTurnID() string {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	if b.turnActive && b.turnEvents != nil {
		return b.turnEvents.TurnID
	}
	return ""
}

// cycleTurn is the turn a cycle is booked on: a subagent's spawning turn, else
// the foci turn open now, else — a cycle codex ran with no foci turn open —
// an autonomous turn of the cycle's own, closed at once. The agent layer
// recorded an open turn's facts at its start; naming it here with only its id
// never overwrites them.
func (b *Backend) cycleTurn(spawnTurn string, at time.Time) accounting.Turn {
	session := b.startOpts.SessionKey
	if session == "" {
		session = accounting.NoSession
	}
	t := accounting.Turn{
		TurnID: spawnTurn, Session: session, AgentID: b.agentID, Backend: accounting.BackendCodex,
		Source: accounting.SourceAutonomous, StartedAt: at,
	}
	if t.TurnID == "" {
		t.TurnID = b.openTurnID()
	}
	if t.TurnID == "" {
		t.TurnID = accounting.MintTurnID(session, accounting.KindCall, at)
		t.EndedAt, t.ActivityClosedAt = at, at
	}
	return t
}

// bookCompaction books a finished compaction of the session's own thread as a
// compaction turn of its own (source compaction), closed at once (#2825). It
// is the row the ledger's staleness guard matches on: without it,
// CurrentContextFill kept the pre-compaction fill as current forever, because
// codex sends no usage of its own for the compaction (the report right after
// it has every class of `last` at 0, which bookCycle skips).
//
// The turn carries no call: codex does not report what the compaction call
// spent over the app-server. It is booked at completion even when a foci turn
// is open (codex's own mid-turn auto-compaction), so that turn's fill reads as
// unknown (0) until the next turn, never as the pre-compaction size.
func (b *Backend) bookCompaction(at time.Time) {
	session := b.startOpts.SessionKey
	if session == "" {
		session = accounting.NoSession
	}
	if err := accounting.RecordTurn(accounting.Turn{
		TurnID: accounting.MintTurnID(session, accounting.KindCompaction, at), Session: session,
		AgentID: b.agentID, Backend: accounting.BackendCodex, Source: accounting.SourceCompaction,
		StartedAt: at, EndedAt: at, ActivityClosedAt: at,
	}); err != nil {
		b.logWarnf("book compaction turn: %v", err)
	}
}
