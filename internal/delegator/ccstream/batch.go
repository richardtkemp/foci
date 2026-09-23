package ccstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"foci/internal/delegator"
	"foci/internal/procx"
	"foci/internal/tempdir"
)

// createMemfdSystemPromptFn is a package-level indirection over
// createMemfdSystemPrompt so tests can force the fallback path (a memfd
// failure — resource exhaustion, a sandboxed/non-Linux runtime) without
// depending on actually exhausting a real system resource. Mirrors
// opencode/batch.go's acquireServerFn.
var createMemfdSystemPromptFn = createMemfdSystemPrompt

// RunBatch implements delegator.BatchRunner: a one-shot `claude --print`
// invocation reusing the parent process's subscription auth (OAuth), so the
// call charges mana rather than separate API spend. Runs on an unstarted
// Backend instance — only cfg (binary override) is consulted.
//
// --no-session-persistence avoids leaving a JSONL file behind; --system-prompt-file
// (when set) replaces the default system prompt, which also skips CLAUDE.md
// auto-discovery and the dynamic cwd/env/git sections. Do NOT add --bare: it
// disables OAuth and forces ANTHROPIC_API_KEY, defeating subscription auth.
//
// The system prompt is NEVER passed as a literal --system-prompt argv
// element (#1963). A composed character system prompt is caller-controlled
// and unbounded — coach's alone is 131,575 chars and growing — while Linux
// caps a single execve() argv element at MAX_ARG_STRLEN (32 pages = 131072
// bytes), independent of the total-argv limit and NOT raised by ulimit.
// Crossing it fails fork/exec with E2BIG ("argument list too long") before
// claude ever starts, which retries cannot fix since every attempt hits the
// same cliff.
//
// Preferred delivery is a Linux memfd (createMemfdSystemPrompt): an
// anonymous, in-memory file with no filesystem entry, passed to the child
// via cmd.ExtraFiles and --system-prompt-file /dev/fd/<N>. Chosen over a
// named pipe/fifo (Dick, 2026-09-23) for two reasons a probe against the
// real claude binary confirmed empirically: (1) a fifo's open-for-write
// BLOCKS until a reader opens it — if claude fails to start, dies early, or
// never opens the path, foci's writer hangs; the memfd is written and
// seeked to 0 BEFORE exec, so nothing can block. (2) a fifo is a filesystem
// entry (mkfifo) that a crash leaves behind, same cleanup problem as the
// temp file it replaces; a memfd has no path to leak. The probe (strace on
// the real binary) also showed claude reads a system-prompt-file via a
// plain openat + read-loop-to-EOF with no fstat/lseek, which is exactly why
// a streamed, non-seekable descriptor works at all — cctmux's Start uses
// the on-disk file form for the same underlying reason as historically used
// here (see cctmux/lifecycle.go) but batch runs, being one-shot and fully
// buffered in memory already (req.SystemPrompt is a Go string), have
// nothing to gain from ever touching disk.
//
// memfd_create is Linux-only; createMemfdSystemPrompt returns an error on
// any other platform (memfd_other.go) or if the syscall itself fails, and
// RunBatch falls back to the pre-memfd temp-file path (writeSystemPromptFile)
// rather than failing the batch.
func (b *Backend) RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error) {
	model := req.Model
	if model == "" {
		model = "sonnet"
	}
	args := []string{
		"--print",
		"--dangerously-skip-permissions",
		"--no-session-persistence",
		"--model", model,
	}

	// promptFD (memfd, preferred) and promptCleanup (temp-file fallback) are
	// mutually exclusive: at most one is set, whichever path SystemPrompt
	// took below.
	var promptFD *os.File
	var promptCleanup func()
	if req.SystemPrompt != "" {
		if fd, err := createMemfdSystemPromptFn(req.SystemPrompt); err == nil {
			promptFD = fd
		} else {
			b.logger().Debugf("memfd system prompt unavailable (%v), falling back to temp file", err)
			promptFile, cleanup, ferr := writeSystemPromptFile(req.SystemPrompt)
			if ferr != nil {
				return "", fmt.Errorf("write system prompt file: %w", ferr)
			}
			promptCleanup = cleanup
			args = append(args, "--system-prompt-file", promptFile)
		}
	}
	if promptFD != nil {
		// Closed only after the process has exited (this defer runs when
		// RunBatch returns, which is after RunWithETXTBSYRetry's blocking
		// cmd.Run() below has completed) — never before Start.
		defer func() { _ = promptFD.Close() }()
	}
	if promptCleanup != nil {
		defer promptCleanup()
	}

	// RunWithETXTBSYRetry rebuilds the Cmd (fresh stdin reader, reset buffers)
	// each attempt to survive a transient "text file busy" from concurrent
	// fork/exec (golang/go#22315). Safe to retry: `claude --print` has no
	// observable side effects until the child actually execs.
	var stdout, stderr bytes.Buffer
	err := procx.RunWithETXTBSYRetry(ctx, func() *exec.Cmd {
		stdout.Reset()
		stderr.Reset()
		// Operator: a Claude Code process — the canonical agent population.
		cmd := procx.Spawn(ctx, procx.Operator, b.resolveBinary(), args...)
		cmd.Dir = req.WorkDir
		cmd.Stdin = strings.NewReader(req.Prompt)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if promptFD != nil {
			// Seek back to the start on every attempt: a retry reuses the
			// same open file description, and although
			// RunWithETXTBSYRetry only retries when Start() itself failed
			// (before the child could ever read), resetting unconditionally
			// costs nothing and removes any dependency on that contract.
			if _, serr := promptFD.Seek(0, io.SeekStart); serr != nil {
				b.logger().Warnf("seek memfd system prompt: %v", serr)
			}
			// cmd.ExtraFiles[i] becomes fd 3+i in the child — compute N
			// from the actual index rather than assuming 3, in case
			// anything else ever populates ExtraFiles ahead of this.
			idx := len(cmd.ExtraFiles)
			cmd.ExtraFiles = append(cmd.ExtraFiles, promptFD)
			cmd.Args = append(cmd.Args, "--system-prompt-file", fmt.Sprintf("/dev/fd/%d", 3+idx))
		}
		return cmd
	})
	if err != nil {
		if d := procx.FailureDetail(stderr.String(), stdout.String()); d != "" {
			return "", fmt.Errorf("claude --print failed: %w (%s)", err, d)
		}
		return "", fmt.Errorf("claude --print failed: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// writeSystemPromptFile writes prompt to a private (0600) temp file under
// foci's temp root and returns its path plus a cleanup func the caller
// should defer. Keeping this out of the caller's WorkDir (which for RunBatch
// is often a persistent agent workspace) means concurrent/overlapping batch
// runs never collide on the same filename.
func writeSystemPromptFile(prompt string) (path string, cleanup func(), err error) {
	f, err := tempdir.Create("system-prompt-*")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
