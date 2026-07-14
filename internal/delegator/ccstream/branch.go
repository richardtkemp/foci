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
	"strings"

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

	if err := forkTranscript(parentPath, newPath, req.ParentSessionID, newID); err != nil {
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

// forkTranscript copies src to dst line by line, replacing every occurrence of
// oldID with newID. Because session UUIDs are globally unique strings and the
// per-message uuid/parentUuid fields hold DIFFERENT values, a plain per-line
// replace rewrites only the "sessionId" fields — the same technique verified
// manually (cp + sed) before this was written. dst is created O_EXCL so a UUID
// collision (astronomically unlikely) fails loudly instead of clobbering.
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
func forkTranscript(src, dst, oldID, newID string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("ccstream fork: open parent transcript %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("ccstream fork: create branch transcript %s: %w", dst, err)
	}

	oldB, newB := []byte(oldID), []byte(newID)
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)
	cutBeforeEOF := false
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
		log.Debugf("ccstream", "fork: parent %s had an in-flight/partial tail record; branch taken as of the last complete record", src)
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
