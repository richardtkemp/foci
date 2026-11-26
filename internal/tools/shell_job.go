package tools

// Shell jobs (#3286): a depth leaf waits on a plain shell command in the
// background without starting a model. The leaf's own backend refuses
// background shells (#2625), because their result would reach only the
// leaf's backend process and be lost once the leaf reports. A shell job runs
// in foci instead, and is a spawn in every way that matters to its caller: it
// is tracked by the spawn tracker (so spawn_status lists it, spawn_stop and
// /stop stop it, the app lists it), and its result is a [SPAWN RESULT]
// delivered through Spawner.deliver, so it is held and passed up the depth
// tree like any spawn's when the leaf has reported (#2624).
//
// Dick's rulings (2026-11-26): leaves only; a command no rule of the agent
// allows is put to the user in the app and runs only on approval; a job
// takes no spawn slot ([tools] max_concurrent_spawns); a job running when
// foci restarts is reported lost, never run again; its own tool, not a spawn
// mode; timeout 1 h by default, 4 h at most.
//
// The command runs under the agent's own permission rules: the outer
// foci_shell_job call is auto-approved like every foci_* function
// (autoapprove.FociShellRulesFor), so the inner command is checked here
// (ShellJobEnv.Check), before anything runs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"foci/internal/usercmd"
)

// spawnModeShell is a shell job's RunningSpawn.Mode.
const spawnModeShell = "shell"

const (
	// ShellJobDefaultTimeout and ShellJobMaxTimeout bound a shell job's run
	// (Dick 2026-11-26: "1 h default, 4 h max").
	ShellJobDefaultTimeout = time.Hour
	ShellJobMaxTimeout     = 4 * time.Hour
	// shellJobOutputTail is how much of the end of a job's output its
	// result carries, in bytes; the whole output is in its saved file when
	// it is longer than the spill threshold.
	shellJobOutputTail = 6000
)

// ShellJobVerdict is what the agent's rules say about a shell job's command.
type ShellJobVerdict int

const (
	// ShellJobAsk: no rule allows the command; the user is asked, and it
	// runs only if they approve.
	ShellJobAsk ShellJobVerdict = iota
	// ShellJobAllow: an auto-approve rule of the agent allows it.
	ShellJobAllow
	// ShellJobDeny: a rule refuses it (a deny rule, a blocked path); it is
	// never run, and nobody is asked.
	ShellJobDeny
)

// ShellJobCheck is ShellJobEnv.Check's answer: the verdict, and why (shown
// to the user when asked, or to the agent when refused).
type ShellJobCheck struct {
	Verdict ShellJobVerdict
	Reason  string
}

// ShellJobApproval is a request to the user to approve a shell job.
type ShellJobApproval struct {
	// SessionKey is the session that started the job: its chat shows the
	// request (or, when that chat cannot, the next one up).
	SessionKey string
	ID, Name   string
	Command    string
	Timeout    time.Duration
	// Why says why the user is asked (no rule allows the command).
	Why string
}

// ShellJobEnv is what a shell job needs from the gateway (SpawnDeps.ShellJobs).
type ShellJobEnv struct {
	// Check judges a command by the agent's own rules (#3286: "no new
	// privilege path").
	Check func(sessionKey, command string) ShellJobCheck
	// Approve asks the user and waits for the answer: true when they
	// approved. False when they denied it, nobody could be asked, the
	// request expired, or ctx ended (the job was stopped); why says which.
	Approve func(ctx context.Context, req ShellJobApproval) (approved bool, why string)
	// Run runs command under the agent's user and workspace and waits for
	// it, killing it at timeout or when ctx ends.
	Run func(ctx context.Context, command string, timeout time.Duration) usercmd.Result
}

// shellJobParams is shell_job's input schema.
var shellJobParams = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {
			"type": "string",
			"description": "A short name for the job, unique among this session's spawns and jobs; spawn_status, spawn_stop and its result use it."
		},
		"command": {
			"type": "string",
			"description": "The shell command (bash), run in your workspace as your user. From the shell, piped stdin (e.g. a quoted heredoc) populates it when no command is given."
		},
		"timeout": {
			"type": "string",
			"description": "How long it may run before it is killed: a duration (90m, 2h) or seconds. Default 1h, at most 4h."
		}
	},
	"required": ["name", "command"]
}`)

// ShellJobTool is shell_job (#3286).
func (s *Spawner) ShellJobTool() *Tool {
	return &Tool{
		Name:       "shell_job",
		ExecExport: true,
		Positional: []string{"command"},
		StdinParam: "command",
		Description: "Inside a depth leaf only: run a plain shell command in the background, in foci, with no model. " +
			"Use it to wait on a script (watch CI, then merge) instead of a spawn. The command runs under your own permission rules: " +
			"one they do not allow is put to the user in the app, and runs only if they approve it. " +
			"Its result (exit status and the end of its output) comes as a [SPAWN RESULT], to you or, once you have reported, " +
			"to the node you report to; you may call foci_depth_report while it runs. spawn_status lists it; spawn_stop stops it. " +
			"It takes no spawn slot. A foci restart while it runs ends it, and it is reported lost, never run again.",
		Parameters: shellJobParams,
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				Name    string `json:"name"`
				Command string `json:"command"`
				Timeout string `json:"timeout"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			return s.startShellJob(ctx, p.Name, p.Command, p.Timeout)
		},
	}
}

// inDepthLeaf is SpawnDeps.InDepthLeaf, or InDepthLeaf when unset.
func (s *Spawner) inDepthLeaf(sessionKey string) bool {
	if s.deps.InDepthLeaf != nil {
		return s.deps.InDepthLeaf(sessionKey)
	}
	return InDepthLeaf(sessionKey)
}

// parseShellJobTimeout reads a shell job's timeout: a Go duration ("90m",
// "2h") or a number of seconds; "" is ShellJobDefaultTimeout.
func parseShellJobTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ShellJobDefaultTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		n, nerr := strconv.Atoi(raw)
		if nerr != nil {
			return 0, fmt.Errorf("timeout %q is neither a duration (90m, 2h) nor a number of seconds", raw)
		}
		d = time.Duration(n) * time.Second
	}
	switch {
	case d <= 0:
		return 0, fmt.Errorf("timeout must be more than 0 (default %s)", ShellJobDefaultTimeout)
	case d > ShellJobMaxTimeout:
		return 0, fmt.Errorf("timeout %s is over the %s limit", d, ShellJobMaxTimeout)
	}
	return d, nil
}

// startShellJob checks and registers a shell job and starts it on a
// goroutine. The call returns at once; the job's result is delivered later.
func (s *Spawner) startShellJob(ctx context.Context, rawName, rawCommand, rawTimeout string) (ToolResult, error) {
	caller := SessionKeyFromContext(ctx)
	if caller == "" {
		return ToolResult{}, errors.New("shell_job: no session in context")
	}
	if !s.inDepthLeaf(caller) {
		return ToolResult{}, errors.New("shell_job runs only inside a depth leaf. Here, run the command in the foreground, or in your own backend's background")
	}
	var env *ShellJobEnv
	if s.deps.ShellJobs != nil {
		env = s.deps.ShellJobs()
	}
	if env == nil || env.Check == nil || env.Approve == nil || env.Run == nil || s.deps.Notifier == nil || s.deps.Tracker == nil {
		return ToolResult{}, errors.New("shell_job is not available on this agent")
	}
	name, err := validSpawnName(rawName)
	if err != nil {
		return ToolResult{}, err
	}
	command := strings.TrimSpace(rawCommand)
	if command == "" {
		return ToolResult{}, errors.New("command is required")
	}
	timeout, err := parseShellJobTimeout(rawTimeout)
	if err != nil {
		return ToolResult{}, err
	}
	chk := env.Check(caller, command)
	if chk.Verdict == ShellJobDeny {
		spawnLog.Infof("shell job refused session=%s name=%q: %s", caller, name, chk.Reason)
		return ToolResult{}, fmt.Errorf("NOT run: %s", chk.Reason)
	}
	release, err := s.claimSpawnName(caller, name)
	if err != nil {
		return ToolResult{}, err
	}
	defer release()

	tr, obs := s.deps.Tracker, s.deps.Observer
	running := RunningSpawn{
		ID: newSpawnID(), Name: name, Description: spawnDescription(command),
		Started: time.Now(), Mode: spawnModeShell,
		SpawnerUnattended: s.deps.turnUnattended(caller),
		Depth:             s.callerSpawnDepth(ctx) + 1,
	}
	id := running.ID
	// Detached from the tool call: the job outlives it. Its session is the
	// caller's, so a foci_* function the command calls acts as the leaf.
	base, cancel := context.WithCancel(context.Background())
	jobCtx := WithSessionKey(base, caller)
	ask := chk.Verdict == ShellJobAsk
	tr.addWithPrompt(caller, running, cancel, command)
	tr.appendLog(caller, id, "prompt", command)
	if ask {
		tr.appendLog(caller, id, "message", "(waiting for the user's approval: "+chk.Reason+")")
	}
	spawnLog.Infof("shell job=%s name=%q session=%s timeout=%s ask=%t command=%d chars", id, name, caller, timeout, ask, len(command))
	if obs != nil {
		obs.SpawnStarted(caller, running, "$ "+command)
	}

	var msg string
	done := make(chan struct{})
	go func() {
		defer cancel()
		defer close(done)
		msg = s.runShellJob(jobCtx, env, caller, running, command, timeout, ask, chk.Reason)
	}()

	pending := fmt.Sprintf("Shell job started in the background (no model runs).\nName: %s\nJob: %s\nCommand: %s\nTimeout: %s\n"+
		"Its [SPAWN RESULT] comes when it ends: to you, or, once you have reported, to the node you report to. "+
		"spawn_status shows it; spawn_stop stops it.",
		running.Label(), id, truncatePromptPreview(oneLine(command)), timeout)
	if ask {
		pending = fmt.Sprintf("Shell job NOT started yet: it is WAITING FOR THE USER'S APPROVAL in the app (%s). It runs only if they approve it; if they deny it, its [SPAWN RESULT] says so.\nName: %s\nJob: %s\nCommand: %s\nTimeout: %s\n"+
			"Its [SPAWN RESULT] goes to you, or, once you have reported, to the node you report to. spawn_status shows it; spawn_stop withdraws it.",
			chk.Reason, running.Label(), id, truncatePromptPreview(oneLine(command)), timeout)
	}
	return RunInBackground(ctx, BackgroundParams{
		SessionKey:    caller,
		Notifier:      s.deps.Notifier,
		Done:          done,
		NotifyMessage: func() string { return msg },
		Deliver:       func(m string) { s.deliver(caller, id, m) },
		ID:            spawnBackgroundID(running),
		PendingResult: TextResult(pending),
	})
}

// runShellJob waits for the user's approval when ask is set, then runs the
// command, and returns the job's [SPAWN RESULT]: "" when foci's shutdown cut
// it, whose persisted record the next process reports lost. The command is
// run only when no approval was needed, or the user gave it.
func (s *Spawner) runShellJob(ctx context.Context, env *ShellJobEnv, caller string, running RunningSpawn,
	command string, timeout time.Duration, ask bool, why string) string {
	tr, obs, id := s.deps.Tracker, s.deps.Observer, running.ID
	head := shellJobResultHead(running.Label(), id)
	finish := func(force bool, outcome, logKind, logText string) (end runEnd, ok bool) {
		end = tr.finishRun(caller, id, force, "", "")
		if end.interrupted {
			spawnLog.Infof("shell job=%s cut short by shutdown — the next process reports it lost", id)
			return end, false
		}
		if end.stopped {
			outcome = "stopped"
		}
		tr.setOutcome(caller, id, outcome)
		if logText != "" {
			tr.appendLog(caller, id, logKind, logText)
		}
		if obs != nil {
			if logText != "" {
				obs.SpawnText(caller, id, logText)
			}
			obs.SpawnEnded(caller, id)
		}
		return end, true
	}

	if ask {
		approved, answer := env.Approve(ctx, ShellJobApproval{
			SessionKey: caller, ID: id, Name: running.Name, Command: command, Timeout: timeout, Why: why,
		})
		if !approved {
			if ctx.Err() != nil {
				if _, ok := finish(true, "stopped", "message", "(stopped before the user approved it; it never ran)"); !ok {
					return ""
				}
				return fmt.Sprintf("%s was stopped by %s before the user approved it. The command never ran.", head, tr.stopper(caller, id))
			}
			spawnLog.Infof("shell job=%s not approved: %s", id, answer)
			if _, ok := finish(false, "denied", "error", "not approved: "+answer); !ok {
				return ""
			}
			return fmt.Sprintf("%s was NOT run: the user did not approve it (%s).\nCommand: %s", head, answer, command)
		}
		tr.appendLog(caller, id, "message", "(approved by the user)")
		spawnLog.Infof("shell job=%s approved by the user", id)
	}
	if ctx.Err() != nil {
		if _, ok := finish(true, "stopped", "message", "(stopped before it started)"); !ok {
			return ""
		}
		return fmt.Sprintf("%s was stopped by %s before it started. The command never ran.", head, tr.stopper(caller, id))
	}

	start := time.Now()
	res := env.Run(ctx, command, timeout)
	took := time.Since(start).Round(time.Second)
	spawnLog.Infof("shell job=%s ended: %s after %s", id, res.Status(), took)
	outcome := "failed"
	if res.State == usercmd.StateExited && res.ExitCode == 0 {
		outcome = "completed"
	}
	end, ok := finish(res.State != usercmd.StateExited, outcome, "result", res.Status())
	if !ok {
		return ""
	}
	if end.stopped {
		return fmt.Sprintf("%s was stopped by %s after %s.%s", head, tr.stopper(caller, id), took, shellJobOutput(res))
	}
	var what string
	switch res.State {
	case usercmd.StateExited:
		what = fmt.Sprintf("exited with status %d after %s", res.ExitCode, took)
	case usercmd.StateTimedOut:
		what = fmt.Sprintf("timed out after %s and was killed", res.Timeout)
	default:
		what = res.Status()
	}
	return fmt.Sprintf("%s %s.\nCommand: %s%s", head, what, command, shellJobOutput(res))
}

// shellJobResultHead opens a shell job's [SPAWN RESULT].
func shellJobResultHead(label, id string) string {
	return fmt.Sprintf("[SPAWN RESULT] Shell job %s (%s)", label, id)
}

// shellJobOutput is the end of a job's output, for its result: the last
// shellJobOutputTail bytes, read from its saved file when the output was
// saved, with that file's path.
func shellJobOutput(res usercmd.Result) string {
	out := res.Output
	if res.File != "" {
		if tail, err := fileTail(res.File, shellJobOutputTail); err == nil {
			out = tail
		}
	}
	cut := len(out) > shellJobOutputTail
	if cut {
		out = out[len(out)-shellJobOutputTail:]
	}
	out = strings.TrimRight(out, "\n")
	var b strings.Builder
	if out == "" {
		b.WriteString("\n\n(no output)")
	} else {
		if cut || res.File != "" {
			fmt.Fprintf(&b, "\n\nLast %d bytes of its output:\n", len(out))
		} else {
			b.WriteString("\n\nOutput:\n")
		}
		b.WriteString("```\n" + out + "\n```")
	}
	if res.File != "" {
		fmt.Fprintf(&b, "\nFull output (%d bytes): %s", res.Size, res.File)
	}
	return b.String()
}

// fileTail reads the last n bytes of the file at path.
func fileTail(path string, n int64) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the job's own spill file, made by foci
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := max(st.Size()-n, 0)
	b := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(b), nil
}
