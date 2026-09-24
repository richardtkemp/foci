package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/delegator"
	"foci/internal/log"
)

// Live checks on the codex token-usage semantics foci's per-turn accounting
// rests on (#2013). See delegator/expectations.go for delivery and rate
// limiting.

const expectBackend = "codex"

// Invariant names; they are the rate-limit keys, so keep them stable.
const (
	// tokenUsage.last is ONE API cycle's figures and tokenUsage.total is the
	// thread's running sum of every last (#1855). foci sums last per turn, so
	// if last ever becomes a per-turn or per-thread running figure, every turn
	// is overcounted. Checked as: the growth of total between two consecutive
	// notifications on one thread equals the later notification's last.
	invTotalIsSumOfLast = "tokenUsage.total grows by exactly tokenUsage.last"
	// cachedInputTokens is a SUBSET of inputTokens (live-verified on 0.144.5:
	// totalTokens == inputTokens + outputTokens). onTokenUsage subtracts it
	// out of input; if codex made it additive, that subtraction would erase
	// real input tokens and undercount every turn.
	invCachedIsSubsetOfInput = "cachedInputTokens is included in inputTokens"
)

// noteInitializeVersion records the app-server's version from its initialize
// response. codex reports it only inside userAgent, as
// "<clientName>/<version> (<os>; <arch>) ..." — verified 2026-09-24 against
// codex-cli 0.145.0: "probe/0.145.0 (Linux Mint 22.0.0; x86_64) unknown (probe; 0)".
func (b *Backend) noteInitializeVersion(raw json.RawMessage) {
	var resp struct {
		UserAgent string `json:"userAgent"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return
	}
	v := versionFromUserAgent(resp.UserAgent)
	if v == "" {
		b.logWarnf("codex initialize: no version in userAgent %q — backend-expectation reports will say 'version unknown' (#2013)", resp.UserAgent)
		return
	}
	owner := b.process()
	owner.mu.Lock()
	owner.codexVersion = v
	owner.mu.Unlock()
	b.expectations().NoteVersion(b.lg, expectBackend, v)
}

// versionFromUserAgent extracts the version from codex's userAgent string: the
// text between the first '/' and the next space.
func versionFromUserAgent(ua string) string {
	_, rest, ok := strings.Cut(ua, "/")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, " ")
	return v
}

// version is the app-server's version, or "" if initialize did not report one.
func (b *Backend) version() string {
	owner := b.process()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.codexVersion
}

// expectations returns the guard this facade reports to.
func (b *Backend) expectations() *delegator.ExpectationGuard {
	if b.expect != nil {
		return b.expect
	}
	return delegator.Expectations
}

func (b *Backend) violated(invariant, detail string) {
	lg := b.lg
	if lg == nil {
		lg = log.NewComponentLogger("codex")
	}
	b.expectations().Violated(lg, expectBackend, b.version(), invariant, detail)
}

// checkTokenUsage runs both token-usage checks on one notification. It takes
// turnMu for the per-thread history and reports after releasing it.
func (b *Backend) checkTokenUsage(p *tokenUsageParams) {
	last, total := p.TokenUsage.Last, p.TokenUsage.Total

	var subsetProblem string
	if in, out := last.InputTokens, last.OutputTokens; last.TotalTokens > 0 && in+out > 0 &&
		(last.TotalTokens != in+out || last.CachedInputTokens > in) {
		subsetProblem = fmt.Sprintf("last input=%d cached=%d output=%d total=%d: expected total == input+output and cached <= input; "+
			"foci subtracts cached from input, so input tokens are being undercounted",
			in, last.CachedInputTokens, out, last.TotalTokens)
	}

	var sumProblem string
	if p.ThreadID != "" {
		b.turnMu.Lock()
		prev, seen := b.threadTotal[p.ThreadID]
		if b.threadTotal == nil {
			b.threadTotal = make(map[string]tokenUsageBreakdown)
		}
		b.threadTotal[p.ThreadID] = total
		b.turnMu.Unlock()
		sumProblem = totalGrowthMismatch(prev, total, last, seen)
	}

	if subsetProblem != "" {
		b.violated(invCachedIsSubsetOfInput, fmt.Sprintf("thread %s turn %s: %s", p.ThreadID, p.TurnID, subsetProblem))
	}
	if sumProblem != "" {
		b.violated(invTotalIsSumOfLast, fmt.Sprintf("thread %s turn %s: %s; foci sums last per turn, so this turn's "+
			"token counts and cost are likely wrong", p.ThreadID, p.TurnID, sumProblem))
	}
}

// totalGrowthMismatch describes how total's growth since prev disagrees with
// last, or returns "" when it agrees or cannot be judged.
//
// Not judged: the first notification on a thread (a resumed thread's total
// carries its whole history, and there is no earlier one to diff), a total
// that went DOWN (a reset — foci never reads total, so a reset corrupts
// nothing), and an exact repeat of the previous total, which codex's own
// rollouts show occasionally (1 in ~930 on this host) and which is a
// duplicate delivery rather than a change of meaning.
func totalGrowthMismatch(prev, cur, last tokenUsageBreakdown, seen bool) string {
	if !seen {
		return ""
	}
	dIn := cur.InputTokens - prev.InputTokens
	dCached := cur.CachedInputTokens - prev.CachedInputTokens
	dOut := cur.OutputTokens - prev.OutputTokens
	if dIn < 0 || dCached < 0 || dOut < 0 {
		return ""
	}
	if dIn == 0 && dCached == 0 && dOut == 0 {
		return ""
	}
	if dIn == last.InputTokens && dCached == last.CachedInputTokens && dOut == last.OutputTokens {
		return ""
	}
	return fmt.Sprintf("total grew by input=%d cached=%d output=%d but last reports input=%d cached=%d output=%d",
		dIn, dCached, dOut, last.InputTokens, last.CachedInputTokens, last.OutputTokens)
}
