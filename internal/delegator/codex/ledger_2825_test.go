package codex

import (
	"testing"

	"foci/internal/delegator/accounting"
	"foci/internal/sqlite"
)

// compactedItem is codex's item/completed for a finished compaction on the
// session's own thread.
const compactedItem = `{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_c","item":{"type":"contextCompaction","id":"cc_1"}}}`

// zeroedAfterCompaction is the thread/tokenUsage/updated codex sends right
// after a compaction: every class of last is 0 and only last.totalTokens
// carries the post-compaction size (real capture shape, work Mac rollout
// 2026-11-03T09:48:25Z: last_token_usage all 0 except total_tokens 34387).
const zeroedAfterCompaction = `{"method":"thread/tokenUsage/updated","params":{"threadId":"th_1","turnId":"tu_c","tokenUsage":{"last":{"inputTokens":0,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":0,"reasoningOutputTokens":0,"totalTokens":34387},"total":{"inputTokens":15509,"cachedInputTokens":11264,"cacheWriteInputTokens":0,"outputTokens":64,"reasoningOutputTokens":0,"totalTokens":15573},"modelContextWindow":258400}}}`

// TestCodexCompactionIsBookedAsCompactionTurn (#2825): a codex compaction is
// booked as a source='compaction' turn, so CurrentContextFill stops reporting
// the pre-compaction fill. Before the fix codex booked nothing for it (the
// post-compaction usage report is all zeros and bookCycle skips it), and the
// stale fill stayed "current" forever.
func TestCodexCompactionIsBookedAsCompactionTurn(t *testing.T) {
	b, _, _, path := ledgerFacade(t)
	b.dispatch(fixtureLines(t)[0]) // one real cycle on the open turn: a fill
	l := accounting.Live()
	fill, err := l.CurrentContextFill("test/session")
	if err != nil || fill == 0 {
		t.Fatalf("premise: fill before compaction = %d (%v), want > 0", fill, err)
	}

	// The turn ends; foci's /compact then runs with no foci turn open.
	b.turnMu.Lock()
	b.turnActive = false
	b.turnMu.Unlock()
	b.dispatch([]byte(compactedItem))
	b.dispatch([]byte(zeroedAfterCompaction))

	if fill, err := l.CurrentContextFill("test/session"); err != nil || fill != 0 {
		t.Errorf("fill after compaction = %d (%v), want 0: the pre-compaction size is stale", fill, err)
	}
	db, err := sqlite.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	var closed bool
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(activity_closed_at IS NOT NULL), 0) FROM turns
		WHERE session = 'test/session' AND source = 'compaction' AND backend = 'codex'`).Scan(&n, &closed); err != nil {
		t.Fatal(err)
	}
	if n != 1 || !closed {
		t.Errorf("compaction turns = %d (closed %v), want exactly one closed codex compaction turn", n, closed)
	}
}
