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
// subagent, and it persists across a restart.
type subagentMeta struct {
	Description string `json:"description"`
	ToolUseID   string `json:"toolUseId"`
}

// loadSubagentMeta reads the task_id -> {groupKey, label} bridge from CC's on-disk
// agent-<taskID>.meta.json so subagent identity survives a foci restart (#1433):
// after a restart the Agent tool_use block is never re-streamed, so this file is
// the only source of a resumed subagent's original group key + label. Returns
// ok=false when the sidecar is absent/unreadable or carries no tool_use id (so the
// caller can fall through to "not a subagent we can identify").
func (b *Backend) loadSubagentMeta(taskID string) (groupKey, label string, ok bool) {
	path := b.subagentFilePath(taskID, ".meta.json")
	if path == "" {
		return "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	var m subagentMeta
	if json.Unmarshal(data, &m) != nil || m.ToolUseID == "" {
		return "", "", false
	}
	return m.ToolUseID, m.Description, true
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
	// lines counts transcript lines this tail delivered. Reported at close so
	// a tail that opened its file but read nothing is distinguishable from one
	// that never opened it at all (#1934).
	lines atomic.Int64
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
		return
	}
	close(t.stop)
	<-t.done
}

// finalizeForeground stops the tail for toolUseID ONLY IF it is a foreground
// one. Called from the Agent PostToolUse hook (#1924).
//
// A FOREGROUND Agent tool_use resolves when the subagent has genuinely finished,
// so its PostToolUse is the right moment to drain the last text into the chit.
// A BACKGROUND one resolves the INSTANT the task is launched — the hook's own
// comment says so — so finalizing there kills the tail before CC has even
// created the transcript.
//
// That used to be harmless: the tail ran for foreground subagents only, and the
// call site's comment still says "No-op for background / untailed subagents".
// 55faa1d8 made maybeStart tail EVERY subagent for its USAGE and left the
// comment behind, so the no-op became a kill. Measured on a live background
// subagent: 13 completed messages worth 47,438 output tokens in the transcript
// against 86 in its rows — everything present had come from the parent stream,
// and the tail had delivered nothing.
//
// Background tails end at task_notification:completed instead, which the same
// comment already names as the real end signal for both kinds.
//
// A pending expectForeground entry is cleared either way: a subagent that ended
// before task_started (an immediate error) has no tail to look up, and leaving
// the entry would make a later, unrelated tail deliver text.
func (m *subagentTailManager) finalizeForeground(toolUseID string) {
	if m == nil || toolUseID == "" {
		return
	}
	m.mu.Lock()
	delete(m.expectFg, toolUseID)
	t := m.tails[toolUseID]
	if t == nil || !t.wantText {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.finalize(toolUseID)
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
		m.lg.Debugf("subagent tail: closed group=%s lines=%d", groupKey, t.lines.Load())
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
					m.deliverLine(groupKey, acc[:i], t.wantText)
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
			drain() // final read: catch text flushed just before completion
			return
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

// deliverLine parses one transcript line and forwards each assistant text block
// as subagent progress. Non-assistant records (the input prompt, tool_use,
// tool_result, attachments) and non-text blocks are skipped.
func (m *subagentTailManager) deliverLine(groupKey string, line []byte, wantText bool) {
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
	}
	// Text only when this tail was started for a FOREGROUND subagent. A
	// background subagent's text already reaches the parent stream, so
	// forwarding it here would render it twice.
	if !wantText || m.deliver == nil {
		return
	}
	for _, blk := range rec.Message.Content {
		if blk.Type == "text" && blk.Text != "" {
			m.deliver(groupKey, blk.Text)
		}
	}
}
