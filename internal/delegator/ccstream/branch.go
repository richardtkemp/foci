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

// projectSlug converts a workspace path to Claude Code's project directory
// name, e.g. "/home/foci/clutch" → "-home-foci-clutch". (Mirrors the same
// mapping in the cctmux backend; kept local to avoid a cross-package
// dependency for a one-line transform.)
func projectSlug(path string) string {
	return strings.ReplaceAll(path, "/", "-")
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
	if err := appendForkBoundary(w, newID, scan.lastUUID, scan.open); err != nil {
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
	UUID string `json:"uuid"`
	// ToolUseResult is raw because CC writes a plain STRING here for an errored
	// tool call; decoding it straight into a struct failed the whole line, which
	// also dropped that line's uuid from the synthetic block's parent chain.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Message       *struct {
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
// text (task-notifications), tool_use (a Bash call's description) and
// tool_result (which call a background shell id belongs to).
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

// forkScan is forkTranscript's running state while copying.
type forkScan struct {
	open     map[string]openTask // task-id -> task; launched, not yet resolved
	bashDesc map[string]string   // Bash tool_use id -> description, until its result line
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
