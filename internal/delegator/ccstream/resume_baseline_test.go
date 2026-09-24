package ccstream

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/delegator"
)

// #2012: since CC 2.1.280 a --resume restores the session's last persisted
// usage totals into the NEW process, so its first result's modelUsage carries
// the whole conversation's history. These pin the baseline foci seeds from the
// same record CC restores.

// The real records from the 2026-09-24 probe (CC 2.1.280, haiku): t1 ran in
// its own process and wrote costStateT1 on exit. t2 resumed it in a new
// process and reported resultT2. Its own per-message usage was in 10,
// cacheRead 22,766, cacheWrite 237, out 33.
const probeSID = "d73108ac-6abc-4ab5-addf-1d98c7d6c1de"

const costStateT1 = `{"type":"cost-state","sessionId":"` + probeSID + `","totalCostUSD":0.0205041,"totalAPIDuration":2744,"modelUsage":{"claude-haiku-4-5":{"inputTokens":10,"outputTokens":195,"thinkingTokens":76,"cacheReadInputTokens":13691,"cacheCreationInputTokens":9075,"webSearchRequests":0,"costUSD":0.0205041}},"hasUnknownModelCost":false}`

var resultT2 = ModelUsage{InputTokens: 20, OutputTokens: 228, CacheReadInputTokens: 36457, CacheCreationInputTokens: 9312, CostUSD: 0.0234297}

func writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var body []byte
	for _, l := range lines {
		body = append(body, l...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResumeBaseline_LastRecordForTheSessionWins(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeTranscript(t, path,
		`{"type":"cost-state","sessionId":"`+probeSID+`","modelUsage":{"m":{"cacheReadInputTokens":1}}}`,
		// Text that merely mentions the record type must not be read as one.
		`{"type":"user","sessionId":"`+probeSID+`","message":{"content":"the \"cost-state\" record"}}`,
		`{"type":"cost-state","sessionId":"`+probeSID+`","modelUsage":{"m":{"cacheReadInputTokens":2}}}`,
		// Another session's record is not what CC restores for this one.
		`{"type":"cost-state","sessionId":"other","modelUsage":{"m":{"cacheReadInputTokens":99}}}`,
	)
	// A torn trailing record, as when the file is read mid-append.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"cost-state","sessionId":"` + probeSID + `","modelUsage":{"m":{"cacheRe`)
	_ = f.Close()

	got, err := resumeBaseline(path, probeSID)
	if err != nil {
		t.Fatalf("resumeBaseline: %v", err)
	}
	if got["m"].CacheReadInputTokens != 2 {
		t.Errorf("baseline cacheRead = %d, want 2 (the last complete record for %s)", got["m"].CacheReadInputTokens, probeSID)
	}
}

func TestResumeBaseline_NoRecordMeansZero(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeTranscript(t, path, `{"type":"user","sessionId":"`+probeSID+`"}`)
	got, err := resumeBaseline(path, probeSID)
	if err != nil || got != nil {
		t.Errorf("resumeBaseline = %v, %v; want nil, nil", got, err)
	}
}

// startWithFakeClaude runs Backend.Start, the production path, against a
// binary that exits at once. Only the launch-time seeding is under test.
// seed is what the Backend held before, as if it had run an earlier process.
func startWithFakeClaude(t *testing.T, workDir, resumeID string, seed map[string]ModelUsage) *Backend {
	t.Helper()
	be, err := newFromConfig(map[string]any{"binary": "/bin/true"})
	if err != nil {
		t.Fatalf("newFromConfig: %v", err)
	}
	b := be.(*Backend)
	b.lastModelUsage = seed
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background(), delegator.StartOptions{
		WorkDir: workDir, AgentID: "resumetest", ResumeSessionID: resumeID,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestStart_ResumeSeedsTheRestoredTotals is the #2012 regression. Without the
// seed, t2's delta is the cumulative 36,457 cacheRead, which includes t1's
// history. With it, the delta is t2's own 22,766.
func TestStart_ResumeSeedsTheRestoredTotals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workDir := filepath.Join(home, "work")
	path, err := ccTranscriptPath(workDir, probeSID)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, path, costStateT1)

	b := startWithFakeClaude(t, workDir, probeSID, nil)

	b.mu.Lock()
	d := b.modelUsageDelta("claude-haiku-4-5", resultT2)
	b.mu.Unlock()
	if d.CacheReadInputTokens != 22766 || d.CacheCreationInputTokens != 237 ||
		d.OutputTokens != 33 || d.InputTokens != 10 {
		t.Errorf("t2 delta = in %d cr %d cw %d out %d; want in 10 cr 22766 cw 237 out 33 (t2's own usage)",
			d.InputTokens, d.CacheReadInputTokens, d.CacheCreationInputTokens, d.OutputTokens)
	}
	if want := 0.0234297 - 0.0205041; math.Abs(d.CostUSD-want) > 1e-12 {
		t.Errorf("t2 delta cost = %v, want %v", d.CostUSD, want)
	}
}

// TestStart_FreshSessionStartsAtZero: a fresh session's counters start at zero
// even on a Backend that previously held a resumed process's baseline.
func TestStart_FreshSessionStartsAtZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := startWithFakeClaude(t, filepath.Join(home, "work"), "",
		map[string]ModelUsage{"claude-haiku-4-5": resultT2})
	b.mu.Lock()
	n := len(b.lastModelUsage)
	b.mu.Unlock()
	if n != 0 {
		t.Errorf("fresh Start left %d baseline model(s), want 0", n)
	}
}

// TestOnResult_ResumedForkPricesOnlyItsOwnWork uses the live incident's
// figures: keepalive fork b346d0d4 of clutch's main session, whose transcript
// ends with the cost-state it inherited from its parent and then the one its
// own process wrote. Its first result is the second record. foci booked
// 280,064,651 cacheRead (all three models' history) to the row. Real work was
// 186,683 cacheRead, 104,017 cacheWrite and 237 output, all on the opus model.
func TestOnResult_ResumedForkPricesOnlyItsOwnWork(t *testing.T) {
	t.Parallel()
	b := &Backend{lastModelUsage: map[string]ModelUsage{
		"claude-opus-5":    {OutputTokens: 423621, CacheReadInputTokens: 142605367, CacheCreationInputTokens: 3312978, CostUSD: 57.0665704},
		"claude-sonnet-5":  {OutputTokens: 579266, CacheReadInputTokens: 135985860, CacheCreationInputTokens: 2303142, CostUSD: 38.751947},
		"claude-haiku-4-5": {OutputTokens: 44782, CacheReadInputTokens: 1286741, CacheCreationInputTokens: 114227, CostUSD: 3.99408275},
	}}
	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5":    {OutputTokens: 423858, CacheReadInputTokens: 142792050, CacheCreationInputTokens: 3416995, CostUSD: 57.940799},
		"claude-sonnet-5":  {OutputTokens: 579266, CacheReadInputTokens: 135985860, CacheCreationInputTokens: 2303142, CostUSD: 38.751947},
		"claude-haiku-4-5": {OutputTokens: 44782, CacheReadInputTokens: 1286741, CacheCreationInputTokens: 114227, CostUSD: 3.99408275},
	})
	if got == nil || got.Usage == nil || got.Usage.Turn == nil {
		t.Fatal("no turn counts")
	}
	if tc := got.Usage.Turn; tc.CacheRead != 186683 || tc.CacheWrite != 104017 || tc.Output != 237 {
		t.Errorf("turn = cr %d cw %d out %d; want cr 186683 cw 104017 out 237", tc.CacheRead, tc.CacheWrite, tc.Output)
	}
}
