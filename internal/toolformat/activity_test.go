package toolformat

import (
	"strings"
	"testing"
)

// TestActivityLabel pins the running-tool caption for a shell call (#1110): a
// delegated agent runs every foci tool through its shell, so the bare tool name
// ("Bash") says nothing about what is running. The caption carries no "Bash: "
// prefix (#2950): it is the description or the foci commands alone.
func TestActivityLabel(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"description wins over foci command", "Bash",
			`{"command":"make deploy; foci_todo complete 1","description":"Deploy clients and close #1"}`, "Deploy clients and close #1"},
		{"foci command when no description", "Bash",
			`{"command":"foci_todo get 1110"}`, "foci_todo"},
		{"several foci commands, deduped, in order", "Bash",
			`{"command":"foci_todo get 1 && foci_ask '{}' ; foci_todo list"}`, "foci_todo, foci_ask"},
		{"foci command after a pipe and in a subshell", "Bash",
			`{"command":"echo x | foci_send_to_chat; (foci_remind now)"}`, "foci_send_to_chat, foci_remind"},
		{"description when no foci command", "Bash",
			`{"command":"git status","description":"Show working tree status"}`, "Show working tree status"},
		{"long description truncated", "Bash",
			`{"command":"x","description":"` + longDesc + `"}`, Truncate(longDesc, activityDetailMax)},
		{"neither: the command itself (#3118)", "Bash", `{"command":"git status"}`, "git status"},
		{"foci_ inside a word is not a command", "Bash",
			`{"command":"cat /tmp/my_foci_notes FOCI_SOCK=x ls $foci_x"}`, "cat /tmp/my_foci_notes FOCI_SOCK=x ls $foci_x"},
		{"codex passes the raw command, not JSON", "bash", `foci_todo list --limit 5`, "foci_todo"},
		{"codex raw command: first non-blank line, cut", "bash",
			"\n  git fetch -q origin && git switch -c " + longDesc + "\necho done",
			Truncate("git fetch -q origin && git switch -c "+longDesc, activityDetailMax)},
		{"blank command: bare tool name", "bash", "  \n ", "bash"},
		{"api shell tool", "shell", `{"command":"foci_memory_search foo"}`, "foci_memory_search"},
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

// TestUndescribedShellCommand pins which calls the app asks the fast model to
// describe (#3118, Q48): a shell call with no description of its own.
func TestUndescribedShellCommand(t *testing.T) {
	tests := []struct {
		name, tool, args, want string
	}{
		{"codex raw command", "bash", "  git status\n", "git status"},
		{"CC Bash without description", "Bash", `{"command":"ls -la"}`, "ls -la"},
		{"CC Bash with description is unchanged", "Bash", `{"command":"ls","description":"List files"}`, ""},
		{"blank description counts as none", "Bash", `{"command":"ls","description":"  "}`, "ls"},
		{"foci command still described", "bash", "foci_todo list", "foci_todo list"},
		{"non-shell tool", "Read", `{"file_path":"/x"}`, ""},
		{"blank command", "bash", "  ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UndescribedShellCommand(tt.tool, []byte(tt.args)); got != tt.want {
				t.Errorf("UndescribedShellCommand(%q, %q) = %q, want %q", tt.tool, tt.args, got, tt.want)
			}
		})
	}
}

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
