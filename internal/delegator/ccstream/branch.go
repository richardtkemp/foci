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
// #1431: while copying, forkTranscript also tracks background (isAsync) subagent
// launches that never resolve within the copied prefix — see
// appendSyntheticTaskEnds for why and what's appended.
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
	openTasks := map[string]string{} // agentId -> description; still-open async launches (#1431)
	var lastUUID string              // uuid of the last real (non-synthetic) copied line
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
		observeForSyntheticEnds(line, openTasks, &lastUUID)
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
	if err := appendSyntheticTaskEnds(w, newID, lastUUID, openTasks); err != nil {
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
// forkTranscript inspects to track open/closed background subagent tasks (#1431).
// Everything else in a line is opaque to the fork and copied byte-for-byte —
// this is read-only enrichment, never mutation.
type transcriptEnvelope struct {
	UUID          string `json:"uuid"`
	ToolUseResult *struct {
		IsAsync     bool   `json:"isAsync"`
		Status      string `json:"status"`
		AgentID     string `json:"agentId"`
		Description string `json:"description"`
	} `json:"toolUseResult"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// taskNotificationIDPattern extracts task-ids out of a task-notification's
// content string (`<task-notification>\n<task-id>ID</task-id>...`), the same
// tag CC itself uses both for a real completion and for its own stale
// stopped/failed synthesis (#1429) — matched directly against a live transcript.
var taskNotificationIDPattern = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)

// observeForSyntheticEnds inspects one already-validated transcript line and
// updates the fork's running state: (a) a background subagent launch
// (toolUseResult.status=="async_launched", carrying an agentId) is recorded into
// openTasks; (b) any task-notification content resolving a task-id removes it
// from openTasks — it already has a resolution in the copied history, no
// synthetic close is needed; (c) the line's own uuid (if any) becomes the new
// lastUUID, so a synthetic close can chain off the true last message in the
// copied prefix. Best-effort: an unmarshal failure is silently ignored (the line
// already passed json.Valid — this enrichment is never fork-fatal).
func observeForSyntheticEnds(line []byte, openTasks map[string]string, lastUUID *string) {
	var env transcriptEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	if env.UUID != "" {
		*lastUUID = env.UUID
	}
	if r := env.ToolUseResult; r != nil && r.IsAsync && r.Status == "async_launched" && r.AgentID != "" {
		openTasks[r.AgentID] = r.Description
	}
	if env.Message != nil {
		var content string
		if json.Unmarshal(env.Message.Content, &content) == nil {
			for _, m := range taskNotificationIDPattern.FindAllStringSubmatch(content, -1) {
				delete(openTasks, m[1])
			}
		}
	}
}

// appendSyntheticTaskEnds writes one synthetic, already-resolved
// <task-notification> line per entry remaining in openTasks after the copy —
// background subagent launches that never resolved within the copied prefix.
//
// Why: the real completion (if any) of such a task lands only in the PARENT
// session's future — a fork never receives it. Left alone, Claude Code's own
// resume-time reconciliation finds a launch in history with no resolution and
// concludes the task was stopped/failed by "the previous process" (verified
// live, #1429 — CC injected exactly this at the top of a forked session's first
// resume, timestamped ~1s after the fork's own `--resume`). Synthesizing an
// explicit closure here, before CC ever resumes the transcript, means CC finds
// the task already resolved and has nothing to reconcile.
//
// The synthesized record deliberately does NOT claim the real work succeeded or
// fabricate a <result> for it (the true outcome is unknown to the fork) — its
// <summary> says plainly that this is a fork-boundary artifact, the task belongs
// to the original session, and the branch must not re-dispatch it or trust any
// implied result. Whether this is sufficient to suppress CC's OWN synthesis is
// externally verified against live CC (see notes-1431.md's probe), not just
// asserted here — CC's detector is closed-source, so this is empirical, not
// something a unit test alone can confirm.
//
// Chained sequentially off lastUUID (the last real copied line's uuid, or the
// previous synthetic close's uuid for the second and later entries) — one
// linear thread, not siblings fanned off the same parent, matching how every
// other message in a CC transcript links to its predecessor.
func appendSyntheticTaskEnds(w *bufio.Writer, sessionID, lastUUID string, openTasks map[string]string) error {
	if len(openTasks) == 0 {
		return nil
	}
	// Deterministic order (map iteration isn't) for reproducible output/tests.
	ids := make([]string, 0, len(openTasks))
	for id := range openTasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range ids {
		newUUID := uuid.NewString()
		summary := fmt.Sprintf("[fork boundary] Background agent %q (task-id %s) was still open when this session was forked from its parent; it is NOT owned by this branch — the original session may still be running it, or it may already be done there. This is a synthetic closure inserted at fork time so no stale stopped/failed notification is raised for it here. Do not re-dispatch it from this branch; if you need its real status, check its worktree/output directly.",
			openTasks[id], id)
		content := "<task-notification>\n" +
			"<task-id>" + id + "</task-id>\n" +
			"<status>completed</status>\n" +
			"<summary>" + summary + "</summary>\n" +
			"</task-notification>"

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
				"content": content,
			},
			"uuid":      newUUID,
			"timestamp": now,
			"sessionId": sessionID,
			"origin":    map[string]any{"kind": "task-notification"},
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("ccstream fork: marshal synthetic task end for %s: %w", id, err)
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return fmt.Errorf("ccstream fork: write synthetic task end: %w", err)
		}
		lastUUID = newUUID
	}
	return nil
}

// finishFork closes the partial output and removes it on error, so a failed
// fork never leaves a half-written transcript CC might try to resume. The
// cleanup itself is best-effort — the original cause is what matters.
func finishFork(out *os.File, _ *bufio.Writer, dst string, cause error) error {
	_ = out.Close()
	_ = os.Remove(dst)
	return cause
}
