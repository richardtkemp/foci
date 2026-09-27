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
	// the subagent finished (#1938; measured at ~110ms and ~220ms on live
	// probes). The tail keeps draining until the transcript ENDS at a terminal
	// record or this expires, whichever comes first — so the happy path costs
	// nothing and a subagent that never writes one (killed, errored,
	// rate-limited) still terminates.
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

	// run is the accounting gate for a REACTIVATION tail (#2057); nil for run 1,
	// which books everything it reads.
	run *runStartGate
}

// usageNote is one transcript line's usage, as the accumulator takes it.
type usageNote struct {
	model, id string
	at        time.Time
	complete  bool
	u         TokenUsage
}

// runStartGate keeps a reactivation tail from booking the earlier runs again
// (#2057). A tail for run 2+ reads the transcript from byte 0, so without a
// gate every earlier run's messages reach the accumulator a second time.
// In-session that is harmless, because the accumulator still holds their ids.
// After a foci restart it holds nothing: run 1 was re-counted in the window when
// the resume came before the new process's first result, and filed as late
// spend (#1918 corrections against a parent turn it was never part of) when it
// came after.
//
// The boundary is read from the transcript's own ORDER, with no timing window.
// Run 1 always ends at rest before a resume can start, and a Stop-hook bounce
// inside run 1 comes before run 1's final terminal record. So the run's prompt
// is the first prompt record (a user record with string content) AFTER, in file
// order, the last terminal assistant record stamped before task_started was
// received; every assistant record before that prompt is an earlier run's and
// is kept out of the accounting, and everything after it counts. The receive
// time matters only if run 2 reached its own terminal record before foci saw
// task_started.
//
// Streaming makes "the last terminal record" unknowable at the moment a prompt
// is read, so records after a candidate prompt are HELD until a record settles
// it: a later terminal stamped before the receive time means the candidate was
// a bounce inside run 1 (drop what was held), and any record stamped at or
// after the receive time means the run has begun (release it). If no terminal
// record precedes the receive time, the gate cannot place the boundary and
// falls back to booking everything, as before the gate existed, and says so.
// Nothing is persisted: the file carries its own boundary.
type runStartGate struct {
	// startedAt is when task_started for this run was received.
	startedAt time.Time
	group     string
	lg        *log.ComponentLogger

	armed      bool      // a terminal record stamped before startedAt has been read
	promptAt   time.Time // the candidate run prompt after the last such record
	havePrompt bool
	open       bool // boundary settled (or fallback): book directly
	pending    []usageNote
	dropped    int // assistant records kept out as an earlier run's
}

// prompt notes a prompt record (user, string content).
func (g *runStartGate) prompt(at time.Time) {
	if g.open || !g.armed || g.havePrompt {
		return
	}
	g.havePrompt, g.promptAt = true, at
}

// assistant takes one assistant record and returns the notes now due to the
// accumulator: none while the boundary is unsettled, or the held ones once it
// settles.
func (g *runStartGate) assistant(n usageNote, terminal bool) []usageNote {
	if g.open {
		return []usageNote{n}
	}
	stamped := !n.at.IsZero()
	switch {
	case stamped && !n.at.Before(g.startedAt):
		// This run is under way (it was announced before this was written).
		if !g.armed {
			g.lg.Debugf("subagent tail: run start NOT found group=%s: no at-rest record before task_started (received %s), booking the whole transcript as before #2057",
				g.group, g.startedAt.Format(time.RFC3339Nano))
		} else {
			g.logStart()
		}
		return g.release(n)
	case stamped && terminal:
		// An earlier run ended here: whatever was held since the last one,
		// and this record, are that run's.
		g.dropped += len(g.pending) + 1
		g.pending = nil
		g.armed, g.havePrompt = true, false
		return nil
	case g.armed && !g.havePrompt:
		// Between an at-rest record and the next prompt: not this run's.
		g.dropped++
		return nil
	default:
		// After a candidate prompt, or before any at-rest record (kept for the
		// fallback): undecided.
		g.pending = append(g.pending, n)
		return nil
	}
}

// close settles a gate the transcript never settled, when the tail ends.
func (g *runStartGate) close() []usageNote {
	if g.open {
		return nil
	}
	switch {
	case !g.armed:
		g.lg.Debugf("subagent tail: run start NOT found group=%s: no at-rest record before task_started (received %s), booking the whole transcript as before #2057",
			g.group, g.startedAt.Format(time.RFC3339Nano))
		return g.release()
	case g.havePrompt:
		// A prompt after run 1's end and nothing to contradict it: run 2
		// wrote records with no usable timestamp.
		g.logStart()
		return g.release()
	default:
		g.lg.Debugf("subagent tail: run start NOT found group=%s: no prompt after the last at-rest record, nothing booked for this run",
			g.group)
		g.dropped += len(g.pending)
		g.pending = nil
		return nil
	}
}

func (g *runStartGate) release(more ...usageNote) []usageNote {
	out := append(g.pending, more...)
	g.pending, g.open = nil, true
	return out
}

func (g *runStartGate) logStart() {
	g.lg.Debugf("subagent tail: run start group=%s prompt_at=%s (first prompt after the last at-rest record; task_started received %s), earlier records skipped=%d",
		g.group, g.promptAt.Format(time.RFC3339Nano), g.startedAt.Format(time.RFC3339Nano), g.dropped)
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
//
// reactivatedAt is the task_started receive time of a REACTIVATION (run 2+),
// which limits the tail's accounting to that run (runStartGate, #2057); zero
// for run 1.
func (m *subagentTailManager) maybeStart(toolUseID, path string, reactivatedAt time.Time) {
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
	if !reactivatedAt.IsZero() {
		t.run = &runStartGate{startedAt: reactivatedAt, group: toolUseID, lg: m.lg}
	}
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
// closed and the transcript has caught up to the run's end (see atRest). Closes
// t.done on exit.
func (m *subagentTailManager) run(groupKey, path string, t *subagentTail) {
	defer close(t.done)

	f := m.waitForFile(path, t.stop)
	if f == nil {
		return
	}
	defer f.Close()
	m.lg.Debugf("subagent tail: opened %s (group=%s wantText=%v)", path, groupKey, t.wantText)

	// atRest: the transcript read so far ENDS at a terminal record, i.e. its last
	// conversational record is an assistant message whose stop_reason ends the
	// run. This is the run's own end-of-stream marker, on the SAME channel as the
	// data, so it cannot race the data the way the stream event does (#1938).
	//
	// It must track the LAST such record, not remember any: a run can write an
	// end_turn and carry on (a Stop hook sends it back for SubagentHandback; a
	// SendMessage reactivates it, and a reactivation tail re-reads run 1 from
	// byte 0). A sticky "saw one" flag was already set when stop arrived for
	// those runs, so the tail returned before their final record was written.
	atRest := false
	defer func() {
		beforeRun := 0
		if t.run != nil {
			u, c := m.emitUsage(groupKey, t.run.close())
			t.usage.Add(int64(u))
			t.completed.Add(int64(c))
			beforeRun = t.run.dropped
		}
		m.lg.Debugf("subagent tail: closed group=%s lines=%d usage=%d completed=%d terminal=%v before_run=%d",
			groupKey, t.lines.Load(), t.usage.Load(), t.completed.Load(), atRest, beforeRun)
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
					r := m.deliverLine(groupKey, acc[:i], t.wantText, t.run)
					if r.conversational {
						atRest = r.terminal
					}
					t.usage.Add(int64(r.usage))
					t.completed.Add(int64(r.complete))
					t.lines.Add(1)
					acc = acc[i+1:]
				}
			}
			if err != nil { // io.EOF (no more appended bytes yet) or a read error
				return
			}
		}
	}

	// stop and settle are the two halves of "finished". stop is the stream event
	// saying so; the FILE has not necessarily caught up, and the two are not
	// ordered (#1938). So once stopped, the tail keeps draining until the
	// transcript itself is at rest, or until the settle window closes for a run
	// that never writes a terminal record (killed, errored, rate-limited).
	stop := t.stop
	var settle <-chan time.Time
	for {
		drain()
		if stop == nil && atRest {
			return
		}
		select {
		case <-stop:
			stop = nil
			settle = time.After(subagentTailSettle)
		case <-settle:
			m.lg.Debugf("subagent tail: no terminal record within %s, closing anyway (group=%s lines=%d)",
				subagentTailSettle, groupKey, t.lines.Load())
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
		// Content stays raw until text delivery needs it: a user record's
		// content is a plain STRING when it is a prompt (a SendMessage, a Stop
		// hook's feedback), and decoding it as blocks would fail the whole line,
		// hiding the one fact the tail needs from it, that the run continued.
		Content json.RawMessage `json:"content"`
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
// conversational: the record is a turn of the conversation (assistant or
// user), so it decides whether the transcript is at rest; attachments and other
// bookkeeping records do not (live transcripts can end with attachments after
// the final end_turn).
//
// terminal: the record ENDS the run — a non-nil stop_reason that is not
// "tool_use". "tool_use" means the assistant will be called again, so it is
// explicitly NOT terminal; the live probe that exposed #1938 had exactly that
// shape (tool_use, then the end_turn that was lost). A user record is never
// terminal: whatever the assistant said before it, the run went on.
//
// usage: how many lines' usage this line handed to the accumulator; complete:
// how many of those carried a stop_reason, so the accumulator counts them
// (#1923). Both feed the tail's close line (#1936). Without a run gate that is
// this line alone (0 or 1); a gate can hold lines and release several at once
// (#2057).
type lineResult struct {
	conversational, terminal bool
	usage, complete          int
}

// emitUsage hands notes to the accumulator and returns how many, and how many
// of them were complete.
func (m *subagentTailManager) emitUsage(groupKey string, notes []usageNote) (usage, complete int) {
	if m.noteUsage == nil {
		return 0, 0
	}
	for _, n := range notes {
		// groupKey IS the Agent tool_use id, so the usage carries the identity of
		// the subagent that spent it, not merely the fact that a subagent spent it
		// (#1880 phase C).
		m.noteUsage(groupKey, n.model, n.id, n.at, n.complete, n.u)
		usage++
		if n.complete {
			complete++
		}
	}
	return usage, complete
}

// deliverLine parses one transcript line and forwards each assistant text block
// as subagent progress. Non-assistant records (the input prompt, tool_use,
// tool_result, attachments) and non-text blocks are skipped. run, when non-nil,
// limits the ACCOUNTING to the current run (#2057); text is not gated.
func (m *subagentTailManager) deliverLine(groupKey string, line []byte, wantText bool, run *runStartGate) (r lineResult) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var rec transcriptLine
	if err := json.Unmarshal(line, &rec); err != nil {
		return
	}
	// A line whose timestamp is absent or unparseable yields the zero time,
	// which the accumulator reads as "unknown, treat as now" — the pre-#1909
	// behaviour, so a format change degrades to the old bucketing rather than
	// dropping the usage.
	at, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if rec.Type == "user" {
		r.conversational = true
		// A prompt's content is a plain string; a tool_result's is an array of
		// blocks, and never starts a run.
		if c := bytes.TrimSpace(rec.Message.Content); run != nil && len(c) > 0 && c[0] == '"' {
			run.prompt(at)
		}
		return
	}
	if rec.Type != "assistant" {
		return
	}
	r.conversational = true
	// Accounting first, and NOT gated on m.deliver. The two sinks fail
	// independently: a consumer with no text sink still spends real money, and
	// the previous early return on a nil deliver would have discarded every
	// token it spent.
	r.terminal = rec.Message.StopReason != nil && *rec.Message.StopReason != "tool_use"
	n := usageNote{model: rec.Message.Model, id: rec.Message.ID, at: at,
		complete: rec.Message.StopReason != nil, u: rec.Message.Usage}
	notes := []usageNote{n}
	if run != nil {
		notes = run.assistant(n, r.terminal)
	}
	r.usage, r.complete = m.emitUsage(groupKey, notes)
	// Text only when this tail was started for a FOREGROUND subagent. A
	// background subagent's text already reaches the parent stream, so
	// forwarding it here would render it twice.
	if !wantText || m.deliver == nil {
		return r
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return r
	}
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			m.deliver(groupKey, blk.Text)
		}
	}
	return r
}
