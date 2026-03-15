package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"foci/internal/log"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
)

// defaultSpillThreshold is the default byte count kept in memory before
// overflowing shell output to a temp file. Matches the default MaxResultChars
// (15K chars ≈ 15KB). Commands producing more output spill to disk
// automatically, avoiding 100MB+ allocations in RAM.
const defaultSpillThreshold = 15000

// sleepRegexp matches commands that start with "sleep" (case-insensitive).
// This blocks bare sleep commands which block for up to 10s then silently
// background — the worst of both worlds.
var sleepRegexp = regexp.MustCompile(`(?i)^\s*sleep\s+`)

// secretTemplateRe matches {{secret:NAME}} templates (same as secrets.FindSecretRefs).
var secretTemplateRe = regexp.MustCompile(`\{\{secret:([a-zA-Z0-9_.\-]+)\}\}`)

// cmdSeparatorRe matches shell command separators: |, ;, &&, ||
// These mark the boundary where a foci_http_request invocation ends and a new
// command begins. Secret refs after a separator are NOT in http_request scope.
var cmdSeparatorRe = regexp.MustCompile(`\|{1,2}|;|&&`)

// execShell is the shell binary used by exec. Prefer bash (needed for pipefail
// and tool-piping shell functions); fall back to sh if bash is not installed.
var execShell = sync.OnceValue(func() string {
	if path, err := exec.LookPath("bash"); err == nil {
		log.Debugf("exec", "using bash: %s", path)
		return "bash"
	}
	log.Infof("exec", "bash not found, falling back to sh (pipefail/failglob unavailable)")
	return "sh"
})

// hasBash reports whether the exec shell is bash.
func hasBash() bool { return execShell() == "bash" }

// execPreamble returns the shell option preamble for exec commands.
// bash gets pipefail, nounset, and failglob; sh gets nounset only.
func execPreamble() string {
	if hasBash() {
		return "set -o pipefail -o nounset; shopt -s failglob"
	}
	return "set -o nounset"
}

// NewExecTool creates an exec tool. If store is non-nil, commands get
// secret template resolution, output redaction, and blocked path checks.
// autoBackgroundSecs is the threshold after which a running command is
// auto-backgrounded (0 disables). notifier delivers results when an
// auto-backgrounded command finishes (nil disables).
// workDir sets the default working directory for commands (empty = process cwd).
// registry enables the exec bridge (tool piping) — if non-nil, exported tools
// are available as shell functions inside exec commands.
// spillThreshold is the byte count kept in memory before overflowing to disk
// (0 = use default 15000). spillTempDir is the directory for overflow files.
func NewExecTool(store *secrets.Store, bwStore *bitwarden.Store, autoBackgroundSecs int, notifier *AsyncNotifier, workDir string, registry *Registry, spillThreshold int, spillTempDir string) *Tool {
	st := int64(spillThreshold)
	if st <= 0 {
		st = defaultSpillThreshold
	}
	return &Tool{
		Name:        "shell",
		Description: "Run a shell command and return its output. pipefail, nounset, and failglob are set. Use timeout to set a generous limit on execution time, or set background=true for persistent processes (tmux, daemons) that should survive after the call. {{secret:}} templates may be used but only inside foci_http_request arguments. Most tools are available as foci_$toolname shell functions. You will find foci_send_message_to_user and foci_summarize especially useful.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {
					"type": "string",
					"description": "Shell command to execute."
				},
				"timeout": {
					"type": "integer",
					"description": "Timeout in seconds (default 30)"
				},
				"background": {
					"type": "boolean",
					"description": "If true, child processes survive after the command exits (for tmux, daemons, etc.)"
				},
				"output_mode": {
					"type": "string",
					"enum": ["combined", "separated"],
					"description": "Output mode: combined (default) merges stdout/stderr; separated returns JSON with stdout, stderr, and exit_code fields"
				}
			},
			"required": ["command"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return execCommand(ctx, params, store, bwStore, autoBackgroundSecs, notifier, workDir, registry, st, spillTempDir)
		},
	}
}

func execCommand(ctx context.Context, params json.RawMessage, store *secrets.Store, bwStore *bitwarden.Store, autoBackgroundSecs int, notifier *AsyncNotifier, workDir string, registry *Registry, spillThreshold int64, spillTempDir string) (ToolResult, error) {
	var p struct {
		Command    string `json:"command"`
		Timeout    int    `json:"timeout"`
		Background bool   `json:"background"`
		OutputMode string `json:"output_mode"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	// Check blocked paths
	if store != nil && store.IsBlockedCommand(p.Command) {
		return ToolResult{}, fmt.Errorf("command references a blocked path")
	}

	// Block bare sleep commands - they block for up to 10s then silently
	// background, which is the worst of both worlds. Use remind instead.
	if sleepRegexp.MatchString(p.Command) {
		return ToolResult{}, fmt.Errorf("sleep is not allowed via exec — use remind for timed check-ins instead")
	}

	// Block regular secret templates — secrets must not reach child processes.
	// Bitwarden secrets (bw.*) are allowed because they're approval-gated via aisudo.
	// Secret refs inside foci_http_request args are also allowed — the template
	// is passed as a literal string to the tool server which resolves it safely.
	cmd := p.Command
	if refs := secrets.FindSecretRefs(cmd); refs != nil {
		for _, ref := range refs {
			if !bitwarden.IsBitwardenRef(ref) && !allSecretRefsInHTTPRequestScope(cmd) {
				return ToolResult{}, fmt.Errorf("{{secret:}} templates are not allowed in exec — use the http_request tool or foci_http_request shell function instead")
			}
		}
	}
	// Resolve bitwarden secret templates (approval-gated, safe for exec)
	if bwStore != nil {
		resolved, err := bwStore.Resolve(cmd)
		if err != nil {
			return ToolResult{}, fmt.Errorf("resolve bitwarden secrets: %w", err)
		}
		cmd = resolved
	}

	timeout := ResolveTimeout(p.Timeout, TimeoutConfig{DefaultSec: 30})

	log.Debugf("exec", "session=%s running: %s (timeout=%s background=%v)", SessionKeyFromContext(ctx), truncateCmd(p.Command, 200), timeout, p.Background)

	// For explicit background mode, use the original direct approach (no bridge)
	if p.Background {
		return execDirect(ctx, cmd, p.Command, timeout, true, store, bwStore, workDir, nil, p.OutputMode, spillThreshold, spillTempDir)
	}

	// Auto-background: if threshold is set and notifier is available,
	// start the command and wait with a timer
	if autoBackgroundSecs > 0 && notifier != nil {
		sk := SessionKeyFromContext(ctx)
		return execWithAutoBackground(ctx, cmd, p.Command, timeout, store, bwStore, autoBackgroundSecs, notifier, sk, workDir, registry, p.OutputMode, spillThreshold, spillTempDir)
	}

	return execDirect(ctx, cmd, p.Command, timeout, false, store, bwStore, workDir, registry, p.OutputMode, spillThreshold, spillTempDir)
}

// execDirect runs a command and waits for completion (original behavior).
func execDirect(ctx context.Context, cmd, displayCmd string, timeout time.Duration, background bool, store *secrets.Store, bwStore *bitwarden.Store, workDir string, registry *Registry, outputMode string, spillThreshold int64, spillTempDir string) (ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Create exec bridge for tool piping (skipped for background mode and nil registry)
	var bridge *ExecBridge
	if !background && registry != nil {
		var err error
		bridge, err = NewExecBridge(registry, ctx)
		if err != nil {
			log.Debugf("exec", "session=%s exec bridge creation failed (continuing without): %v", SessionKeyFromContext(ctx), err)
		} else {
			defer bridge.Close()
			cmd = fmt.Sprintf("%s; source %s; %s", execPreamble(), bridge.FuncsPath(), cmd)
		}
	}

	proc := exec.CommandContext(ctx, execShell(), "-c", cmd)
	proc.Dir = workDir

	// Inject FOCI_SOCK for exec bridge
	if bridge != nil {
		proc.Env = append(os.Environ(), "FOCI_SOCK="+bridge.SockPath())
	}

	if background {
		proc.SysProcAttr = ChildSysProcAttrSetsid()
		proc.WaitDelay = 2 * time.Second
	} else {
		proc.SysProcAttr = ChildSysProcAttr()
		proc.Cancel = func() error {
			return syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
		}
	}

	// Use pipes with LimitReader to cap memory usage (Bug #115)
	stdout, err := proc.StdoutPipe()
	if err != nil {
		return ToolResult{}, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderr, err := proc.StderrPipe()
	if err != nil {
		return ToolResult{}, fmt.Errorf("create stderr pipe: %w", err)
	}

	if err := proc.Start(); err != nil {
		return ToolResult{}, fmt.Errorf("start command: %w", err)
	}

	// Read stdout and stderr concurrently with process exit.
	stdoutSpill, stderrSpill, combinedSpill, doneRead := startPipeReaders(stdout, stderr, outputMode, spillThreshold, spillTempDir)

	// Wait for process exit concurrently. If the process exits but pipe
	// reads don't complete promptly (a leaked child holds the pipe FDs
	// open), kill the entire process group to unblock the readers.
	err = waitAndReap(proc, doneRead, stdout, stderr)

	if outputMode == "separated" {
		result := TextResult(formatSeparatedResult(stdoutSpill.String(), stderrSpill.String(), err, store, bwStore))
		applySpillResult(&result, stdoutSpill, stderrSpill, nil)
		return result, nil
	}
	result := TextResult(formatResult(combinedSpill.String(), err, ctx, timeout, displayCmd, store, bwStore))
	applySpillResult(&result, nil, nil, combinedSpill)
	return result, nil
}

// execWithAutoBackground starts a command and returns early if it exceeds the threshold.
// The command continues running and results are delivered via notifier to the originating session.
func execWithAutoBackground(ctx context.Context, cmd, displayCmd string, timeout time.Duration, store *secrets.Store, bwStore *bitwarden.Store, thresholdSecs int, notifier *AsyncNotifier, sessionKey, workDir string, registry *Registry, outputMode string, spillThreshold int64, spillTempDir string) (ToolResult, error) {
	// Use a separate context for tracking (not for killing the process)
	cmdCtx, cmdCancel := context.WithTimeout(context.Background(), timeout)

	// Create exec bridge for tool piping.
	// Use context.Background() + session key so bridge survives agent turn end.
	var bridge *ExecBridge
	if registry != nil {
		bridgeCtx := WithSessionKey(context.Background(), sessionKey)
		var err error
		bridge, err = NewExecBridge(registry, bridgeCtx)
		if err != nil {
			log.Debugf("exec", "session=%s exec bridge creation failed (continuing without): %v", sessionKey, err)
		} else {
			cmd = fmt.Sprintf("%s; source %s; %s", execPreamble(), bridge.FuncsPath(), cmd)
		}
	}

	// Use exec.Command (not CommandContext) so timeout doesn't kill the process.
	// Auto-backgrounded commands should run to completion regardless of timeout.
	proc := exec.Command(execShell(), "-c", cmd)
	proc.Dir = workDir

	// Inject FOCI_SOCK for exec bridge
	if bridge != nil {
		proc.Env = append(os.Environ(), "FOCI_SOCK="+bridge.SockPath())
	}
	proc.SysProcAttr = ChildSysProcAttr()
	// Don't set proc.Cancel — let the command run to completion

	// Use pipes with LimitReader to cap memory usage (Bug #115)
	stdout, err := proc.StdoutPipe()
	if err != nil {
		cmdCancel()
		return ToolResult{}, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderr, err := proc.StderrPipe()
	if err != nil {
		cmdCancel()
		return ToolResult{}, fmt.Errorf("create stderr pipe: %w", err)
	}

	if err := proc.Start(); err != nil {
		cmdCancel()
		return ToolResult{}, fmt.Errorf("start command: %w", err)
	}

	// Read stdout and stderr concurrently with process exit.
	stdoutSpill, stderrSpill, combinedSpill, doneRead := startPipeReaders(stdout, stderr, outputMode, spillThreshold, spillTempDir)

	// Wait for process exit and reap leaked children (same as execDirect).
	var cmdErr error
	signal := make(chan struct{})
	go func() {
		cmdErr = waitAndReap(proc, doneRead, stdout, stderr)
		close(signal)
	}()

	formatExecResult := func() ToolResult {
		if outputMode == "separated" {
			result := TextResult(formatSeparatedResult(stdoutSpill.String(), stderrSpill.String(), cmdErr, store, bwStore))
			applySpillResult(&result, stdoutSpill, stderrSpill, nil)
			return result
		}
		result := TextResult(formatResult(combinedSpill.String(), cmdErr, cmdCtx, timeout, displayCmd, store, bwStore))
		applySpillResult(&result, nil, nil, combinedSpill)
		return result
	}

	return RunInBackground(ctx, BackgroundParams{
		SessionKey:    sessionKey,
		Notifier:      notifier,
		ThresholdSecs: thresholdSecs,
		Done:          signal,
		SyncResult: func() (ToolResult, error) {
			cmdCancel()
			if bridge != nil {
				bridge.Close()
			}
			return formatExecResult(), nil
		},
		NotifyMessage: func() string {
			result := formatExecResult()
			return fmt.Sprintf("[EXEC RESULT] Command completed:\n$ %s\n\n%s", displayCmd, result.Text)
		},
		Cleanup: func() {
			cmdCancel()
			if bridge != nil {
				bridge.Close()
			}
			stdoutSpill.Cleanup()
			stderrSpill.Cleanup()
			combinedSpill.Cleanup()
		},
		PendingResult:  TextResult(fmt.Sprintf("Command still running (exceeded %ds threshold). Results will be delivered when complete.\n$ %s", thresholdSecs, displayCmd)),
		NotifyOnCancel: true,
	})
}

// pipeReapGrace is how long to wait for pipe readers after the main process
// exits before killing the process group. Leaked child processes that inherit
// pipe FDs keep the write-ends open, blocking io.Copy indefinitely.
const pipeReapGrace = 2 * time.Second

// waitAndReap waits for both the process to exit and all pipe reads to
// complete. It uses Process.Wait() (low-level, does NOT close pipes) to
// detect process exit independently from Cmd.Wait(). This avoids the
// data race where Cmd.Wait() closes pipe read-ends while io.Copy is
// still reading.
//
// Normal case: pipe reads complete when all write-end holders exit (fast).
// Leaked child case: process exits but a child holds pipe FDs open. After
// pipeReapGrace, the entire process group is killed to close the write-ends,
// letting io.Copy drain remaining data and return EOF.
func waitAndReap(proc *exec.Cmd, doneRead <-chan struct{}, stdout, stderr io.ReadCloser) error {
	// Detect process exit via low-level Process.Wait (doesn't close pipes).
	exitCh := make(chan error, 1)
	go func() {
		state, waitErr := proc.Process.Wait()
		exitCh <- processExitError(state, waitErr)
	}()

	var err error
	select {
	case <-doneRead:
		// Pipe reads completed first (normal fast path).
		err = <-exitCh
	case err = <-exitCh:
		// Process exited but pipe reads still pending — a leaked child
		// is holding pipe FDs open. Give a short grace, then kill the
		// process group to close the write-ends and unblock the readers.
		select {
		case <-doneRead:
		case <-time.After(pipeReapGrace):
			log.Debugf("exec", "pipe reads stuck after process exit — killing process group %d", proc.Process.Pid)
			_ = syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
			<-doneRead
		}
	}

	// Close pipe read-ends after all reads are complete (Cmd.Wait is not
	// called because Process.Wait already reaped the process, and Cmd.Wait
	// would return ErrProcessDone without doing its cleanup).
	_ = stdout.Close()
	_ = stderr.Close()
	return err
}

// processExitError converts a ProcessState into the error that Cmd.Wait
// would return: nil for success, *exec.ExitError for non-zero exit.
func processExitError(state *os.ProcessState, waitErr error) error {
	if waitErr != nil {
		return waitErr
	}
	if !state.Success() {
		return &exec.ExitError{ProcessState: state}
	}
	return nil
}

// startPipeReaders launches goroutines to read stdout and stderr concurrently.
// All reads must complete before proc.Wait is called (Go 1.22+ closes pipe
// read-ends in Wait, racing in-progress reads).
//
// For "separated" mode, stdout and stderr are captured into independent spillWriters.
// For combined mode, both streams write through a single spillWriter (mutex-protected)
// so output appears in roughly chronological order (not all-stdout-then-all-stderr).
//
// spillThreshold controls how many bytes are kept in memory before overflowing to
// a temp file. spillTempDir is the directory for overflow files.
//
// Returns (stdoutSpill, stderrSpill, combinedSpill, doneRead). Use stdout/stderr
// for separated mode; use combined for combined mode. doneRead is closed when
// all reads are finished.
func startPipeReaders(stdout, stderr io.ReadCloser, outputMode string, spillThreshold int64, spillTempDir string) (stdoutSpill, stderrSpill, combinedSpill *spillWriter, doneRead chan struct{}) {
	stdoutSpill = newSpillWriter(spillThreshold, spillTempDir)
	stderrSpill = newSpillWriter(spillThreshold, spillTempDir)
	combinedSpill = newSpillWriter(spillThreshold, spillTempDir)
	doneRead = make(chan struct{})

	go func() {
		defer close(doneRead)
		var wg sync.WaitGroup
		wg.Add(2)
		if outputMode == "separated" {
			go func() { defer wg.Done(); _, _ = io.Copy(stdoutSpill, stdout) }()
			go func() { defer wg.Done(); _, _ = io.Copy(stderrSpill, stderr) }()
		} else {
			go func() { defer wg.Done(); _, _ = io.Copy(combinedSpill, stdout) }()
			go func() { defer wg.Done(); _, _ = io.Copy(combinedSpill, stderr) }()
		}
		wg.Wait()
	}()
	return
}

// applySpillResult sets ResultFile and ResultSize on the ToolResult if any
// spillWriter overflowed to disk. For separated mode, pass stdoutSpill and
// stderrSpill (the combined one is ignored); for combined mode, pass combinedSpill.
func applySpillResult(result *ToolResult, stdoutSpill, stderrSpill, combinedSpill *spillWriter) {
	if combinedSpill != nil && combinedSpill.Spilled() {
		result.ResultFile = combinedSpill.FilePath()
		result.ResultSize = combinedSpill.Total()
		return
	}
	// For separated mode, report the larger spill
	if stdoutSpill != nil && stdoutSpill.Spilled() {
		result.ResultFile = stdoutSpill.FilePath()
		result.ResultSize = stdoutSpill.Total()
	} else if stderrSpill != nil && stderrSpill.Spilled() {
		result.ResultFile = stderrSpill.FilePath()
		result.ResultSize = stderrSpill.Total()
	}
}

// redactSecrets removes sensitive information from output using available stores.
func redactSecrets(output string, store *secrets.Store, bwStore *bitwarden.Store) string {
	if store != nil {
		output = store.Redact(output)
	}
	if bwStore != nil {
		output = bwStore.Redact(output)
	}
	return output
}

// formatResult formats command output with error info, truncation, and redaction.
func formatResult(output string, err error, ctx context.Context, timeout time.Duration, displayCmd string, store *secrets.Store, bwStore *bitwarden.Store) string {
	result := redactSecrets(output, store, bwStore)

	if err != nil {
		if ctx.Err() != nil {
			log.Debugf("exec", "session=%s command timed out after %s: %s", SessionKeyFromContext(ctx), timeout, truncateCmd(displayCmd, 100))
		}
		return result + "\nError: " + err.Error()
	}

	return result
}

// separatedOutput is the JSON structure returned by exec in "separated" output mode.
type separatedOutput struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// formatSeparatedResult returns a JSON object with stdout, stderr, and exit_code.
func formatSeparatedResult(stdout, stderr string, err error, store *secrets.Store, bwStore *bitwarden.Store) string {
	stdout = redactSecrets(stdout, store, bwStore)
	stderr = redactSecrets(stderr, store, bwStore)

	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}

	b, _ := json.Marshal(separatedOutput{Stdout: stdout, Stderr: stderr, ExitCode: code})
	return string(b)
}

// allSecretRefsInHTTPRequestScope returns true if every {{secret:}} template
// in cmd appears within a foci_http_request invocation — i.e. before any
// shell command separator (|, ;, &&, ||) that follows the foci_http_request call.
// This allows the exec bridge to pass secret templates as literal strings to
// the http_request tool for server-side resolution.
func allSecretRefsInHTTPRequestScope(cmd string) bool {
	// Find all {{secret:...}} positions in the command
	secretLocs := secretTemplateRe.FindAllStringIndex(cmd, -1)
	if len(secretLocs) == 0 {
		return true
	}

	// For each secret ref, check that it falls within a foci_http_request segment.
	// Split the command by separators and check which segment each ref is in.
	sepLocs := cmdSeparatorRe.FindAllStringIndex(cmd, -1)

	// Build segment boundaries: [0, sep1_start], [sep1_end, sep2_start], ...
	type segment struct{ start, end int }
	var segments []segment
	pos := 0
	for _, sep := range sepLocs {
		segments = append(segments, segment{pos, sep[0]})
		pos = sep[1]
	}
	segments = append(segments, segment{pos, len(cmd)})

	for _, loc := range secretLocs {
		refStart := loc[0]
		// Find which segment this ref is in
		inHTTPRequest := false
		for _, seg := range segments {
			if refStart >= seg.start && refStart < seg.end {
				segText := cmd[seg.start:seg.end]
				if strings.Contains(segText, "foci_http_request") {
					inHTTPRequest = true
				}
				break
			}
		}
		if !inHTTPRequest {
			return false
		}
	}
	return true
}

func truncateCmd(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
