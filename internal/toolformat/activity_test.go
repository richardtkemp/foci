package toolformat

import (
	"strings"
	"testing"
)

// TestActivityLabel pins the running-tool caption for a shell call (#1110): a
// delegated agent runs every foci tool through its shell, so the bare tool name
// ("Bash") says nothing about what is running.
func TestActivityLabel(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"description wins over foci command", "Bash",
			`{"command":"make deploy; foci_todo complete 1","description":"Deploy clients and close #1"}`, "Bash: Deploy clients and close #1"},
		{"foci command when no description", "Bash",
			`{"command":"foci_todo get 1110"}`, "Bash: foci_todo"},
		{"several foci commands, deduped, in order", "Bash",
			`{"command":"foci_todo get 1 && foci_ask '{}' ; foci_todo list"}`, "Bash: foci_todo, foci_ask"},
		{"foci command after a pipe and in a subshell", "Bash",
			`{"command":"echo x | foci_send_to_chat; (foci_remind now)"}`, "Bash: foci_send_to_chat, foci_remind"},
		{"description when no foci command", "Bash",
			`{"command":"git status","description":"Show working tree status"}`, "Bash: Show working tree status"},
		{"long description truncated", "Bash",
			`{"command":"x","description":"` + longDesc + `"}`, "Bash: " + Truncate(longDesc, activityDetailMax)},
		{"neither: bare tool name", "Bash", `{"command":"git status"}`, "Bash"},
		{"foci_ inside a word is not a command", "Bash",
			`{"command":"cat /tmp/my_foci_notes FOCI_SOCK=x ls $foci_x"}`, "Bash"},
		{"codex passes the raw command, not JSON", "bash", `foci_todo list --limit 5`, "bash: foci_todo"},
		{"api shell tool", "shell", `{"command":"foci_memory_search foo"}`, "shell: foci_memory_search"},
		{"non-shell tool unchanged", "Read", `{"file_path":"/x","description":"y"}`, "Read"},
		{"no args", "Bash", ``, "Bash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ActivityLabel(tt.tool, []byte(tt.args)); got != tt.want {
				t.Errorf("ActivityLabel(%q, %s) = %q, want %q", tt.tool, tt.args, got, tt.want)
			}
		})
	}
}

const longDesc = "Run the entire integration suite against the staging database with verbose output"

// TestFullCommand pins the stop dialog's full-command text (#2138): a shell
// call's command line, any other tool's JSON input, nothing for an empty one,
// and a cap on a huge script.
func TestFullCommand(t *testing.T) {
	cases := []struct{ name, tool, args, want string }{
		{"shell command", "Bash", `{"command":"make test","description":"Run tests"}`, "make test"},
		{"codex raw line", "bash", `ls -la`, "ls -la"},
		{"other tool input", "Read", `{"file_path":"/x"}`, `{"file_path":"/x"}`},
		{"empty input", "Read", `{}`, ""},
	}
	for _, c := range cases {
		if got := FullCommand(c.tool, []byte(c.args)); got != c.want {
			t.Errorf("%s: FullCommand = %q, want %q", c.name, got, c.want)
		}
	}
	long := FullCommand("Bash", []byte(`{"command":"`+strings.Repeat("x", fullCommandMax+50)+`"}`))
	if len(long) > fullCommandMax+len("\n…") || !strings.HasSuffix(long, "…") {
		t.Errorf("long command not capped: len=%d", len(long))
	}
}
