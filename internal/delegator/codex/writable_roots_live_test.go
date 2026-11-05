package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/procx"
	"github.com/BurntSushi/toml"
)

// TestWritableRootsLive probes the actual installed app-server without a model
// turn or the running Foci. Opt in with an absolute Codex binary and an existing
// scratch parent OUTSIDE /tmp and TMPDIR (both writable by default in Codex).
func TestWritableRootsLive(t *testing.T) {
	parent, bin := os.Getenv("FOCI_CODEX_LIVE_ROOT"), os.Getenv("FOCI_CODEX_BIN")
	if parent == "" || bin == "" {
		t.Skip("set FOCI_CODEX_LIVE_ROOT outside /tmp and FOCI_CODEX_BIN to probe an isolated app-server")
	}
	base, err := os.MkdirTemp(parent, "foci-roots-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	workspace, allowed, blocked := filepath.Join(base, "workspace"), filepath.Join(base, "allowed"), filepath.Join(base, "blocked")
	for _, dir := range []string{workspace, allowed, blocked} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var cfg config.Config
	text := "[sandbox]\nwritable_roots = [" + tomlBasicString(allowed) + "]\n"
	if _, err := toml.Decode(text, &cfg); err != nil {
		t.Fatal(err)
	}
	roots := config.Resolve(&cfg, config.AgentConfig{}).Sandbox.WritableRoots
	b := &Backend{cfg: map[string]any{"writable_roots": roots}}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	launch := func() (func(string, any) json.RawMessage, func()) {
		t.Helper()
		serverCtx, stop := context.WithCancel(ctx)
		cmd := procx.Spawn(serverCtx, procx.Operator, bin, b.appServerArgs("")...)
		cmd.Dir = workspace
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		closeServer := sync.OnceFunc(func() { stdin.Close(); _ = cmd.Wait(); stop() })
		t.Cleanup(closeServer)
		enc, dec := json.NewEncoder(stdin), json.NewDecoder(stdout)
		id := 0
		rpc := func(method string, params any) json.RawMessage {
			t.Helper()
			id++
			if err := enc.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
				t.Fatal(err)
			}
			for {
				var reply struct {
					ID     int             `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  json.RawMessage `json:"error"`
				}
				if err := dec.Decode(&reply); err != nil {
					t.Fatal(err)
				}
				if reply.ID != id {
					continue
				}
				if len(reply.Error) > 0 {
					t.Fatalf("%s: %s", method, reply.Error)
				}
				return reply.Result
			}
		}
		rpc("initialize", map[string]any{"clientInfo": map[string]string{"name": "foci-writable-roots-probe", "version": "1"}})
		if err := enc.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		return rpc, closeServer
	}
	rpc, closeServer := launch()
	start := func(method, thread string, roots, expected []string) string {
		t.Helper()
		b.cfg["writable_roots"] = roots
		// Use the production wire types so this probe also detects serialization drift.
		var params any
		switch method {
		case "thread/start":
			params = threadStartParams{Cwd: workspace, Sandbox: b.sandboxMode(), ApprovalsReviewer: b.approvalsReviewer(), Config: b.writableRootsConfig()}
		case "thread/resume":
			params = threadResumeParams{ThreadID: thread, Sandbox: b.sandboxMode(), ApprovalsReviewer: b.approvalsReviewer(), Config: b.writableRootsConfig()}
		case "thread/fork":
			params = threadForkParams{ThreadID: thread, Sandbox: b.sandboxMode(), Config: b.writableRootsConfig()}
		default:
			t.Fatalf("unexpected lifecycle method %q", method)
		}
		raw := rpc(method, params)
		var result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
			Sandbox sandboxPolicy `json:"sandbox"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if result.Sandbox.Type != "workspaceWrite" || !reflect.DeepEqual(result.Sandbox.WritableRoots, expected) {
			t.Fatalf("%s sandbox=%s", method, raw)
		}
		t.Logf("%s thread=%s requested=%v confirmed workspaceWrite roots=%v network=%v", method, result.Thread.ID, roots, result.Sandbox.WritableRoots, result.Sandbox.NetworkAccess)
		return result.Thread.ID
	}
	thread := start("thread/start", "", roots, roots)
	for _, dir := range []string{allowed, blocked} {
		target := filepath.Join(dir, "probe.txt")
		raw := rpc("command/exec", map[string]any{"cwd": workspace, "command": []string{"/bin/sh", "-c", `printf test > "$1" && cat "$1"`, "sh", target}})
		var result struct {
			ExitCode int    `json:"exitCode"`
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		_, statErr := os.Stat(target)
		if dir == allowed {
			if result.ExitCode != 0 || result.Stdout != "test" || statErr != nil {
				t.Fatalf("configured write/read failed: %s", raw)
			}
		} else if result.ExitCode == 0 || !os.IsNotExist(statErr) {
			t.Fatalf("unconfigured write was not blocked: %s", raw)
		}
		t.Logf("%s: %s", filepath.Base(dir), raw)
	}

	inject := func(thread string) {
		t.Helper()
		// Empty threads have no rollout and cannot be resumed on Codex 0.166.0.
		rpc("thread/inject_items", threadInjectItemsParams{ThreadID: thread, Items: []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Synthetic writable-root probe; no model turn requested."}}}}})
	}
	inject(thread)
	// Overrides affect loaded forks, but resume keeps their loaded roots.
	start("thread/resume", thread, []string{}, roots)
	fork := start("thread/fork", thread, []string{}, []string{})
	start("thread/resume", fork, roots, []string{})
	rootless := start("thread/start", "", []string{}, []string{})
	inject(rootless)
	addedFork := start("thread/fork", rootless, roots, roots)
	// Branches are unsubscribed and later resumed by another facade. Confirm
	// fork grants survive disk reload, not just an already-loaded thread.
	closeServer()
	var closeCold func()
	rpc, closeCold = launch()

	// Cold resume unions requested grants with saved grants; it cannot revoke.
	t.Log("cold app-server: distinct nonempty overrides")
	otherRoots := []string{blocked}
	start("thread/resume", thread, otherRoots, []string{allowed, blocked})
	start("thread/resume", fork, otherRoots, otherRoots)
	start("thread/resume", rootless, otherRoots, otherRoots)
	start("thread/resume", addedFork, otherRoots, []string{allowed, blocked})
	closeCold()
	rpc, _ = launch()
	t.Log("second cold app-server: empty overrides")
	start("thread/resume", thread, []string{}, roots)
	start("thread/resume", fork, []string{}, []string{})
	start("thread/resume", rootless, []string{}, []string{})
	start("thread/resume", addedFork, []string{}, roots)
}
