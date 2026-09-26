package ccstream

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/log"
)

// subagentTranscriptPath returns the on-disk path of the subagent transcript
// for agentID (== the task_started task_id, == the SubagentStop agent_id),
// which CC writes at
//
//	~/.claude/projects/<slug>/<parent-session-uuid>/subagents/agent-<agent_id>.jsonl
//
// Returns "" if the session id isn't known yet or the home dir can't be found.
func (b *Backend) subagentTranscriptPath(agentID string) string {
	return b.subagentFilePath(agentID, ".jsonl")
}

// subagentFilePath returns the path of a per-subagent file CC writes under
//
//	~/.claude/projects/<slug>/<parent-session-uuid>/subagents/agent-<agent_id><suffix>
//
// for a given suffix (".jsonl" for the transcript, ".meta.json" for the metadata
// sidecar). Returns "" if the session id isn't known yet or the home dir can't be
// found. The parent session uuid is STABLE across a `claude --resume` (verified
// live 2026-07-20), so this same path resolves the pre-restart subagent files
// after a foci restart — the basis for identity rehydration (#1433).
func (b *Backend) subagentFilePath(agentID, suffix string) string {
	if agentID == "" {
		return ""
	}
	b.mu.Lock()
	sessionID := b.sessionID
	b.mu.Unlock()
	if sessionID == "" || b.workDir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ccProjectsDir, projectSlug(b.workDir), sessionID,
		"subagents", "agent-"+agentID+suffix)
}

// subagentMeta is the subset of CC's agent-<task_id>.meta.json sidecar foci reads:
// the bridge from a subagent's stable task_id (the filename) to the ORIGINAL Agent
// tool_use id (== the run's group key) and its description (== the chit label). CC
// writes this next to the subagent transcript when the Agent tool spawns the
// subagent, and it persists across a restart. SpawnDepth (1 for a subagent the
// main thread spawned) and ParentAgentID (the spawner's task_id, set only when
// nested) identify a nested subagent after a restart (#1554; captured live on CC
// 2.1.280).
type subagentMeta struct {
	Description   string `json:"description"`
	ToolUseID     string `json:"toolUseId"`
	SpawnDepth    int    `json:"spawnDepth"`
	ParentAgentID string `json:"parentAgentId"`
}

// loadSubagentMeta reads the task_id -> {groupKey, label} bridge from CC's on-disk
// agent-<taskID>.meta.json so subagent identity survives a foci restart (#1433):
// after a restart the Agent tool_use block is never re-streamed, so this file is
// the only source of a resumed subagent's original group key + label. Returns
// ok=false when the sidecar is absent/unreadable or carries no tool_use id (so the
// caller can fall through to "not a subagent we can identify").
func (b *Backend) loadSubagentMeta(taskID string) (groupKey, label string, ok bool) {
	m, ok := b.readSubagentMeta(taskID)
	return m.ToolUseID, m.Description, ok
}

// readSubagentMeta parses the whole sidecar; see loadSubagentMeta for ok.
func (b *Backend) readSubagentMeta(taskID string) (subagentMeta, bool) {
	path := b.subagentFilePath(taskID, ".meta.json")
	if path == "" {
		return subagentMeta{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return subagentMeta{}, false
	}
	var m subagentMeta
	if json.Unmarshal(data, &m) != nil || m.ToolUseID == "" {
		return subagentMeta{}, false
	}
	return m, true
}

// Foreground subagents (Task/Agent tool run synchronously) do NOT stream their
// assistant text to the parent stdout stream at all — Claude Code filters text
// blocks out of the parent forwarding path (tools/AgentTool/AgentTool.tsx),
// emitting only the subagent's tool_use/tool_result blocks and, at completion,
// the aggregated final message in the Agent tool_result. Empirically verified
// via headless `claude -p --output-format stream-json` captures: zero
// text_delta events and zero parent_tool_use_id-tagged assistant text for a
// foreground subagent, regardless of length.
//
// The subagent's full message stream IS written — line by line, flushed
// per-message — to its own transcript at
//
//	~/.claude/projects/<slug>/<parent-session-uuid>/subagents/agent-<agent_id>.jsonl
//
// So to populate a foreground subagent's chit live, we tail that file and
// forward each newly-appended assistant text block via OnSubagentText under the
// run's group key (the Agent tool_use id, matching OnSubagentStart/End).
//
// TEXT is foreground-only: background subagents already surface their text in
// the parent stdout stream as parent_tool_use_id-tagged assistant messages
// (OnAssistant → OnSubagentText), so forwarding it from the transcript too
// would double-deliver. USAGE is tailed for BOTH — the parent stream never
// completes output_tokens, so a background subagent's output would otherwise
// stay a 1-3 placeholder with no second source to correct it (#1880).
var (
	// subagentTailPoll is how often the tailer checks the transcript file for
	// newly-appended bytes (and, before the file exists, for its creation).
	// A var (not const) so tests can shorten it.
	subagentTailPoll = 200 * time.Millisecond
	// subagentTailFileWait bounds how long the tailer waits for CC to create
	// the transcript file before giving up (CC creates it a few seconds into
	// the run). A subagent that errors before writing anything just times out.
	subagentTailFileWait = 60 * time.Second

	// subagentTailSettle bounds the post-stop drain. finalize() fires on CC's
	// task_notification:completed, which arrives on CC's STDOUT STREAM — a
	// different channel from CC's append to the transcript FILE, with no ordering
	// between them. So the last record can still be in flight when we are told
	// the subagent finished (#1938; measured at ~110ms on live probes). The tail
	// keeps draining until it reads a TERMINAL record or this expires, whichever
	// comes first — so the happy path costs nothing and a subagent that never
	// writes one (killed, errored, rate-limited) still terminates.
	subagentTailSettle = 3 * time.Second
)

// subagentTailManager tails foreground subagent transcript files and forwards
// each appended assistant text block as a subagent progress message. Keyed by
// the Agent tool_use id (the run's group key). Safe for concurrent use.
type subagentTailManager struct {
	mu       sync.Mutex
	expectFg map[string]bool          // tool_use_id -> awaiting task_started (fg Agent PreToolUse seen)
	tails    map[string]*subagentTail // tool_use_id -> running tail
	// deliver forwards one subagent text block to the session. Captured from
	// the backend so it always reads the current SessionEvents. May be nil in
	// tests that only exercise lifecycle bookkeeping.
	deliver func(groupKey, text string)
	// noteUsage records one transcript message's token usage. Separate from
	// deliver because the two have different failure modes: dropping a text
	// block loses display, dropping usage loses money. May be nil in tests
	// that only exercise text forwarding.
	noteUsage func(agent, model, id string, at time.Time, complete bool, u TokenUsage)
	lg        *log.ComponentLogger
}

// wantText is whether this tail forwards the subagent's TEXT to the session,
// as opposed to only recording its usage. Foreground subagents need both: CC
// filters their assistant text out of the parent stream. Background subagents
// need only the accounting — their text already streams to the parent, so
// delivering it again would double it in the chat.
type subagentTail struct {
	wantText bool
	stop     chan struct{}
	done     chan struct{}
	// sawTerminal records that a record with a TERMINAL stop_reason has been
	// read — the run's own end-of-stream marker, on the SAME channel as the data,
	// so it cannot race the data the way the stream event does (#1938).
	sawTerminal atomic.Bool
	// lines counts transcript lines this tail delivered. Reported at close so
	// a tail that opened its file but read nothing is distinguishable from one
	// that never opened it at all (#1934).
	lines atomic.Int64
	// usage counts the assistant lines whose usage was handed to the
	// accumulator, and completed the subset carrying a stop_reason — the only
	// lines that can reach a subagent_turn row (#1923). Reported at close
	// beside lines, so "read lines but none were billable" and "billable but
	// never completed" each have their own number rather than a guess (#1936).
	usage     atomic.Int64
	completed atomic.Int64
}

func newSubagentTailManager(deliver func(groupKey, text string), noteUsage func(agent, model, id string, at time.Time, complete bool, u TokenUsage), lg *log.ComponentLogger) *subagentTailManager {
	if lg == nil {
		lg = log.NewComponentLogger("ccstream")
	}
	return &subagentTailManager{
		noteUsage: noteUsage,
		expectFg:  make(map[string]bool),
		tails:     make(map[string]*subagentTail),
		deliver:   deliver,
		lg:        lg,
	}
}

// expectForeground records that a foreground Agent subagent with this tool_use
// id has started (its PreToolUse hook fired). The tail begins only once
// task_started supplies the agent_id needed to locate the transcript file.
func (m *subagentTailManager) expectForeground(toolUseID string) {
	if m == nil || toolUseID == "" {
		return
	}
	m.mu.Lock()
	m.expectFg[toolUseID] = true
	m.mu.Unlock()
}

// maybeStart begins tailing path for EVERY subagent, foreground or background.
// Whether a foreground Agent start was recorded (expectForeground) decides only
// whether the tail also forwards TEXT.
//
// It used to start for foreground subagents alone, because the text was the
// only thing it existed to recover. That left background subagents' USAGE
// coming from the parent stream only — and the stream never completes
// output_tokens: every assistant line carries stop_reason null and a running
// count of 1-3 that is never revised (probe-verified 2026-09-10 on two
// captures, and again on a background subagent's own transcript, where the
// same message reads 2 then 319). Their transcripts DO carry the completed
// figure, exactly like foreground ones — verified by starting a background
// subagent and reading the file it wrote.
//
// So the gate was right about text and wrong about money. Tailing everything
// costs one goroutine and one file handle per live subagent, bounded by how
// many can run at once.
func (m *subagentTailManager) maybeStart(toolUseID, path string) {
	if m == nil || toolUseID == "" || path == "" {
		return
	}
	m.mu.Lock()
	wantText := m.expectFg[toolUseID]
	delete(m.expectFg, toolUseID)
	if _, running := m.tails[toolUseID]; running {
		m.mu.Unlock()
		m.lg.Debugf("subagent tail: NOT started, already running for group=%s", toolUseID)
		return
	}
	t := &subagentTail{wantText: wantText, stop: make(chan struct{}), done: make(chan struct{})}
	m.tails[toolUseID] = t
	m.mu.Unlock()

	go m.run(toolUseID, path, t)
}

// finalize stops the tail for toolUseID, draining any final appended lines
// first, and blocks until the tail goroutine exits so all of the subagent's
// text is delivered before its SubagentEnd. Also clears a pending
// expectForeground entry for a subagent that ended before task_started (e.g. an
// immediate error). Idempotent.
func (m *subagentTailManager) finalize(toolUseID string) {
	if m == nil || toolUseID == "" {
		return
	}
	m.mu.Lock()
	delete(m.expectFg, toolUseID)
	t := m.tails[toolUseID]
	delete(m.tails, toolUseID)
	m.mu.Unlock()
	if t == nil {
		// Named so a finalize that stopped nothing is not read as one that
		// stopped a tail: the tail was never started, or already ended (#1936).
		m.lg.Debugf("subagent tail: finalize found no running tail for group=%s", toolUseID)
		return
	}
	close(t.stop)
	<-t.done
}

// clearPendingForeground drops a pending expectForeground entry for toolUseID.
// Called from the Agent PostToolUse hook — which MUST NOT stop a tail (#1934).
//
// Measured on CC 2.1.261 (timing.sh, 4 scenarios): a BACKGROUND Agent
// PostToolUse fires at +0.03s, the instant the task is launched, with the whole
// run still ahead of it; a FOREGROUND one fires ~30ms AFTER the subagent has
// genuinely ended. So stopping here is fatal for one kind and redundant for the
// other, and task_notification:completed — the real end for BOTH — is the only
// place a tail should be finalized.
//
// This used to be finalizeForeground, which stopped the tail when it was
// labelled foreground. That guard never protected the subagents it was written
// for: the Agent tool backgrounds BY DEFAULT, so run_in_background is absent and
// was read as false, and every ordinary subagent was armed foreground and killed
// at launch — before CC had created the transcript. Its usage then reached no
// row at all and was absorbed by the parent turn at the dearer unobserved rate.
//
// The expectForeground entry must still be cleared here: a subagent that ended
// before task_started (an immediate error) has no tail to look up, and leaving
// the entry would make a later, unrelated tail forward text.
func (m *subagentTailManager) clearPendingForeground(toolUseID string) {
	if m == nil || toolUseID == "" {
		return
	}
	m.mu.Lock()
	delete(m.expectFg, toolUseID)
	m.mu.Unlock()
}

// stopAll cancels every running tail without waiting. Called on backend
// teardown so no tailer goroutine outlives the process.
func (m *subagentTailManager) stopAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	tails := m.tails
	m.tails = make(map[string]*subagentTail)
	m.expectFg = make(map[string]bool)
	m.mu.Unlock()
	for _, t := range tails {
		close(t.stop)
	}
}

// run tails path, forwarding appended assistant text blocks until stop is
// closed. On stop it performs one final drain so text written right before
// completion is not lost. Closes t.done on exit.
func (m *subagentTailManager) run(groupKey, path string, t *subagentTail) {
	defer close(t.done)

	f := m.waitForFile(path, t.stop)
	if f == nil {
		return
	}
	defer f.Close()
	m.lg.Debugf("subagent tail: opened %s (group=%s wantText=%v)", path, groupKey, t.wantText)
	defer func() {
		m.lg.Debugf("subagent tail: closed group=%s lines=%d usage=%d completed=%d terminal=%v",
			groupKey, t.lines.Load(), t.usage.Load(), t.completed.Load(), t.sawTerminal.Load())
	}()

	var acc []byte
	drain := func() {
		for {
			chunk := make([]byte, 8192)
			n, err := f.Read(chunk)
			if n > 0 {
				acc = append(acc, chunk[:n]...)
				for {
					i := bytes.IndexByte(acc, '\n')
					if i < 0 {
						break
					}
					r := m.deliverLine(groupKey, acc[:i], t.wantText)
					if r.terminal {
						t.sawTerminal.Store(true)
					}
					if r.usage {
						t.usage.Add(1)
					}
					if r.complete {
						t.completed.Add(1)
					}
					t.lines.Add(1)
					acc = acc[i+1:]
				}
			}
			if err != nil { // io.EOF (no more appended bytes yet) or a read error
				return
			}
		}
	}

	for {
		drain()
		select {
		case <-t.stop:
			// The stream event said "finished". The FILE has not necessarily
			// caught up, and the two are not ordered (#1938). Keep draining until
			// the run's own terminal record arrives or the settle window closes.
			deadline := time.Now().Add(subagentTailSettle)
			for {
				drain()
				if t.sawTerminal.Load() {
					return
				}
				if time.Now().After(deadline) {
					m.lg.Debugf("subagent tail: no terminal record within %s, closing anyway (group=%s lines=%d)",
						subagentTailSettle, groupKey, t.lines.Load())
					return
				}
				time.Sleep(subagentTailPoll)
			}
		case <-time.After(subagentTailPoll):
		}
	}
}

// waitForFile opens path once it exists, polling until it appears or stop is
// closed or the wait budget expires. Returns nil if the file never appeared.
func (m *subagentTailManager) waitForFile(path string, stop <-chan struct{}) *os.File {
	deadline := time.Now().Add(subagentTailFileWait)
	for {
		if f, err := os.Open(path); err == nil {
			return f
		}
		select {
		case <-stop:
			// One last attempt — the subagent may have written and finished
			// within a single poll interval.
			if f, err := os.Open(path); err == nil {
				return f
			}
			// SILENT UNTIL #1934. A tail stopped before its transcript appeared
			// logged nothing at all, because only the DEADLINE branch below
			// reported. Anything shorter-lived than subagentTailFileWait —
			// which is every subagent that finishes inside 60s — vanished
			// without trace, and the missing usage looked like no subagent.
			m.lg.Debugf("subagent tail: stopped before transcript appeared: %s", path)
			return nil
		case <-time.After(subagentTailPoll):
		}
		if time.Now().After(deadline) {
			m.lg.Debugf("subagent tail: transcript never appeared: %s", path)
			return nil
		}
	}
}

// transcriptLine is the subset of a CC transcript JSONL record we parse.
type transcriptLine struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	// Timestamp is when CC WROTE this message — the moment the tokens were
	// billed, as against the moment foci read the line. The two differ by the
	// tail's delivery lag, which is ~60ms for a foreground subagent and can be
	// half an hour for a background one. Accounting must bucket by this field
	// and not by arrival, or a burst of catch-up lines books a previous turn's
	// spend onto whichever turn happened to be open when they landed (#1909).
	Timestamp string `json:"timestamp"`
	Message   struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		// ID/Model/Usage carry the accounting half of the line. A FOREGROUND
		// subagent's pure-text messages never reach the parent stream, and
		// their usage goes with them, so the transcript is the only complete
		// source for what that subagent actually spent (#1866 P2).
		ID    string     `json:"id"`
		Model string     `json:"model"`
		Usage TokenUsage `json:"usage"`
		// StopReason is non-nil only on the line where the message COMPLETED.
		// CC's result.modelUsage counts a message at completion, so this is the
		// line whose usage may be folded into the accounting — see note()
		// (#1923).
		StopReason *string `json:"stop_reason"`
	} `json:"message"`
}

// lineResult is what deliverLine observed about one transcript line.
//
// terminal: the record ENDS the run — a non-nil stop_reason that is not
// "tool_use". "tool_use" means the assistant will be called again, so it is
// explicitly NOT terminal; the live probe that exposed #1938 had exactly that
// shape (tool_use, then the end_turn that was lost).
//
// usage: the line's usage was handed to the accumulator; complete: it carried
// a stop_reason, so the accumulator counts it (#1923). Both feed the tail's
// close line (#1936).
type lineResult struct {
	terminal, usage, complete bool
}

// deliverLine parses one transcript line and forwards each assistant text block
// as subagent progress. Non-assistant records (the input prompt, tool_use,
// tool_result, attachments) and non-text blocks are skipped.
func (m *subagentTailManager) deliverLine(groupKey string, line []byte, wantText bool) (r lineResult) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var rec transcriptLine
	if err := json.Unmarshal(line, &rec); err != nil {
		return
	}
	if rec.Type != "assistant" {
		return
	}
	// Accounting first, and NOT gated on m.deliver. The two sinks fail
	// independently: a consumer with no text sink still spends real money, and
	// the previous early return on a nil deliver would have discarded every
	// token it spent.
	if m.noteUsage != nil {
		// groupKey IS the Agent tool_use id, so the usage carries the identity of
		// the subagent that spent it, not merely the fact that a subagent spent it
		// (#1880 phase C).
		// A line whose timestamp is absent or unparseable yields the zero
		// time, which the accumulator reads as "unknown, treat as now" — the
		// pre-#1909 behaviour, so a format change degrades to the old bucketing
		// rather than dropping the usage.
		at, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
		m.noteUsage(groupKey, rec.Message.Model, rec.Message.ID, at,
			rec.Message.StopReason != nil, rec.Message.Usage)
		r.usage = true
		r.complete = rec.Message.StopReason != nil
	}
	r.terminal = rec.Message.StopReason != nil && *rec.Message.StopReason != "tool_use"
	// Text only when this tail was started for a FOREGROUND subagent. A
	// background subagent's text already reaches the parent stream, so
	// forwarding it here would render it twice.
	if !wantText || m.deliver == nil {
		return r
	}
	for _, blk := range rec.Message.Content {
		if blk.Type == "text" && blk.Text != "" {
			m.deliver(groupKey, blk.Text)
		}
	}
	return r
}
