package ccstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"foci/internal/delegator"
	"foci/internal/log"
)

// ccProjectsDir is where Claude Code stores per-project session transcripts,
// relative to the user's home: ~/.claude/projects/<cwd-slug>/<uuid>.jsonl.
const ccProjectsDir = ".claude/projects"

// nonAlnum matches the characters Claude Code replaces when slugging a path.
var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// projectSlug converts a workspace path to Claude Code's project directory
// name. CC replaces every non-alphanumeric character with '-', not just '/':
// e.g. "/home/foci/clutch" → "-home-foci-clutch", "/a/.b_c" → "-a--b-c".
// (Mirrors the same mapping in the cctmux backend; kept local to avoid a
// cross-package dependency for a one-line transform.)
func projectSlug(path string) string {
	return nonAlnum.ReplaceAllString(path, "-")
}

// ForkSession implements delegator.BackendBrancher for the CC stream backend.
//
// It forks a Claude Code conversation by copying the parent's transcript
// (~/.claude/projects/<slug>/<parent>.jsonl) to a new UUID-named file, with
// every line's "sessionId" field rewritten to the new UUID. Claude Code has
// no session registry gate — a transcript present in the correct project-slug
// directory can be resumed with `claude --resume <uuid>`, so foci does not
// need CC to pre-create the session.
//
// This is a pure filesystem operation: it does not require a running backend
// and never touches the live process. The returned SessionID is a fresh UUID
// whose transcript is a copy of the parent's, ready to be persisted as the
// branch key's cc_resume_id and resumed by the normal getOrCreate path.
func (b *Backend) ForkSession(ctx context.Context, req delegator.ForkRequest) (delegator.ForkResult, error) {
	if req.ParentSessionID == "" {
		return delegator.ForkResult{}, fmt.Errorf("ccstream fork: empty parent session id")
	}
	if req.WorkDir == "" {
		return delegator.ForkResult{}, fmt.Errorf("ccstream fork: empty workdir")
	}
	if req.TruncateAfter > 0 {
		// Mid-conversation truncation requires mapping foci's message-count
		// branch point onto CC's transcript line chain (see plan's Deferred
		// section). Not supported in v1 — fork the whole conversation only.
		return delegator.ForkResult{}, fmt.Errorf("ccstream fork: TruncateAfter>0 not supported")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return delegator.ForkResult{}, fmt.Errorf("ccstream fork: home dir: %w", err)
	}
	dir := filepath.Join(home, ccProjectsDir, projectSlug(req.WorkDir))
	parentPath := filepath.Join(dir, req.ParentSessionID+".jsonl")

	newID := uuid.NewString()
	newPath := filepath.Join(dir, newID+".jsonl")

	if err := forkTranscript(parentPath, newPath, req.ParentSessionID, newID, b.logger()); err != nil {
		return delegator.ForkResult{}, err
	}
	return delegator.ForkResult{SessionID: newID}, nil
}

// CleanupSession implements delegator.BackendBrancher: it deletes the CC
// transcript for req.SessionID (~/.claude/projects/<slug>/<uuid>.jsonl),
// reclaiming an ephemeral fork. A missing file is not an error. Pure
// filesystem operation — no running backend required.
func (b *Backend) CleanupSession(_ context.Context, req delegator.CleanupRequest) error {
	if req.SessionID == "" || req.WorkDir == "" {
		return fmt.Errorf("ccstream cleanup: empty session id or workdir")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("ccstream cleanup: home dir: %w", err)
	}
	path := filepath.Join(home, ccProjectsDir, projectSlug(req.WorkDir), req.SessionID+".jsonl")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ccstream cleanup: remove %s: %w", path, err)
	}
	return nil
}

// forkTranscript copies src to dst line by line, rewriting each envelope's
// top-level "sessionId" field from oldID to newID. #1432: the rewrite is scoped
// to the exact `"sessionId":"<id>"` byte pattern — NOT a blanket replace of
// oldID anywhere in the line — because oldID can also appear embedded inside
// historical tool-result TEXT (notably an async subagent's `output_file` path,
// which is generated under the *launching* session's own directory). A blanket
// replace rewrote that embedded path to point at the fork's own (wrong, never
// populated) directory instead of preserving the real one — verified live by
// diffing the same task's output_file path across 5 sibling forks of one root
// session, each showing its own (corrupted) directory. dst is created O_EXCL so
// a UUID collision (astronomically unlikely) fails loudly instead of clobbering.
//
// The fork is safe to take WITHOUT quiescing the parent's writer. CC transcripts
// are append-only (earlier bytes never change; new events only extend the file)
// and one-JSON-object-per-line, so we copy only whole, well-formed records and
// stop at the first line that is either unterminated (a half-appended trailing
// record) or not valid JSON (a torn boundary from a non-atomic write). The prefix
// we copy is immutable under append, so it can't tear even if the writer is busy;
// the fork is simply taken as of the last good record, and any in-flight tail is
// not part of it (it lands in the parent, never the branch). This is what lets a
// fork run while the parent has pending background work in flight.
//
// #1431/#2086: while copying, forkTranscript also tracks background tasks — async
// subagents and background shell commands — whose launch never resolves within
// the copied prefix. See appendForkBoundary for why and what's appended.
//
// #2097: that same no-quiesce copy can end in the middle of a FOREGROUND tool
// call — an assistant tool_use whose tool_result the parent has not written yet.
// Before the task closures, forkTranscript writes a synthetic tool_result for
// each such call (appendToolClosures), so the fork's first record after its
// copied history is never a dangling tool_use. See appendToolClosures for why.
func forkTranscript(src, dst, oldID, newID string, lg *log.ComponentLogger) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("ccstream fork: open parent transcript %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("ccstream fork: create branch transcript %s: %w", dst, err)
	}

	// Scoped to the envelope field itself (#1432) — see the function doc above.
	oldB := []byte(`"sessionId":"` + oldID + `"`)
	newB := []byte(`"sessionId":"` + newID + `"`)
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)
	cutBeforeEOF := false
	scan := newForkScan()
	for {
		// ReadBytes (not bufio.Scanner) so multi-hundred-KB tool-result
		// lines aren't truncated by the 64KB scanner token cap.
		line, readErr := r.ReadBytes('\n')
		if len(line) == 0 || line[len(line)-1] != '\n' {
			// No terminating newline: a half-appended trailing record (or EOF
			// with nothing pending). Exclude it — the fork ends at the last
			// complete record above.
			if len(line) > 0 {
				cutBeforeEOF = true
			}
			break
		}
		// A complete line must parse as JSON. If it doesn't, the boundary was
		// torn by a non-atomic write becoming partially visible — stop here and
		// fork as of the previous good record. json.Valid tolerates the trailing
		// newline. (Interior CC records are always valid JSON, so in practice this
		// only ever trips on the tail.)
		if !json.Valid(line) {
			cutBeforeEOF = true
			break
		}
		scan.observe(line)
		if _, werr := w.Write(bytes.ReplaceAll(line, oldB, newB)); werr != nil {
			return finishFork(out, w, dst, fmt.Errorf("ccstream fork: write: %w", werr))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return finishFork(out, w, dst, fmt.Errorf("ccstream fork: read parent: %w", readErr))
		}
	}
	lastUUID, err := appendToolClosures(w, newID, scan.lastUUID, scan.pending)
	if err != nil {
		return finishFork(out, w, dst, err)
	}
	if err := appendForkBoundary(w, newID, lastUUID, scan.open); err != nil {
		return finishFork(out, w, dst, err)
	}
	if err := w.Flush(); err != nil {
		return finishFork(out, w, dst, fmt.Errorf("ccstream fork: flush: %w", err))
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("ccstream fork: close branch transcript: %w", err)
	}
	if cutBeforeEOF {
		// Normal when forking a live session mid-write; logged so a genuinely
		// corrupt interior record (which would also cut the fork short) is visible.
		lg.Debugf("fork: parent %s had an in-flight/partial tail record; branch taken as of the last complete record", src)
	}
	return nil
}

// transcriptEnvelope is the minimal subset of a CC transcript line's fields
// forkTranscript inspects to track open/closed background tasks (#1431, #2086).
// Everything else in a line is opaque to the fork and copied byte-for-byte —
// this is read-only enrichment, never mutation.
type transcriptEnvelope struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	IsSidechain bool   `json:"isSidechain"`
	// ToolUseResult is raw because CC writes a plain STRING here for an errored
	// tool call; decoding it straight into a struct failed the whole line, which
	// also dropped that line's uuid from the synthetic block's parent chain.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Message       *struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// Attachment carries CC's queued_command record: a task-notification that
	// arrived while a turn was running is stored as the attachment's prompt,
	// never as a user message. In a busy session that is where nearly every
	// real completion lives (56 of 57 in one clutch transcript), so reading only
	// user messages left every delegate "open" forever (#2051).
	Attachment *struct {
		Type   string          `json:"type"`
		Prompt json.RawMessage `json:"prompt"`
	} `json:"attachment"`
}

// toolUseResult is the object form of a line's toolUseResult. An async Agent
// launch carries isAsync+status=="async_launched"+agentId; a background shell
// (run_in_background, or a foreground command moved to the background by its
// timeout) carries backgroundTaskId — verified on CC 2.1.280 (#2086).
type toolUseResult struct {
	IsAsync          bool   `json:"isAsync"`
	Status           string `json:"status"`
	AgentID          string `json:"agentId"`
	Description      string `json:"description"`
	BackgroundTaskID string `json:"backgroundTaskId"`
}

// contentBlock is the subset of a message content block the fork scan reads:
// text (task-notifications), tool_use (a Bash call's description; any call still
// awaiting its result) and tool_result (which call a background shell id belongs
// to; which call it answers).
type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	Input     *struct {
		Description string `json:"description"`
	} `json:"input"`
}

// taskNotificationIDPattern extracts task-ids out of a task-notification's
// content string (`<task-notification>\n<task-id>ID</task-id>...`), the same
// tag CC itself uses both for a real completion and for its own stale
// stopped/failed synthesis (#1429) — matched directly against a live transcript.
var taskNotificationIDPattern = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)

type taskKind int

const (
	taskAgent taskKind = iota
	taskShell
)

// openTask is a background task launched in the copied prefix with no
// resolution (yet) in it.
type openTask struct {
	kind taskKind
	desc string
}

// pendingUse is an assistant tool_use in the copied prefix with no tool_result
// (yet) in it (#2097).
type pendingUse struct {
	id            string // tool_use id
	name          string // tool name
	desc          string // input.description, when the tool takes one
	msgID         string // API message id of the assistant turn that made the call
	assistantUUID string // uuid of the transcript line holding the tool_use
}

// forkScan is forkTranscript's running state while copying.
type forkScan struct {
	open     map[string]openTask // task-id -> task; launched, not yet resolved
	bashDesc map[string]string   // Bash tool_use id -> description, until its result line
	pending  []pendingUse        // tool_uses of the current assistant turn still awaiting a result
	lastUUID string              // uuid of the last real (non-synthetic) copied line
}

func newForkScan() *forkScan {
	return &forkScan{open: map[string]openTask{}, bashDesc: map[string]string{}}
}

// observe inspects one already-validated transcript line and updates the scan:
// (a) a background launch — an async Agent (toolUseResult.status==
// "async_launched" with an agentId) or a background shell (toolUseResult.
// backgroundTaskId) — is recorded as open; (b) any task-notification, in a
// user message or a queued_command attachment, resolving a task-id removes it
// from open — it already has a resolution in the copied history, so no
// synthetic close is needed; (c) the line's own uuid (if any) becomes lastUUID,
// so the synthetic block chains off the true last message in the copied prefix.
// Best-effort: an unmarshal failure is silently ignored (the line already passed
// json.Valid — this enrichment is never fork-fatal).
func (s *forkScan) observe(line []byte) {
	var env transcriptEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	if env.UUID != "" {
		s.lastUUID = env.UUID
	}
	var blocks []contentBlock
	if env.Message != nil {
		blocks = s.readContent(env.Message.Content)
		if !env.IsSidechain {
			s.trackToolUses(env, blocks)
		}
	}
	var r toolUseResult
	if len(env.ToolUseResult) > 0 && env.ToolUseResult[0] == '{' && json.Unmarshal(env.ToolUseResult, &r) == nil {
		switch {
		case r.IsAsync && r.Status == "async_launched" && r.AgentID != "":
			s.open[r.AgentID] = openTask{kind: taskAgent, desc: r.Description}
		case r.BackgroundTaskID != "":
			desc := ""
			for _, b := range blocks {
				if b.Type == "tool_result" {
					desc = s.bashDesc[b.ToolUseID]
				}
			}
			s.open[r.BackgroundTaskID] = openTask{kind: taskShell, desc: desc}
		}
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			delete(s.bashDesc, b.ToolUseID)
		}
	}
	if a := env.Attachment; a != nil && a.Type == "queued_command" {
		s.readContent(a.Prompt)
	}
}

// trackToolUses maintains s.pending: the calls of the current assistant turn
// that have no tool_result yet. Only calls the transcript ENDS on are dangling
// in the fork; an orphan further back (CC ran and resumed past it long ago) is
// dropped as soon as the conversation moves on — a new assistant turn (a
// different message id), or a user message that is not a tool result. A result
// for it written at the fork's tail would sit far from its call, and CC already
// carries that history.
//
// Calls of one turn are not all written before the first result: CC runs each
// tool as its block streams in, so a turn's lines interleave as tool_use,
// tool_result, tool_use, tool_result. Hence a new turn is recognised by
// message id, not by "an assistant line after a result".
func (s *forkScan) trackToolUses(env transcriptEnvelope, blocks []contentBlock) {
	switch env.Type {
	case "assistant":
		kept := s.pending[:0]
		for _, p := range s.pending {
			if p.msgID == env.Message.ID {
				kept = append(kept, p)
			}
		}
		s.pending = kept
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID != "" {
				p := pendingUse{id: b.ID, name: b.Name, msgID: env.Message.ID, assistantUUID: env.UUID}
				if b.Input != nil {
					p.desc = b.Input.Description
				}
				s.pending = append(s.pending, p)
			}
		}
	case "user":
		if blocks == nil {
			// String content: a prompt or a task-notification, never a result.
			s.pending = nil
			return
		}
		for _, b := range blocks {
			if b.Type != "tool_result" {
				s.pending = nil
				return
			}
		}
		for _, b := range blocks {
			for i, p := range s.pending {
				if p.id == b.ToolUseID {
					s.pending = append(s.pending[:i], s.pending[i+1:]...)
					break
				}
			}
		}
	}
}

// readContent resolves every task-id named by a task-notification in raw, which
// must be a JSON string (a user message's content, or a queued_command's prompt)
// or an array of content blocks; it also remembers each Bash tool_use's
// description until that call's result line. Tool results are deliberately not
// searched for notifications: a tool that merely PRINTS one (a grep over a
// transcript) is not a completion. Returns the decoded blocks (nil for a string).
func (s *forkScan) readContent(raw json.RawMessage) []contentBlock {
	var texts []string
	var str string
	var blocks []contentBlock
	if json.Unmarshal(raw, &str) == nil {
		texts = append(texts, str)
	} else if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			switch {
			case b.Type == "text":
				texts = append(texts, b.Text)
			case b.Type == "tool_use" && b.Name == "Bash" && b.Input != nil:
				s.bashDesc[b.ID] = b.Input.Description
			}
		}
	}
	for _, t := range texts {
		for _, m := range taskNotificationIDPattern.FindAllStringSubmatch(t, -1) {
			delete(s.open, m[1])
		}
	}
	return blocks
}

// appendToolClosures writes ONE synthetic user message holding a tool_result
// for every tool call the copied prefix ends on without a result (#2097), and
// returns the uuid the next synthetic record must chain off: the closure's own,
// or lastUUID unchanged when there is nothing to close.
//
// Why: the fork is taken without quiescing the parent, so it can land while the
// parent is inside a foreground tool call. That call is the parent's — it is
// still running there, and its result will be written to the parent's
// transcript, never the fork's. Left dangling, the fork's CC answers the call
// itself on resume — "[Request interrupted by user for tool use]" on 2.1.280,
// "outcome is unknown ... check whether it took effect" from 2.1.281 — and the
// fork treats it as its own unfinished work: in live probes on 2.1.280 and
// 2.1.284 (#2097) the fork ran the parent's command a second time on both. The
// closure answers the call with what is true for the fork: it belongs to the
// parent session, its outcome is not visible here, and it must not be re-run;
// CC keeps it as the call's result, and in the same probes no fork re-ran the
// call. It also keeps appendForkBoundary's task-notification from being the
// record directly after a tool_use: the API requires a tool_use be answered by
// tool_results in the very next user message.
//
// The record mirrors CC's own tool-result lines: parentUuid and
// sourceToolAssistantUUID name the assistant line of the last open call (CC
// parents a result on its call's line, not on hook attachments written in
// between), and toolUseResult is a plain string, which is what CC writes for a
// call that produced no structured result, so stoprule's reader skips it.
// is_error is false: the call did not fail, it is simply not this fork's.
func appendToolClosures(w *bufio.Writer, sessionID, lastUUID string, pending []pendingUse) (string, error) {
	if len(pending) == 0 {
		return lastUUID, nil
	}
	blocks := make([]map[string]any, 0, len(pending))
	texts := make([]string, 0, len(pending))
	for _, p := range pending {
		text := toolClosureText(p)
		texts = append(texts, text)
		blocks = append(blocks, map[string]any{
			"tool_use_id": p.id,
			"type":        "tool_result",
			"content":     text,
			"is_error":    false,
		})
	}
	source := pending[len(pending)-1].assistantUUID
	var parentUUID any
	switch {
	case source != "":
		parentUUID = source
	case lastUUID != "":
		parentUUID = lastUUID
	}
	id := uuid.NewString()
	rec := map[string]any{
		"parentUuid":  parentUUID,
		"isSidechain": false,
		"promptId":    uuid.NewString(),
		"type":        "user",
		"message": map[string]any{
			"role":    "user",
			"content": blocks,
		},
		"uuid":                    id,
		"timestamp":               time.Now().UTC().Format(time.RFC3339Nano),
		"toolUseResult":           strings.Join(texts, "\n"),
		"sourceToolAssistantUUID": source,
		"sessionId":               sessionID,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("ccstream fork: marshal fork-boundary tool closures: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return "", fmt.Errorf("ccstream fork: write fork-boundary tool closures: %w", err)
	}
	return id, nil
}

// toolClosureText is the tool_result the fork's model reads for one call still
// running in the parent.
func toolClosureText(p pendingUse) string {
	name := p.name
	if name == "" {
		name = "tool"
	}
	call := fmt.Sprintf("This %s call", name)
	if p.desc != "" {
		call += fmt.Sprintf(" (%q)", p.desc)
	}
	return "[fork boundary] " + call + " belongs to the parent session: it was still running there when this fork was taken, and its result is not visible to this fork. It was NOT interrupted, stopped or failed. This is a synthetic result inserted at fork time. Do not re-run it from this branch; the parent session is carrying it out."
}

// appendForkBoundary writes ONE synthetic user message holding a
// <task-notification> block for every task still open after the copy —
// background subagents and background shell commands launched in the parent
// whose resolution is not in the copied prefix.
//
// Why: the real completion (if any) of such a task lands only in the PARENT
// session's future — a fork never receives it. Left alone, Claude Code's own
// resume-time reconciliation finds a launch in history with no resolution and
// tells the fork the task "didn't finish before the previous session ended":
// status=failed for an agent (#1429) and status=stopped for a shell (#2086, where
// a reflection branch read its parent's still-running `make land` chains as
// killed). Writing an explicit closure before CC ever resumes the transcript
// means CC finds the task already resolved and has nothing to reconcile.
//
// What CC needs, measured on 2.1.280 (#2086, clutch notes/2086.md): a
// task-notification naming the task-id AND carrying a <status> element. With no
// <status> CC still emits its orphan notice; with one, any value suppressed it
// (completed and running both did). "completed" is used because it is the value
// CC itself treats as terminal, so a future CC that checks the value still reads
// it as finished. The <summary> is what the model reads, and it deliberately
// claims no outcome and fabricates no <result>: the task belongs to the parent
// and its outcome is unknown to the fork.
//
// One message, not one per task (#2051): a fork refreshes the parent's cache
// entry only if everything after the parent's history fits in the API's
// ~20-content-block look-back, and 21+ per-task messages put it out of reach.
// CC resolves every block in the one message — verified live with two shells
// and one agent batched.
func appendForkBoundary(w *bufio.Writer, sessionID, lastUUID string, open map[string]openTask) error {
	if len(open) == 0 {
		return nil
	}
	// Deterministic order (map iteration isn't) for reproducible output/tests.
	ids := make([]string, 0, len(open))
	for id := range open {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	notes := make([]string, 0, len(ids))
	for _, id := range ids {
		notes = append(notes, "<task-notification>\n"+
			"<task-id>"+id+"</task-id>\n"+
			"<status>completed</status>\n"+
			"<summary>"+forkBoundarySummary(id, open[id])+"</summary>\n"+
			"</task-notification>")
	}

	var parentUUID any
	if lastUUID != "" {
		parentUUID = lastUUID
	}
	rec := map[string]any{
		"parentUuid":  parentUUID,
		"isSidechain": false,
		"promptId":    uuid.NewString(),
		"type":        "user",
		"message": map[string]any{
			"role":    "user",
			"content": strings.Join(notes, "\n"),
		},
		"uuid":      uuid.NewString(),
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"sessionId": sessionID,
		"origin":    map[string]any{"kind": "task-notification"},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ccstream fork: marshal fork-boundary closures: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("ccstream fork: write fork-boundary closures: %w", err)
	}
	return nil
}

// forkBoundarySummary is the text the fork's model reads for one open task.
func forkBoundarySummary(id string, t openTask) string {
	if t.kind == taskShell {
		name := "Background command"
		if t.desc != "" {
			name += fmt.Sprintf(" %q", t.desc)
		}
		return fmt.Sprintf("[fork boundary] %s (task-id %s) was still running in the parent session when this fork was taken; its outcome is not visible to this fork. It belongs to the parent session and was NOT stopped or killed. This is a synthetic closure inserted at fork time so no false stopped notification is raised for it here. Do not re-run it from this branch; its output file, named in its launch result above, shows its progress.",
			name, id)
	}
	return fmt.Sprintf("[fork boundary] Background agent %q (task-id %s) was still open when this session was forked from its parent; it is NOT owned by this branch — the original session may still be running it, or it may already be done there. This is a synthetic closure inserted at fork time so no stale stopped/failed notification is raised for it here. Do not re-dispatch it from this branch; if you need its real status, check its worktree/output directly.",
		t.desc, id)
}

// finishFork closes the partial output and removes it on error, so a failed
// fork never leaves a half-written transcript CC might try to resume. The
// cleanup itself is best-effort — the original cause is what matters.
func finishFork(out *os.File, _ *bufio.Writer, dst string, cause error) error {
	_ = out.Close()
	_ = os.Remove(dst)
	return cause
}
