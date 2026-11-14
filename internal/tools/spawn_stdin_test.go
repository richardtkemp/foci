package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// spawnShellParams runs invocation against the generated shell function of
// tool and returns the params it sent to foci-call (nil when it did not
// reach it), the combined output and the exit code.
func spawnShellParams(t *testing.T, tool *Tool, invocation string) (map[string]any, string, int) {
	t.Helper()
	out, code := runShellFunc(t, generateShellFunc(tool), invocation)
	_, payload, ok := strings.Cut(out, "FOCI_CALL_REACHED:")
	if !ok {
		return nil, out, code
	}
	var call struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &call); err != nil {
		t.Fatalf("decode foci-call payload: %v\n%s", err, out)
	}
	return call.Params, out, code
}

// TestSpawnShellReadsStdinAsPrompt is the #3107 repro: `foci_spawn --name x
// <<'EOF' ... EOF` and `foci_spawn_send --id x <<'EOF' ... EOF` exited 1 with
// the usage line, because neither wrapper read stdin. Like foci_todo add
// (#3013) and foci_send_to_chat, piped stdin is the prompt/message when no
// positional or flag gives one, so a long multi-line text with apostrophes
// can go through a quoted heredoc.
func TestSpawnShellReadsStdinAsPrompt(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	s := NewSpawner(SpawnDeps{}, nil)
	const body = "line one\nit's line two"
	heredoc := " <<'EOF'\nline one\nit's line two\nEOF"
	cases := []struct {
		name       string
		tool       *Tool
		invocation string
		key, want  string
	}{
		{"spawn heredoc", s.SpawnTool(), "foci_spawn --name x --background" + heredoc, "prompt", body},
		{"spawn pipe", s.SpawnTool(), "printf 'p' | foci_spawn --name x", "prompt", "p"},
		{"spawn positional, empty stdin", s.SpawnTool(), "foci_spawn hello there --name x </dev/null", "prompt", "hello there"},
		{"spawn --prompt -", s.SpawnTool(), "printf 'p' | foci_spawn --prompt - --name x", "prompt", "p"},
		{"spawn_send heredoc", s.SendTool(), "foci_spawn_send --id x" + heredoc, "message", body},
		{"spawn_send positional, empty stdin", s.SendTool(), "foci_spawn_send hi --id x </dev/null", "message", "hi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, out, code := spawnShellParams(t, c.tool, c.invocation)
			if code != 0 || p == nil {
				t.Fatalf("exit %d, foci-call reached=%v\n%s", code, p != nil, out)
			}
			if p[c.key] != c.want {
				t.Errorf("want %s=%q, got %v\n%s", c.key, c.want, p, out)
			}
		})
	}
}

// TestSpawnShellRejectsStdinBesidePrompt: when the prompt or message already
// came as an argument, piped content would be dropped, so the call fails
// loudly and never reaches foci-call (the generic StdinParam guard). Empty
// stdin, with no prompt at all, still gets the usage line.
func TestSpawnShellRejectsStdinBesidePrompt(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	s := NewSpawner(SpawnDeps{}, nil)
	cases := []struct {
		name       string
		tool       *Tool
		invocation string
		wantErr    string
	}{
		{"spawn positional + pipe", s.SpawnTool(), "printf 'lost' | foci_spawn hi --name x", "stdin"},
		{"spawn --prompt + pipe", s.SpawnTool(), "printf 'lost' | foci_spawn --prompt hi --name x", "stdin"},
		{"spawn_send positional + pipe", s.SendTool(), "printf 'lost' | foci_spawn_send hi --id x", "stdin"},
		{"spawn nothing at all", s.SpawnTool(), "foci_spawn --name x </dev/null", "usage:"},
		{"spawn_send nothing at all", s.SendTool(), "foci_spawn_send --id x </dev/null", "usage:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, out, code := spawnShellParams(t, c.tool, c.invocation)
			if code == 0 || p != nil {
				t.Fatalf("expected a refusal before foci-call; exit %d, params %v\n%s", code, p, out)
			}
			if !strings.Contains(out, c.wantErr) {
				t.Errorf("output lacks %q\n%s", c.wantErr, out)
			}
		})
	}
}
