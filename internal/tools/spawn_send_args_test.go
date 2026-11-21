package tools

import (
	"strings"
	"testing"
)

// TestSpawnSendTakesSpawnAsFirstBareWord is the #3200 repro. foci_spawn_send
// took its target only as --id and the message as the bare words, the
// opposite of foci_send_to_session (<session_key> --message). So
// `foci_spawn_send todo-3159 --message "text"` failed with "message was
// already given positionally" (the spawn name had become the message), and
// `foci_spawn_send todo-3159 "text"` failed with the usage line. Now, when no
// --id is given, the first bare word is the spawn and the rest (or --message,
// or stdin) is the message. With --id given, every bare word is the message,
// as before.
func TestSpawnSendTakesSpawnAsFirstBareWord(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	s := NewSpawner(SpawnDeps{}, nil)
	cases := []struct {
		name, invocation string
		wantID, wantMsg  string
	}{
		{"name then --message", `foci_spawn_send todo-3159 --message "the text" </dev/null`, "todo-3159", "the text"},
		{"--message then name", `foci_spawn_send --message "the text" todo-3159 </dev/null`, "todo-3159", "the text"},
		{"name then quoted message", `foci_spawn_send todo-3159 "the text" </dev/null`, "todo-3159", "the text"},
		{"name then bare words", `foci_spawn_send todo-3159 the text </dev/null`, "todo-3159", "the text"},
		{"name then piped message", `printf 'piped' | foci_spawn_send todo-3159`, "todo-3159", "piped"},
		// The form that already worked keeps working.
		{"message then --id", `foci_spawn_send "the text" --id todo-3159 </dev/null`, "todo-3159", "the text"},
		{"--id then bare words", `foci_spawn_send --id todo-3159 the text </dev/null`, "todo-3159", "the text"},
		{"bare words then --id", `foci_spawn_send the text --id todo-3159 </dev/null`, "todo-3159", "the text"},
		{"--id and --message", `foci_spawn_send --id todo-3159 --message "the text" </dev/null`, "todo-3159", "the text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, out, code := spawnShellParams(t, s.SendTool(), c.invocation)
			if code != 0 || p == nil {
				t.Fatalf("exit %d, foci-call reached=%v\n%s", code, p != nil, out)
			}
			if p["id"] != c.wantID || p["message"] != c.wantMsg {
				t.Errorf("want id=%q message=%q, got %v\n%s", c.wantID, c.wantMsg, p, out)
			}
		})
	}
}

// TestSpawnSendRefusesAmbiguousArgs: a message given both ways, or a spawn
// with no message, fails before foci-call.
func TestSpawnSendRefusesAmbiguousArgs(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	s := NewSpawner(SpawnDeps{}, nil)
	cases := []struct{ name, invocation, wantErr string }{
		{"--id, bare word and --message", `foci_spawn_send --id x word --message t </dev/null`, "error:"},
		{"name, extra word and --message", `foci_spawn_send x word --message t </dev/null`, "error:"},
		{"name only", `foci_spawn_send x </dev/null`, "usage:"},
		{"nothing", `foci_spawn_send </dev/null`, "usage:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, out, code := spawnShellParams(t, s.SendTool(), c.invocation)
			if code == 0 || p != nil {
				t.Fatalf("expected a refusal before foci-call; exit %d, params %v\n%s", code, p, out)
			}
			if !strings.Contains(out, c.wantErr) {
				t.Errorf("output lacks %q\n%s", c.wantErr, out)
			}
		})
	}
}
