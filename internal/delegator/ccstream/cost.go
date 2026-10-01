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
)

// What a CC process's cost reports start from (#1674, #2012).
//
// EVERY counter in CC's per-result ModelUsage map is CUMULATIVE over the life
// of the CC PROCESS — cost, output tokens, cache reads, cache writes —
// probe-verified 2026-08-05 on one process fed four turns (cacheRead
// 21,624 / 46,722 / 72,545 / 98,434 for four identical trivial prompts). The
// ledger adapter (ccbook.go) therefore measures its remainder from a
// per-process baseline, which this file reads for a --resume.
//
// NOTE FOR ANYONE RE-VERIFYING THIS: `claude -p --resume` spawns a FRESH
// PROCESS PER TURN, so a per-process counter cannot accumulate and the probe
// cannot distinguish cumulative from per-turn. It must be ONE process fed
// multiple turns.

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
// whose own usage was 22,766. Without a baseline the whole history would be
// booked as this process's remainder. Live, before the baseline, a keepalive
// fork was booked $111 against about $0.87 of real work.
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
// logged and also gets nil, so the first remainder is over-stated rather than
// the launch failing.
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
	b.logger().Warnf("resume baseline for %s unreadable: %v — this process's first ledger remainder will hold the whole conversation's history (#2012)",
		sessionID, err)
	return nil
}
