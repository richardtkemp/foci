package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/internal/procx"
	"foci/internal/secrets"
	"foci/internal/secrets/bitwarden"
	"foci/internal/tools"
	"foci/internal/tools/spill"
	"foci/internal/usercmd"
)

// UserRunner runs chat "!" commands for one agent (#2812): the same user, cwd,
// capture, spill and scrubbing as the shell tool (runForeground), on every
// backend. Dick's rulings differ from the shell tool in the checks: blocked
// paths are not checked and sleep is not refused; the {{secret:}} refusal stays.
// The agent's own shell tool is untouched and keeps all its checks.
type UserRunner struct {
	Store          *secrets.Store
	Bitwarden      *bitwarden.Store
	WorkDir        string
	Registry       *tools.Registry // foci_* shell functions; nil = none
	SpillThreshold func() int64    // [summary] max_result_chars; <=0 = defaultSpillThreshold
	SpillTempDir   string
	ExtraEnv       []string
	Timeout        func() time.Duration
}

// Check refuses a script the runner will not run, as the shell tool refuses it.
func (r *UserRunner) Check(script string) error {
	return checkSecretTemplates(script)
}

// Runnable reports whether word, the first command word of a code block in chat
// (#2878, #2882), can start a "!" command. Three kinds of word can:
//   - an executable file, found the way the command's shell finds it: a regular
//     file this user may execute. A word with a slash is a path (relative to
//     WorkDir, the command's cwd; a leading ~/ is the home dir). A bare word is
//     looked up on the operator PATH, the PATH the command's process gets
//     (runForeground; ExtraEnv never sets PATH). A file in WorkDir with no slash in
//     the word is not found, as in the shell.
//   - a foci_* shell function runForeground defines (one per available exported tool).
//   - a word the shell itself runs (shellStartsCommand).
func (r *UserRunner) Runnable(word string) bool {
	// A file of the same name does not help: the shell's own word comes first (macOS
	// has /usr/bin/fg).
	if word == "" || strings.ContainsRune(word, 0) || notCommandStart[word] {
		return false
	}
	if r.hasFociFunction(word) || shellStartsCommand(word) {
		return true
	}
	name := word
	if rest, ok := strings.CutPrefix(word, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		name = filepath.Join(home, rest)
	}
	if strings.Contains(name, "/") && !filepath.IsAbs(name) {
		name = filepath.Join(r.WorkDir, name)
	}
	// An ErrDot answer (a relative PATH entry) is relative to the gateway's cwd, not
	// the command's: it counts as not found.
	_, err := procx.LookPath(procx.Operator, name)
	return err == nil
}

// hasFociFunction reports whether word is a foci_* function a command can call. A
// tool whose live gate is closed still has a function, but only to refuse: not
// listed by ExportedTools, so it does not count.
func (r *UserRunner) hasFociFunction(word string) bool {
	if r.Registry == nil || !strings.HasPrefix(word, "foci_") {
		return false
	}
	for _, t := range r.Registry.ExportedTools() {
		if t.Name == word {
			return true
		}
	}
	return false
}

// notCommandStart holds the shell's own words that cannot start a script run by
// "bash -c". The reserved words only continue a compound command (a script starting
// with one is a syntax error). The builtins always fail outside a function, a loop
// or an interactive shell, whatever their arguments (measured on bash 5).
var notCommandStart = map[string]bool{
	"then": true, "else": true, "elif": true, "fi": true, "esac": true,
	"do": true, "done": true, "in": true, "}": true, "]]": true,
	"bg": true, "fg": true, "break": true, "continue": true, "compopt": true,
	"local": true, "logout": true, "return": true, "suspend": true,
}

// shellWords caches the shell's builtins and reserved words. A failure to ask is
// not cached: it would remove the button until the next restart.
var shellWords struct {
	sync.Mutex
	words map[string]bool
}

// shellStartsCommand reports whether the exec shell itself runs word as the start
// of a script: a builtin or reserved word (the shell is asked once, because the
// lists differ by version: macOS has bash 3.2) or "(", a subshell. Only bash can
// answer. Under the sh fallback it is always false.
func shellStartsCommand(word string) bool {
	if !hasBash() {
		return false
	}
	if word == "(" {
		return true
	}
	shellWords.Lock()
	defer shellWords.Unlock()
	if shellWords.words == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Operator: the shell a "!" command runs under.
		out, err := procx.Spawn(ctx, procx.Operator, execShell(), "-c", "compgen -b -k").Output()
		if err != nil {
			execLog.Warnf("list shell builtins: %v", err)
			return false
		}
		shellWords.words = make(map[string]bool)
		for _, w := range strings.Fields(string(out)) {
			shellWords.words[w] = true
		}
	}
	return shellWords.words[word]
}

// Redact scrubs secret values from text, exactly as shell tool output is.
func (r *UserRunner) Redact(text string) string {
	return redactSecrets(text, r.Store, r.Bitwarden)
}

// Run runs script and waits for it. Cancelling ctx kills the command's process
// group and reports StateCancelled. The result's ForAgent is left for the caller
// (the agent's result guard decides it).
func (r *UserRunner) Run(ctx context.Context, script string) usercmd.Result {
	res := usercmd.Result{Command: r.Redact(script), Timeout: r.Timeout()}

	if r.SpillTempDir != "" {
		if err := os.MkdirAll(r.SpillTempDir, 0o700); err != nil {
			execLog.Warnf("create spill dir %s: %v", r.SpillTempDir, err)
		}
	}
	st := int64(defaultSpillThreshold)
	if r.SpillThreshold != nil {
		if v := r.SpillThreshold(); v > 0 {
			st = v
		}
	}

	cmd := script
	if r.Bitwarden != nil {
		resolved, err := r.Bitwarden.Resolve(cmd)
		if err != nil {
			res.State = usercmd.StateFailed
			res.Detail = r.Redact(fmt.Sprintf("resolve bitwarden secrets: %v", err))
			return res
		}
		cmd = resolved
	}

	runCtx, cancel := context.WithTimeout(ctx, res.Timeout)
	defer cancel()
	out, err := runForeground(runCtx, cmd, false, r.WorkDir, r.Registry, "combined", st, r.SpillTempDir, r.ExtraEnv)
	if err != nil {
		res.State = usercmd.StateFailed
		res.Detail = r.Redact(err.Error())
		return res
	}
	_ = out.combined.Close()

	switch {
	case out.err == nil:
		res.State = usercmd.StateExited
	case ctx.Err() != nil:
		res.State = usercmd.StateCancelled
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.State = usercmd.StateTimedOut
	default:
		var exitErr *exec.ExitError
		if errors.As(out.err, &exitErr) {
			res.State = usercmd.StateExited
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.State = usercmd.StateFailed
			res.Detail = r.Redact(out.err.Error())
		}
	}

	if out.combined.Spilled() {
		res.File = out.combined.FilePath()
		res.Size = out.combined.Total()
		if err := spill.ScrubFile(res.File, r.Redact); err != nil {
			// A saved file that could not be scrubbed must not be handed on.
			execLog.Errorf("scrub saved output %s: %v", res.File, err)
			out.combined.Cleanup()
			res.File = ""
			res.State = usercmd.StateFailed
			res.Detail = "could not scrub the saved output"
			return res
		}
	}
	res.Output = r.Redact(out.combined.String())
	return res
}
