package ccstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Cost and token accounting for CC turns (#1674).
//
// The one fact everything here exists for: EVERY counter in CC's per-result
// ModelUsage map is CUMULATIVE over the life of the CC PROCESS — cost, output
// tokens, cache reads, cache writes. Storing any of them as a per-turn figure
// over-counts by roughly the turn index, which compounds to ~quadratic when
// rows are summed (13x measured across 28 Jul - 4 Aug 2026: $32,566 reported
// against ~$2,500 real).
//
// Probe-verified 2026-08-05, one CC process fed four turns over
// --input-format stream-json, four identical trivial prompts:
//
//	turn | costUSD  | outputTokens | cacheReadInputTokens
//	  1  | 0.0099234|      56      |   21,624
//	  2  | 0.0141382|     105      |   46,722
//	  3  | 0.0170425|     141      |   72,545
//	  4  | 0.0199124|     174      |   98,434
//
// cacheRead climbing in near-equal ~25k steps for a trivial turn is the tell:
// a running sum, not a growing context.
//
// NOTE FOR ANYONE RE-VERIFYING THIS: `claude -p --resume` spawns a FRESH
// PROCESS PER TURN, so a per-process counter cannot accumulate and the probe
// cannot distinguish cumulative from per-turn. It must be ONE process fed
// multiple turns.

// modelUsageDelta converts CC's cumulative ModelUsage snapshot into this
// cycle's own figures by subtracting the previous snapshot for the same model,
// then records the new snapshot. Caller must hold b.mu.
//
// The previous snapshot lives on the Backend because a Backend's lifetime IS
// the CC process's. What the counters hold when that process STARTS depends on
// how it was launched: a fresh session starts them at zero, but since CC
// 2.1.280 a --resume (with or without --fork-session) restores the totals CC
// last persisted for that session, so they start at the whole conversation's
// history (#2012). Start seeds the map with those restored totals via
// resumeBaseline; see there. Before 2.1.280 a resume restarted at zero
// (probe-verified 2026-08-05: 0.0243 -> 0.0035), and the empty map that a
// fresh Backend started with was the right baseline for free.
//
// Each field is guarded independently rather than trusting one to speak for
// the rest: a counter that has gone DOWN means the process restarted beneath a
// reused Backend, and the current value is then the whole delta. Guarding only
// (say) cost would let a mixed snapshot yield a negative token count.
func (b *Backend) modelUsageDelta(model string, cur ModelUsage) ModelUsage {
	prev, seen := b.lastModelUsage[model]
	if b.lastModelUsage == nil {
		b.lastModelUsage = make(map[string]ModelUsage)
	}
	b.lastModelUsage[model] = cur
	if !seen {
		return cur
	}

	sub := func(now, before int) int {
		if now < before {
			return now
		}
		return now - before
	}

	d := cur
	d.InputTokens = sub(cur.InputTokens, prev.InputTokens)
	d.OutputTokens = sub(cur.OutputTokens, prev.OutputTokens)
	d.CacheReadInputTokens = sub(cur.CacheReadInputTokens, prev.CacheReadInputTokens)
	d.CacheCreationInputTokens = sub(cur.CacheCreationInputTokens, prev.CacheCreationInputTokens)
	d.WebSearchRequests = sub(cur.WebSearchRequests, prev.WebSearchRequests)
	if cur.CostUSD < prev.CostUSD {
		d.CostUSD = cur.CostUSD
	} else {
		d.CostUSD = cur.CostUSD - prev.CostUSD
	}
	return d
}

// pricedSpanStart records that model's snapshot was just taken at `now`, and
// returns when the PREVIOUS one was — i.e. the start of the window the delta
// being computed right now actually covers. A zero time means this is the first
// snapshot for that model, so the window has no measured start.
//
// Deliberately separate from modelUsageDelta rather than folded into its return
// value: the delta's arithmetic is covered by nine tests that pin exact token
// counts, and widening its signature would have edited all of them to prove
// nothing about the timestamps. Caller must hold b.mu, the same lock
// modelUsageDelta requires, and must call this BEFORE (or with) that call so
// the two agree on which snapshot they are describing.
func (b *Backend) pricedSpanStart(model string, now time.Time) time.Time {
	prev := b.lastModelUsageAt[model]
	if b.lastModelUsageAt == nil {
		b.lastModelUsageAt = make(map[string]time.Time)
	}
	b.lastModelUsageAt[model] = now
	return prev
}

// turnElapsed is time since `from`, or zero when `from` is unset. A zero return
// means NOT MEASURED, never "no time passed" — callers must not print it as a
// duration.
func turnElapsed(from time.Time) time.Duration {
	if from.IsZero() {
		return 0
	}
	return time.Since(from)
}

// ccTranscriptPath is where CC keeps the transcript of sessionID for a process
// run in workDir: ~/.claude/projects/<slug>/<sessionID>.jsonl.
func ccTranscriptPath(workDir, sessionID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ccProjectsDir, projectSlug(workDir), sessionID+".jsonl"), nil
}

// costStateEntry is the transcript record CC writes as it shuts down (type
// "cost-state"), holding the process's cumulative totals. Only the fields the
// baseline needs are decoded.
type costStateEntry struct {
	Type       string                `json:"type"`
	SessionID  string                `json:"sessionId"`
	ModelUsage map[string]ModelUsage `json:"modelUsage"`
}

// resumeBaseline returns the per-model totals a CC process resuming sessionID
// will START from, read from the transcript at path: the modelUsage of the
// LAST "cost-state" record for that session (#2012).
//
// Since CC 2.1.280, a --resume restores that record into the new process, so
// the first result's modelUsage is the conversation's whole history plus the
// turn. Probe-verified 2026-09-24 on 2.1.280 (haiku): t1 in its own process
// ended at cacheRead 13,691; a NEW process resuming it reported 36,457 for t2,
// whose own usage was 22,766. Without a baseline foci priced that first turn
// at the whole history. Live, a keepalive fork was booked $111 against about
// $0.87 of real work.
//
// Reading the same record CC reads, rather than inferring the baseline from the
// first result (modelUsage minus result.usage), is what makes this exact for
// every model at once. result.usage covers only the main loop's model, and
// CC's restored map also holds each subagent model and the models of earlier
// processes. foci's own forks (ForkSession) copy the parent's cost-state
// records with the session id rewritten, and CC restores those too, so the
// same read covers them.
//
// Caveat: CC writes the record at shutdown. A process killed without one
// leaves the previous record in place. CC restores that one too, so reading
// the last record still matches what CC restored. No record means CC starts at
// zero, and so does the returned nil map.
//
// Must run BEFORE the process starts. The new process appends its own record
// when it exits.
func resumeBaseline(path, sessionID string) (map[string]ModelUsage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	marker := []byte(`"cost-state"`)
	var last map[string]ModelUsage
	r := bufio.NewReader(f)
	for {
		line, rerr := r.ReadBytes('\n')
		// Cheap filter first: transcripts reach tens of MB and only a few
		// records per process are cost-state. The decode then rejects text
		// that merely mentions the word, and a torn trailing record.
		if bytes.Contains(line, marker) {
			var e costStateEntry
			if json.Unmarshal(line, &e) == nil && e.Type == "cost-state" && e.SessionID == sessionID {
				last = e.ModelUsage
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return last, nil
			}
			return last, fmt.Errorf("read %s: %w", path, rerr)
		}
	}
}

// resumeBaselineFor is resumeBaseline for a Start: nil for a fresh session
// (CC's counters start at zero). A resume whose transcript cannot be read is
// logged and also gets nil. That is the pre-#2012 behaviour, so the first turn
// is over-priced rather than the launch failing.
func (b *Backend) resumeBaselineFor(workDir, sessionID string) map[string]ModelUsage {
	if sessionID == "" {
		return nil
	}
	path, err := ccTranscriptPath(workDir, sessionID)
	if err == nil {
		var base map[string]ModelUsage
		if base, err = resumeBaseline(path, sessionID); err == nil {
			var cost float64
			for _, u := range base {
				cost += u.CostUSD
			}
			b.logger().Infof("resume baseline: %d model(s), $%.4f restored by CC for %s (#2012)",
				len(base), cost, sessionID)
			return base
		}
	}
	b.logger().Warnf("resume baseline for %s unreadable: %v — its first turn will be priced at the whole conversation's history (#2012)",
		sessionID, err)
	return nil
}
