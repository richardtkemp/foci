package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"foci/internal/fap"
)

// TestShellFuncRefusalExitsNonZero is the #2841 regression: a refused call
// printed "Error: ..." on stdout and exited 0, against the promise in --help
// ("a message on stderr and a non-zero exit"), so a script that branches on the
// exit code read a refusal as success. It drives the real path: generated shell
// function, foci-call, bridge, tool.
func TestShellFuncRefusalExitsNonZero(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	binDir := buildFociCall(t)

	phone := func(res fap.ToolResult, err error) func() (AppInvoker, bool) {
		return func() (AppInvoker, bool) { return &fakeInvoker{result: res, err: err}, true }
	}
	cases := []struct {
		name    string
		tool    *Tool
		command string
		wantErr string
	}{
		{"client bad argument", NewClientTool(phone(fap.ToolResult{}, nil)), `foci_client set_alarm 24:10 x`, "outside 00:00-23:59"},
		{"client bad argument --json", NewClientTool(phone(fap.ToolResult{}, nil)), `foci_client set_alarm 24:10 x --json`, "outside 00:00-23:59"},
		{"client unknown action", NewClientTool(phone(fap.ToolResult{}, nil)), `foci_client nosuchaction`, "unknown action"},
		{"client no phone", NewClientTool(func() (AppInvoker, bool) { return nil, false }), `foci_client set_alarm 07:00 x`, "no Android device connected"},
		{"client phone refuses", NewClientTool(phone(fap.ToolResult{Status: fap.ToolStatusError, Error: "action switched off"}, nil)), `foci_client set_alarm 07:00 x`, "action switched off"},
		{"client phone unreachable", NewClientTool(phone(fap.ToolResult{}, ErrNoAppDevice)), `foci_client set_alarm 07:00 x`, "no Android device connected"},
		{"client still pending", NewClientTool(phone(fap.ToolResult{Status: fap.ToolStatusPending}, nil)), `foci_client set_alarm 07:00 x`, "won't be delivered"},
		{"close_session no session", NewCloseSessionTool(func(_, _ string) (int, error) { return 1, nil }, nil), `foci_close_session`, "no session key"},
		{"set_session_alias no session", NewSetSessionAliasTool(nil, nil), `foci_set_session_alias --alias name`, "no session key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, runErr := runToolShell(t, binDir, c.tool, c.command)
			var exit *osexec.ExitError
			if !errors.As(runErr, &exit) {
				t.Errorf("exit status: run error = %v, want a non-zero exit", runErr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, c.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, c.wantErr)
			}
		})
	}

	// The other arm, through the same path: a completed phone action keeps exit
	// 0 and its output on stdout.
	stdout, stderr, runErr := runToolShell(t, binDir,
		NewClientTool(phone(fap.ToolResult{Status: fap.ToolStatusCompleted, Output: json.RawMessage(`{"ok":true}`)}, nil)),
		`foci_client set_alarm 07:00 x`)
	if runErr != nil || stdout != `{"ok":true}` {
		t.Errorf("completed action: run error = %v, stdout = %q, stderr = %q; want exit 0 and the output", runErr, stdout, stderr)
	}
}

// runToolShell runs command against a bridge holding only tool, through the
// generated shell function and foci-call.
func runToolShell(t *testing.T, binDir string, tool *Tool, command string) (stdout, stderr string, runErr error) {
	t.Helper()
	return runToolShellCtx(t, context.Background(), binDir, tool, command)
}

// runToolShellCtx is runToolShell for a tool that reads its caller from ctx.
func runToolShellCtx(t *testing.T, ctx context.Context, binDir string, tool *Tool, command string) (stdout, stderr string, runErr error) {
	t.Helper()
	r := NewRegistry()
	r.Register(tool)
	bridge, err := NewExecBridge(r, ctx)
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	cmd := osexec.Command("bash", "-c", "source "+bridge.FuncsPath()+"; "+command)
	cmd.Env = append(os.Environ(), "FOCI_SOCK="+bridge.SockPath(), "PATH="+binDir+":"+os.Getenv("PATH"))
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	runErr = cmd.Run()
	return outBuf.String(), errBuf.String(), runErr
}

// TestSpawnRefusalExitsNonZero is the #2849 regression, the same class as #2841:
// "NOT started" and "NOT delivered" printed on stdout and exited 0, so a script
// that branches on the exit code read a refusal as success. Each arm goes through
// the real path: generated shell function, foci-call, bridge, tool.
func TestSpawnRefusalExitsNonZero(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	binDir := buildFociCall(t)
	const holdMsg = "the usage hold is on (only keepalives and reminders run) until Tue 3 Nov 04:00."
	warm := func(now time.Time) time.Time { return now.Add(time.Hour) }
	cold := func(now time.Time) time.Time { return now.Add(-time.Hour) }

	// setup starts one spawn and lets it finish, ready for spawn_send.
	setup := func(t *testing.T, expiry func(time.Time) time.Time) (*followupHarness, *heldSpawnAgent, string) {
		t.Helper()
		agent := &heldSpawnAgent{cacheSpawnAgent: &cacheSpawnAgent{scriptedSpawnAgent: newScriptedSpawnAgent(0), expiry: expiry}}
		h := newFollowupHarness(agent)
		id := h.start(t, "find the bug")
		waitStarted(t, agent.scriptedSpawnAgent, 0)
		h.result(t)
		return h, agent, id
	}
	wantRefused := func(t *testing.T, stdout, stderr string, runErr error, wantErr ...string) {
		t.Helper()
		var exit *osexec.ExitError
		if !errors.As(runErr, &exit) {
			t.Errorf("exit status: run error = %v, want a non-zero exit", runErr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		for _, want := range wantErr {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, want)
			}
		}
	}

	t.Run("spawn during a usage hold", func(t *testing.T) {
		t.Parallel()
		h, agent, _ := setup(t, warm)
		agent.hold(holdMsg)
		stdout, stderr, runErr := runToolShellCtx(t, h.ctx, binDir, h.spawner.SpawnTool(),
			`foci_spawn --name nope --prompt task --context clone`)
		wantRefused(t, stdout, stderr, runErr, "NOT started", "usage hold", "04:00")
	})
	t.Run("spawn_send during a usage hold", func(t *testing.T) {
		t.Parallel()
		h, agent, id := setup(t, warm)
		agent.hold(holdMsg)
		stdout, stderr, runErr := runToolShellCtx(t, h.ctx, binDir, h.spawner.SendTool(),
			`foci_spawn_send --id `+id+` --message go`)
		wantRefused(t, stdout, stderr, runErr, "NOT delivered", "usage hold", "04:00")
	})
	t.Run("spawn_send to a cold finished spawn", func(t *testing.T) {
		t.Parallel()
		h, _, id := setup(t, cold)
		stdout, stderr, runErr := runToolShellCtx(t, h.ctx, binDir, h.spawner.SendTool(),
			`foci_spawn_send --id `+id+` --message go`)
		wantRefused(t, stdout, stderr, runErr, "NOT delivered", "cold", "new spawn")
	})

	// The other arm, through the same path: an accepted call keeps exit 0 and
	// its output on stdout.
	t.Run("spawn accepted", func(t *testing.T) {
		t.Parallel()
		h, _, _ := setup(t, warm)
		stdout, stderr, runErr := runToolShellCtx(t, h.ctx, binDir, h.spawner.SpawnTool(),
			`foci_spawn --name yes --prompt task --context clone`)
		if runErr != nil || !strings.Contains(stdout, "Spawn: spawn-") {
			t.Errorf("run error = %v, stdout = %q, stderr = %q; want exit 0 and the spawn id", runErr, stdout, stderr)
		}
	})
	t.Run("spawn_send accepted", func(t *testing.T) {
		t.Parallel()
		h, _, id := setup(t, warm)
		stdout, stderr, runErr := runToolShellCtx(t, h.ctx, binDir, h.spawner.SendTool(),
			`foci_spawn_send --id `+id+` --message go`)
		if runErr != nil || !strings.Contains(stdout, "resumed") {
			t.Errorf("run error = %v, stdout = %q, stderr = %q; want exit 0 and resumed", runErr, stdout, stderr)
		}
	})
}

// errorAsTextPattern matches a tool result written as an "Error..." or
// "NOT started/delivered..." string.
var errorAsTextPattern = regexp.MustCompile(`(TextResult\(|ToolResult\{Text: )(fmt\.Sprintf\()?"(Error|NOT )`)

// TestToolsDoNotReturnErrorsAsText keeps #2841 fixed for the next tool: a
// failure goes back as Execute's error, which the exec bridge turns into stderr
// and a non-zero exit, and the API path into an is_error tool result. A result
// that is text cannot be told from success by the bridge: it does not sniff for
// an "Error:" prefix, because a fetched page may legally start with one.
//
// disconnected-test-ok: scans this package's source text; it calls no symbol.
func TestToolsDoNotReturnErrorsAsText(t *testing.T) {
	t.Parallel()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if errorAsTextPattern.MatchString(line) {
				t.Errorf("%s:%d returns an error as text; return it as Execute's error: %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
