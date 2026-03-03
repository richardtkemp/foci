package tools

import (
	"bytes"
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

	"foci/log"
	"foci/secrets"
	"foci/secrets/bitwarden"
)

const execMaxOutputBytes = 100 * 1024 * 1024 // 100MB backstop on stdout/stderr; guardToolResult handles char truncation

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
	log.Infof("exec", "bash not found, falling back to sh (pipefail and tool-piping shell functions unavailable)")
	return "sh"
})

// hasBash reports whether the exec shell is bash.
func hasBash() bool { return execShell() == "bash" }

// NewExecTool creates an exec tool. If store is non-nil, commands get
// secret template resolution, output redaction, and blocked path checks.
// autoBackgroundSecs is the threshold after which a running command is
// auto-backgrounded (0 disables). notifier delivers results when an
// auto-backgrounded command finishes (nil disables).
// workDir sets the default working directory for commands (empty = process cwd).
// registry enables the exec bridge (tool piping) — if non-nil, exported tools
// are available as shell functions inside exec commands.
func NewExecTool(store *secrets.Store, bwStore *bitwarden.Store, autoBackgroundSecs int, notifier *AsyncNotifier, workDir string, registry *Registry) *Tool {
	return &Tool{
		Name:        "exec",
		Description: "Run a shell command and return its output. set -e is active (stops on first error). Use timeout to limit execution time. Set background=true for commands that spawn persistent processes (tmux, daemons) — children will survive after the exec call. Regular {{secret:}} templates are blocked — use http_request for API calls. Exception: {{secret:}} templates inside foci_http_request arguments are allowed (passed as literal strings for server-side resolution). Bitwarden {{secret:bw.UUID}} templates are allowed (approval-gated).",
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return execCommand(ctx, params, store, bwStore, autoBackgroundSecs, notifier, workDir, registry)
		},
	}
}

func execCommand(ctx context.Context, params json.RawMessage, store *secrets.Store, bwStore *bitwarden.Store, autoBackgroundSecs int, notifier *AsyncNotifier, workDir string, registry *Registry) (string, error) {
	var p struct {
		Command    string `json:"command"`
		Timeout    int    `json:"timeout"`
		Background bool   `json:"background"`
		OutputMode string `json:"output_mode"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	// Check blocked paths
	if store != nil && store.IsBlockedCommand(p.Command) {
		return "", fmt.Errorf("command references a blocked path")
	}

	// Block bare sleep commands - they block for up to 10s then silently
	// background, which is the worst of both worlds. Use remind instead.
	if sleepRegexp.MatchString(p.Command) {
		return "", fmt.Errorf("sleep is not allowed via exec — use remind for timed check-ins instead")
	}

	// Block regular secret templates — secrets must not reach child processes.
	// Bitwarden secrets (bw.*) are allowed because they're approval-gated via aisudo.
	// Secret refs inside foci_http_request args are also allowed — the template
	// is passed as a literal string to the tool server which resolves it safely.
	cmd := p.Command
	if refs := secrets.FindSecretRefs(cmd); refs != nil {
		for _, ref := range refs {
			if !bitwarden.IsBitwardenRef(ref) && !allSecretRefsInHTTPRequestScope(cmd) {
				return "", fmt.Errorf("{{secret:}} templates are not allowed in exec — use the http_request tool or foci_http_request shell function instead")
			}
		}
	}
	// Resolve bitwarden secret templates (approval-gated, safe for exec)
	if bwStore != nil {
		resolved, err := bwStore.Resolve(cmd)
		if err != nil {
			return "", fmt.Errorf("resolve bitwarden secrets: %w", err)
		}
		cmd = resolved
	}

	timeout := 30 * time.Second
	if p.Timeout > 0 {
		timeout = time.Duration(p.Timeout) * time.Second
	}

	log.Debugf("exec", "running: %s (timeout=%s background=%v)", truncateCmd(p.Command, 200), timeout, p.Background)

	// For explicit background mode, use the original direct approach (no bridge)
	if p.Background {
		return execDirect(ctx, cmd, p.Command, timeout, true, store, bwStore, workDir, nil, p.OutputMode)
	}

	// Auto-background: if threshold is set and notifier is available,
	// start the command and wait with a timer
	if autoBackgroundSecs > 0 && notifier != nil {
		sk := SessionKeyFromContext(ctx)
		return execWithAutoBackground(ctx, cmd, p.Command, timeout, store, bwStore, autoBackgroundSecs, notifier, sk, workDir, registry, p.OutputMode)
	}

	return execDirect(ctx, cmd, p.Command, timeout, false, store, bwStore, workDir, registry, p.OutputMode)
}

// execDirect runs a command and waits for completion (original behavior).
func execDirect(ctx context.Context, cmd, displayCmd string, timeout time.Duration, background bool, store *secrets.Store, bwStore *bitwarden.Store, workDir string, registry *Registry, outputMode string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Create exec bridge for tool piping (skipped for background mode and nil registry)
	var bridge *ExecBridge
	if !background && registry != nil {
		var err error
		bridge, err = NewExecBridge(registry, ctx)
		if err != nil {
			log.Debugf("exec", "exec bridge creation failed (continuing without): %v", err)
		} else {
			defer bridge.Close()
			if hasBash() {
				cmd = fmt.Sprintf("set -e -o pipefail; source %s; %s", bridge.FuncsPath(), cmd)
			} else {
				cmd = fmt.Sprintf("set -e; source %s; %s", bridge.FuncsPath(), cmd)
			}
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
		return "", fmt.Errorf("create stdout pipe: %w", err)
	}
	stderr, err := proc.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("create stderr pipe: %w", err)
	}

	if err := proc.Start(); err != nil {
		return "", fmt.Errorf("start command: %w", err)
	}

	// Read stdout and stderr concurrently — all reads must complete before
	// proc.Wait is called (Wait closes pipe read-ends, racing in-progress reads).
	stdoutBuf, stderrBuf, combined, doneRead := startPipeReaders(stdout, stderr, outputMode)
	<-doneRead
	err = proc.Wait()

	if outputMode == "separated" {
		return formatSeparatedResult(stdoutBuf.String(), stderrBuf.String(), err, store, bwStore), nil
	}
	return formatResult(combined.String(), err, ctx, timeout, displayCmd, store, bwStore), nil
}

// execWithAutoBackground starts a command and returns early if it exceeds the threshold.
// The command continues running and results are delivered via notifier to the originating session.
func execWithAutoBackground(ctx context.Context, cmd, displayCmd string, timeout time.Duration, store *secrets.Store, bwStore *bitwarden.Store, thresholdSecs int, notifier *AsyncNotifier, sessionKey, workDir string, registry *Registry, outputMode string) (string, error) {
	// Use a separate context for the command (not tied to agent turn)
	cmdCtx, cmdCancel := context.WithTimeout(context.Background(), timeout)

	// Create exec bridge for tool piping.
	// Use context.Background() + session key so bridge survives agent turn end.
	var bridge *ExecBridge
	if registry != nil {
		bridgeCtx := WithSessionKey(context.Background(), sessionKey)
		var err error
		bridge, err = NewExecBridge(registry, bridgeCtx)
		if err != nil {
			log.Debugf("exec", "exec bridge creation failed (continuing without): %v", err)
		} else {
			if hasBash() {
				cmd = fmt.Sprintf("set -e -o pipefail; source %s; %s", bridge.FuncsPath(), cmd)
			} else {
				cmd = fmt.Sprintf("set -e; source %s; %s", bridge.FuncsPath(), cmd)
			}
		}
	}

	proc := exec.CommandContext(cmdCtx, execShell(), "-c", cmd)
	proc.Dir = workDir

	// Inject FOCI_SOCK for exec bridge
	if bridge != nil {
		proc.Env = append(os.Environ(), "FOCI_SOCK="+bridge.SockPath())
	}
	proc.SysProcAttr = ChildSysProcAttr()
	proc.Cancel = func() error {
		return syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
	}

	// Use pipes with LimitReader to cap memory usage (Bug #115)
	stdout, err := proc.StdoutPipe()
	if err != nil {
		cmdCancel()
		return "", fmt.Errorf("create stdout pipe: %w", err)
	}
	stderr, err := proc.StderrPipe()
	if err != nil {
		cmdCancel()
		return "", fmt.Errorf("create stderr pipe: %w", err)
	}

	if err := proc.Start(); err != nil {
		cmdCancel()
		return "", fmt.Errorf("start command: %w", err)
	}

	// Read stdout and stderr concurrently — all reads must complete before
	// proc.Wait is called (Wait closes pipe read-ends, racing in-progress reads).
	stdoutBuf, stderrBuf, combined, doneRead := startPipeReaders(stdout, stderr, outputMode)

	// done goroutine waits for all reads before calling proc.Wait so that
	// pipe read-ends are not closed while reads are still in progress.
	done := make(chan error, 1)
	go func() {
		<-doneRead
		done <- proc.Wait()
	}()

	threshold := time.Duration(thresholdSecs) * time.Second
	select {
	case err := <-done:
		// Command finished before threshold; reads already complete (done goroutine waited).
		cmdCancel()
		if bridge != nil {
			bridge.Close()
		}
		if outputMode == "separated" {
			return formatSeparatedResult(stdoutBuf.String(), stderrBuf.String(), err, store, bwStore), nil
		}
		return formatResult(combined.String(), err, cmdCtx, timeout, displayCmd, store, bwStore), nil

	case <-time.After(threshold):
		// Threshold exceeded — auto-background
		log.Infof("exec", "auto-backgrounding after %v: %s", threshold, truncateCmd(displayCmd, 100))
		bgDeliverResult(notifier, sessionKey, done, cmdCancel, bridge,
			stdoutBuf, stderrBuf, combined, outputMode, cmdCtx, timeout, displayCmd, store, bwStore)

		return fmt.Sprintf("Command still running (exceeded %ds threshold). Results will be delivered when complete.\n$ %s", thresholdSecs, displayCmd), nil

	case <-ctx.Done():
		// Agent turn cancelled — deliver results asynchronously like the
		// threshold path. Without this, command output is silently lost.
		// NOTE: async results live only in goroutine memory and are lost
		// on process restart. Persisting them (e.g. to a spool file)
		// would require plumbing a workspace path here; left as future work.
		log.Infof("exec", "turn cancelled, backgrounding: %s", truncateCmd(displayCmd, 100))
		bgDeliverResult(notifier, sessionKey, done, cmdCancel, bridge,
			stdoutBuf, stderrBuf, combined, outputMode, cmdCtx, timeout, displayCmd, store, bwStore)
		return "", ctx.Err()
	}
}

// lockedWriter wraps a bytes.Buffer with a mutex so concurrent goroutines can
// write without interleaving mid-write. Each Write call is atomic with respect
// to other writes, producing approximately chronological interleaving of
// stdout and stderr chunks.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string { return w.buf.String() }

// startPipeReaders launches goroutines to read stdout and stderr concurrently.
// All reads must complete before proc.Wait is called (Go 1.22+ closes pipe
// read-ends in Wait, racing in-progress reads).
//
// For "separated" mode, stdout and stderr are captured into independent buffers.
// For combined mode, both streams write through a mutex-protected buffer so
// output appears in roughly chronological order (not all-stdout-then-all-stderr).
//
// Returns (stdoutBuf, stderrBuf, combined, doneRead). Use stdoutBuf/stderrBuf
// for separated mode; use combined for combined mode. doneRead is closed when
// all reads are finished.
func startPipeReaders(stdout, stderr io.ReadCloser, outputMode string) (stdoutBuf, stderrBuf *bytes.Buffer, combined *lockedWriter, doneRead chan struct{}) {
	stdoutBuf = &bytes.Buffer{}
	stderrBuf = &bytes.Buffer{}
	combined = &lockedWriter{}
	doneRead = make(chan struct{})

	go func() {
		defer close(doneRead)
		var wg sync.WaitGroup
		wg.Add(2)
		if outputMode == "separated" {
			go func() { defer wg.Done(); _, _ = io.Copy(stdoutBuf, io.LimitReader(stdout, execMaxOutputBytes)) }()
			go func() { defer wg.Done(); _, _ = io.Copy(stderrBuf, io.LimitReader(stderr, execMaxOutputBytes)) }()
		} else {
			go func() { defer wg.Done(); _, _ = io.Copy(combined, io.LimitReader(stdout, execMaxOutputBytes)) }()
			go func() { defer wg.Done(); _, _ = io.Copy(combined, io.LimitReader(stderr, execMaxOutputBytes)) }()
		}
		wg.Wait()
	}()
	return
}

// bgDeliverResult waits for a backgrounded command to complete and delivers
// its results via the notifier. Used by both the threshold-exceeded and
// ctx.Done() paths in execWithAutoBackground.
func bgDeliverResult(notifier *AsyncNotifier, sessionKey string, done <-chan error, cmdCancel context.CancelFunc, bridge *ExecBridge,
	stdoutBuf, stderrBuf *bytes.Buffer, combined *lockedWriter, outputMode string, cmdCtx context.Context, timeout time.Duration, displayCmd string, store *secrets.Store, bwStore *bitwarden.Store) {
	notifier.MarkPending(sessionKey)
	go func() {
		defer cmdCancel()
		defer notifier.MarkDone(sessionKey)
		if bridge != nil {
			defer bridge.Close()
		}
		err := <-done // reads already complete (done goroutine waited)
		var result string
		if outputMode == "separated" {
			result = formatSeparatedResult(stdoutBuf.String(), stderrBuf.String(), err, store, bwStore)
		} else {
			result = formatResult(combined.String(), err, cmdCtx, timeout, displayCmd, store, bwStore)
		}
		msg := fmt.Sprintf("[EXEC RESULT] Command completed:\n$ %s\n\n%s", displayCmd, result)
		notifier.Notify(sessionKey, msg)
	}()
}

// formatResult formats command output with error info, truncation, and redaction.
func formatResult(output string, err error, ctx context.Context, timeout time.Duration, displayCmd string, store *secrets.Store, bwStore *bitwarden.Store) string {
	result := output

	// Redact secrets from output
	if store != nil {
		result = store.Redact(result)
	}
	if bwStore != nil {
		result = bwStore.Redact(result)
	}

	if err != nil {
		if ctx.Err() != nil {
			log.Debugf("exec", "command timed out after %s: %s", timeout, truncateCmd(displayCmd, 100))
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
	if store != nil {
		stdout = store.Redact(stdout)
		stderr = store.Redact(stderr)
	}
	if bwStore != nil {
		stdout = bwStore.Redact(stdout)
		stderr = bwStore.Redact(stderr)
	}

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
