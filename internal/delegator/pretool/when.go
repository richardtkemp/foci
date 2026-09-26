package pretool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// A rule's when-check (#2034) is a bash script that decides, from live
// state, whether a call the rule's patterns matched is really a violation:
// the branch the repo is on, whether a file has uncommitted edits, what a
// file's head says. It runs only after every other constraint of the rule
// holds, and the rule denies iff the script exits 0.
//
// It must never block a call on its own failure, so it FAILS OPEN: exit 1 is
// the ordinary "no", and anything else (another exit status, a timeout, a
// signal, bash not starting) is reported as an error and does not deny.
//
// What the script gets:
//   - $0 is the rule name.
//   - For a Bash rule with command patterns or fact filters, the words of
//     the matched simple command are its positional parameters: for `git -C /r checkout -- f`,
//     $1=git $2=-C $3=/r ... Quotes are removed; expansions keep their source
//     form ("$HOME/x" arrives as the literal $HOME/x). If several commands in
//     the script match, it runs once per command until one denies.
//   - stdin and $TOOL_INPUT are the tool_input JSON. Each top-level string
//     field is also $TOOL_INPUT_<FIELD> (upper-cased, other characters as _),
//     e.g. $TOOL_INPUT_FILE_PATH, $TOOL_INPUT_COMMAND.
//   - $TOOL_NAME is the tool; $TOOL_CWD and the working directory are the
//     call's cwd.
//   - For a Bash call, facts from the parsed script (#2040): $TOOL_COMMANDS
//     is every simple command, one per line, as command patterns see it;
//     $TOOL_END_DIR is the main shell's directory after the script (empty
//     if a cd target is dynamic). For a rule with per-command constraints,
//     the matched command's own: $CMD_DIR (the absolute directory it runs
//     in, after any cd before it; empty if unknown), $CMD_OP (the operator
//     before it) and $CMD_PIPE (the commands downstream of it in its
//     pipeline, one per line). See Command.
//   - The rest of the environment is the hook's (CC's), minus BASH_ENV and
//     ENV, so a non-interactive bash sources nothing first.
//
// Its stdout is discarded; stderr is kept (truncated) for the error report.

// Time limits, vars so tests can shrink them.
var (
	// whenTimeout bounds one run of a when-check.
	whenTimeout = 2 * time.Second
	// whenBudget bounds all when-checks for one call. CC kills the hook at
	// hookTimeoutSeconds (10s, ccstream), and a killed hook reports nothing,
	// so the whole evaluation has to finish well inside that.
	whenBudget = 5 * time.Second
)

const (
	// whenStderrMax caps the stderr kept for an error report.
	whenStderrMax = 512
	// whenWaitDelay is how long a run may hold its pipes after bash exits
	// or is killed (a backgrounded grandchild), before Wait gives up on it.
	whenWaitDelay = 500 * time.Millisecond
)

// WhenError is a when-check that failed open: the call was not denied by it.
type WhenError struct {
	Rule string
	Err  error
}

func (e WhenError) Error() string { return fmt.Sprintf("rule %s: when: %v", e.Rule, e.Err) }

var envNameRe = regexp.MustCompile(`[^A-Z0-9_]`)

// whenEnv builds the script's environment from the hook's own.
func whenEnv(c Call, fields map[string]json.RawMessage) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		// PWD would name the hook's directory, not the call's; bash sets it.
		if k == "BASH_ENV" || k == "ENV" || k == "PWD" || strings.HasPrefix(k, "TOOL_") || strings.HasPrefix(k, "CMD_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "TOOL_NAME="+c.Tool, "TOOL_INPUT="+string(c.Input), "TOOL_CWD="+c.Cwd)
	for field, raw := range fields {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		env = append(env, "TOOL_INPUT_"+envNameRe.ReplaceAllString(strings.ToUpper(field), "_")+"="+s)
	}
	return env
}

// scriptEnv is the when-check environment for a Bash call's parsed script.
func scriptEnv(s Script, cwd string) []string {
	texts := make([]string, len(s.Commands))
	for i, c := range s.Commands {
		texts[i] = c.Text
	}
	return []string{
		"TOOL_COMMANDS=" + strings.Join(texts, "\n"),
		"TOOL_END_DIR=" + absDir(s.EndDir, s.EndDirKnown, cwd),
	}
}

// commandEnv is the when-check environment for one matched command.
func commandEnv(c *Command, cwd string) []string {
	return []string{
		"CMD_DIR=" + absDir(c.Dir, c.DirKnown, cwd),
		"CMD_OP=" + c.Op,
		"CMD_PIPE=" + strings.Join(c.Pipe, "\n"),
	}
}

// absDir resolves a Script directory against the call's cwd (the hook's own
// directory when the call has none): "" when unknown.
func absDir(dir string, known bool, cwd string) string {
	if !known {
		return ""
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		cwd = wd
	}
	return filepath.Join(cwd, dir)
}

// runWhen runs one when-check. deny is true iff it exited 0; err is set for
// every outcome other than exit 0 or exit 1.
func runWhen(ctx context.Context, rule, script string, args []string, c Call, env []string) (deny bool, err error) {
	if c.Cwd != "" {
		if st, serr := os.Stat(c.Cwd); serr != nil || !st.IsDir() {
			return false, fmt.Errorf("cwd %q is not a directory", c.Cwd)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, whenTimeout)
	defer cancel()
	// Raw exec, not procx.Spawn: this runs in foci-cc-hook (a child of CC,
	// which foci-gw already spawned without the secret groups) and in the
	// foci CLI, never in foci-gw. procx would also link its sqlite and log
	// dependencies into the per-call hook helper.
	cmd := exec.CommandContext(ctx, "bash", append([]string{"-c", script, rule}, args...)...) //nolint:forbidigo // see above
	cmd.Dir = c.Cwd
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(c.Input)
	var stderr capped
	cmd.Stderr = &stderr
	// Own process group, killed whole on timeout, so a pipeline or a
	// backgrounded child can't outlive the check.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = whenWaitDelay

	runErr := cmd.Run()
	if ctx.Err() != nil {
		return false, fmt.Errorf("timed out (%s)%s", whenTimeout, stderr.suffix())
	}
	if runErr == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("%v%s", runErr, stderr.suffix())
}

// capped is a Writer that keeps the first whenStderrMax bytes.
type capped struct{ b bytes.Buffer }

func (w *capped) Write(p []byte) (int, error) {
	if room := whenStderrMax - w.b.Len(); room > 0 {
		w.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (w *capped) suffix() string {
	s := strings.TrimSpace(w.b.String())
	if s == "" {
		return ""
	}
	return ": " + s
}
