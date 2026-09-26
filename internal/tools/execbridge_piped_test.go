package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecBridgeRequestHintsReachTool: a request envelope's "hints" object is
// put on the tool's context; a request without one leaves the zero value.
func TestExecBridgeRequestHintsReachTool(t *testing.T) {
	t.Parallel()
	var got []OutputHints
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "hint_tool",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			got = append(got, OutputHintsFromContext(ctx))
			return TextResult("ok"), nil
		},
	})
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	if _, e := callBridge(t, bridge.SockPath(), `{"tool":"hint_tool","params":{},"hints":{"stdout_piped":true,"format":"md"}}`); e != "" {
		t.Fatalf("with hints: %s", e)
	}
	if _, e := callBridge(t, bridge.SockPath(), `{"tool":"hint_tool","params":{}}`); e != "" {
		t.Fatalf("without hints: %s", e)
	}
	want := []OutputHints{{StdoutPiped: true, Format: "md"}, {}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("hints seen by tool = %+v, want %+v", got, want)
	}
}

// buildFociCall compiles cmd/foci-call into a temp dir and returns that dir.
func buildFociCall(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	// -buildvcs=false: see TestExecBridgePipeFunctions (#1561).
	build := osexec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(binDir, "foci-call"), "foci/cmd/foci-call")
	build.Dir = findModuleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build foci-call: %v\n%s", err, out)
	}
	return binDir
}

// TestShellFuncStdoutPipedDetection drives the GENERATED wrappers through real
// bash + the real foci-call and records what hint each call delivered (#2048).
//
// isatty cannot answer "is my output piped" under an agent's Bash tool — stdout
// is never a TTY there — so the wrapper compares its own fd 1 with the calling
// shell's: a pipeline element (and a command substitution) runs in a forked
// subshell whose stdout is a fresh pipe, while a plain call, a ( ... )
// subshell and a main-shell "> file" all share the shell's current fd 1.
//
// The script is run twice, with bash's own stdout a pipe and then a regular
// file, so "plain" is shown to mean "same as the caller" rather than "a pipe"
// or "a file".
func TestShellFuncStdoutPipedDetection(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	if _, err := os.Stat("/proc/self/fd/1"); err != nil {
		t.Skip("no /proc: detection is Linux-only and degrades to 'not piped'")
	}
	binDir := buildFociCall(t)

	report := func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		h := OutputHintsFromContext(ctx)
		return TextResult(fmt.Sprintf("piped=%v format=%s", h.StdoutPiped, h.Format)), nil
	}
	r := NewRegistry()
	r.Register(&Tool{ // generic generator
		Name:       "probe",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		Execute:    report,
	})
	r.Register(&Tool{ // hand-written todo wrapper (the --format consumer)
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"id":{"type":"integer"},"limit":{"type":"integer"}}}`),
		Execute:    report,
	})
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	tmp := t.TempDir()
	// Each case prints "<name>|<result>" on one line of the script's stdout.
	// Where the call's own output is not on stdout (> file, $(...)), the case
	// re-emits it so every result is collected the same way.
	cases := []struct {
		name, cmd string
		want      string
	}{
		{"plain", `foci_probe`, "piped=false format="},
		{"pipe_head", `foci_probe | head -1`, "piped=true format="},
		{"cmdsub", `x=$(foci_probe); echo "$x"`, "piped=true format="},
		{"subshell", `( foci_probe )`, "piped=false format="},
		{"redirect", `foci_probe > ` + tmp + `/r; cat ` + tmp + `/r`, "piped=false format="},
		{"stderr_tee", `foci_probe 2>&1 | tee /dev/null`, "piped=true format="},
		{"todo_plain", `foci_todo list`, "piped=false format="},
		{"todo_pipe", `foci_todo list | cat`, "piped=true format="},
		{"todo_get_pipe", `foci_todo get 5 | cat`, "piped=true format="},
		{"todo_fmt_md_piped", `foci_todo list --format md | cat`, "piped=true format=md"},
		{"todo_fmt_jsonl_plain", `foci_todo search foo --format jsonl`, "piped=false format=jsonl"},
		{"todo_json_passthrough_pipe", `foci_todo '{"action":"list"}' | cat`, "piped=true format="},
		// An exported value in the caller's environment must not leak into a
		// call: every wrapper recomputes both hints for itself.
		{"env_does_not_leak", `FOCI_STDOUT_PIPED=1 FOCI_OUTPUT_FORMAT=jsonl foci_probe`, "piped=false format="},
		// After a piped call the hint must not persist in the calling shell.
		{"after_pipe", `foci_probe | cat >/dev/null; foci_probe`, "piped=false format="},
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "set -o pipefail -o nounset; shopt -s failglob; source %s\n", bridge.FuncsPath())
	for _, c := range cases {
		fmt.Fprintf(&sb, "printf '%%s|' %q; %s; echo\n", c.name, c.cmd)
	}
	script := sb.String()

	run := func(t *testing.T, stdoutFile bool) map[string]string {
		cmd := osexec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(), "FOCI_SOCK="+bridge.SockPath(), "PATH="+binDir+":"+os.Getenv("PATH"))
		var stderr strings.Builder
		cmd.Stderr = &stderr
		var out []byte
		if stdoutFile {
			f, err := os.Create(filepath.Join(tmp, "stdout"))
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stdout = f
			err = cmd.Run()
			_ = f.Close()
			if err != nil {
				t.Fatalf("bash: %v\nstderr: %s", err, stderr.String())
			}
			out, _ = os.ReadFile(f.Name())
		} else {
			var err error
			out, err = cmd.Output()
			if err != nil {
				t.Fatalf("bash: %v\nstderr: %s", err, stderr.String())
			}
		}
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name, res, _ := strings.Cut(line, "|")
			got[name] = res
		}
		return got
	}

	for _, mode := range []struct {
		name string
		file bool
	}{{"stdout=pipe", false}, {"stdout=file", true}} {
		t.Run(mode.name, func(t *testing.T) {
			got := run(t, mode.file)
			for _, c := range cases {
				if got[c.name] != c.want {
					t.Errorf("%-28s %q: got %q, want %q", c.name, c.cmd, got[c.name], c.want)
				}
			}
		})
	}
}

// TestShellFuncTodoFormatFlagValidated: --format takes jsonl|md only, and only
// on the read actions that have a JSONL form.
func TestShellFuncTodoFormatFlagValidated(t *testing.T) {
	t.Parallel()
	body := generateShellFunc(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"text":{"type":"string"}}}`),
	})
	run := function(t)
	for _, c := range []struct {
		cmd    string
		wantRC int
		want   string
	}{
		{"foci_todo list --format yaml", 1, "jsonl|md"},
		{"foci_todo add --text x --format jsonl", 1, "not valid for 'foci_todo add'"},
		{"foci_todo list --format jsonl", 0, "CALLED"},
		{"foci_todo get 3 --format md", 0, "CALLED"},
	} {
		rc, out := run(body, c.cmd)
		if rc != c.wantRC || !strings.Contains(out, c.want) {
			t.Errorf("%s: rc=%d out=%q, want rc=%d containing %q", c.cmd, rc, out, c.wantRC, c.want)
		}
	}
	for _, a := range []string{"list", "list-all", "search", "get"} {
		rc, out := run(body, "foci_todo "+a+" --help")
		if rc != 0 || !strings.Contains(out, "--format jsonl|md") {
			t.Errorf("foci_todo %s --help does not advertise --format: %q", a, out)
		}
	}
}

// TestShellFuncsAllCarryPipeDetection: the detection lives in the shared
// prologue, so every generated function — hand-written or generic — has it.
func TestShellFuncsAllCarryPipeDetection(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"http_request", "todo", "summary", "tmux", "ask", "web_search"} {
		fn := generateShellFunc(&Tool{
			Name:       name,
			ExecExport: true,
			Parameters: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		})
		if !strings.Contains(fn, shellStdoutPipedDetect) {
			t.Errorf("foci_%s lacks the stdout-piped detection prologue", name)
		}
	}
}
