package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runStubbedShellFunc runs cmd against a generated function with the real foci__json
// helper and a stub foci-call that prints the output format it would forward
// and the request it was given.
func runStubbedShellFunc(t *testing.T, body, cmd string) (int, string) {
	t.Helper()
	script := jsonPassthroughHelper +
		"foci-call() { printf 'fmt=%s req=%s\\n' \"$FOCI_OUTPUT_FORMAT\" \"$1\"; }\n" +
		body + "\n" + cmd + "\n"
	c := osexec.Command("bash", "-c", script)
	out, err := c.CombinedOutput()
	rc := 0
	if ee, ok := err.(*osexec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("bash: %v", err)
	}
	return rc, string(out)
}

// TestShellFuncJSONOutputFlag: --json (#1215) is accepted in any position by
// the generic and the hand-written wrappers, sets the forwarded output format,
// and is removed before parsing so every other argument arrives intact.
// foci_ask keeps --json as its INPUT flag.
func TestShellFuncJSONOutputFlag(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	probe := &Tool{
		Name: "probe", ExecExport: true, Positional: []string{"query"},
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"n":{"type":"integer"}}}`),
	}
	summary := &Tool{
		Name: "summary", ExecExport: true, Positional: []string{"prompt"},
		Parameters: json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"prompt":{"type":"string"}}}`),
	}
	todo := &Tool{
		Name: "todo", ExecExport: true, Positional: []string{"action"},
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"text":{"type":"string"},"limit":{"type":"integer"}}}`),
	}
	ask := &Tool{
		Name: "ask", ExecExport: true, Positional: []string{"questions"},
		Parameters: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array"}}}`),
	}
	for _, c := range []struct {
		tool       *Tool
		cmd        string
		wantFormat string
		wantReq    string // substring of the forwarded request
	}{
		{probe, `foci_probe hello world`, "", `"query":"hello world"`},
		{probe, `foci_probe --json hello world`, "json", `"query":"hello world"`},
		{probe, `foci_probe hello --n 3 --json`, "json", `"n":3`},
		{probe, `foci_probe --json '{"query":"x"}'`, "json", `"params":{"query":"x"}`},
		{summary, `foci_summary --json "sum it" --file /tmp/f`, "json", `"prompt":"sum it"`},
		{todo, `foci_todo list --json --limit 2`, "json", `"limit":2`},
		{todo, `foci_todo add --json --text hi`, "json", `"text":"hi"`},
		{todo, `foci_todo list --format md --json`, "md", `"action":"list"`}, // the later flag wins
		{ask, `foci_ask --json '{"questions":[]}'`, "", `"questions":[]`},
	} {
		rc, out := runStubbedShellFunc(t, generateShellFunc(c.tool), c.cmd)
		if rc != 0 || !strings.Contains(out, "fmt="+c.wantFormat+" ") || !strings.Contains(out, c.wantReq) {
			t.Errorf("%s: rc=%d out=%q, want fmt=%q and request containing %q", c.cmd, rc, out, c.wantFormat, c.wantReq)
		}
	}
}

// TestShellFuncsAllCarryJSONFlag: the --json output flag lives in the shared
// prologue, so every generated function has it — except ask, where --json is
// the input flag — and --help documents it.
func TestShellFuncsAllCarryJSONFlag(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"http_request", "todo", "summary", "tmux", "ask", "web_search", "send_to_chat", "memory_search", "web_fetch", "spawn"} {
		tool := &Tool{
			Name:       name,
			ExecExport: true,
			Parameters: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		}
		fn := generateShellFunc(tool)
		help := generateHelpText(tool)
		want := name != "ask"
		if got := strings.Contains(fn, shellJSONFlagStrip); got != want {
			t.Errorf("foci_%s carries the --json output flag = %v, want %v", name, got, want)
		}
		if got := strings.Contains(help, "--json (flag, any position)"); got != want {
			t.Errorf("foci_%s --help documents --json = %v, want %v", name, got, want)
		}
	}
	custom := generateHelpText(&Tool{Name: "x", ExecExport: true, JSONOutput: "SHAPE-DOC",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`)})
	if !strings.Contains(custom, "SHAPE-DOC") || strings.Contains(custom, genericJSONOutput) {
		t.Errorf("a tool's JSONOutput should replace the generic shape in --help:\n%s", custom)
	}
}

// rawBridgeCall returns the whole response envelope, result_file included.
func rawBridgeCall(t *testing.T, sockPath, request string) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "%s\n", request)
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() {
		t.Fatalf("read response: %v", sc.Err())
	}
	var env map[string]any
	if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return env
}

// TestExecBridgeJSONOutputWrapping: with hints.format=json, a text-only tool's
// result is wrapped as {"result": ...} (a spilled one names its file instead of
// having it streamed raw), a tool's own JSON passes through untouched, and
// without the hint nothing changes.
func TestExecBridgeJSONOutputWrapping(t *testing.T) {
	t.Parallel()
	spill := filepath.Join(t.TempDir(), "spill")
	if err := os.WriteFile(spill, []byte("FULL"), 0600); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	reg := func(name string, fn func(ctx context.Context) (ToolResult, error)) {
		r.Register(&Tool{Name: name, ExecExport: true,
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:    func(ctx context.Context, _ json.RawMessage) (ToolResult, error) { return fn(ctx) }})
	}
	reg("text", func(context.Context) (ToolResult, error) { return TextResult("a <b> & c\n"), nil })
	reg("spilled", func(context.Context) (ToolResult, error) {
		return ToolResult{Text: "preview", ResultFile: spill, ResultSize: 4}, nil
	})
	reg("native", func(ctx context.Context) (ToolResult, error) {
		if WantsJSON(ctx) {
			return JSONResult([]int{1, 2})
		}
		return TextResult("one two"), nil
	})
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	call := func(tool string, asJSON bool) map[string]any {
		req := fmt.Sprintf(`{"tool":%q,"params":{}}`, tool)
		if asJSON {
			req = fmt.Sprintf(`{"tool":%q,"params":{},"hints":{"format":"json"}}`, tool)
		}
		return rawBridgeCall(t, bridge.SockPath(), req)
	}
	for _, c := range []struct {
		tool   string
		asJSON bool
		want   string // the envelope's result
		file   string // the envelope's result_file
	}{
		{"text", false, "a <b> & c\n", ""},
		{"text", true, `{"result":"a <b> & c\n"}` + "\n", ""},
		{"spilled", false, "preview", spill},
		{"spilled", true, `{"result":"preview","result_file":"` + spill + `","result_size":4}` + "\n", ""},
		{"native", false, "one two", ""},
		{"native", true, "[1,2]\n", ""},
	} {
		env := call(c.tool, c.asJSON)
		got, _ := env["result"].(string)
		file, _ := env["result_file"].(string)
		if got != c.want || file != c.file {
			t.Errorf("%s json=%v: result=%q result_file=%q, want %q / %q", c.tool, c.asJSON, got, file, c.want, c.file)
		}
	}
}
