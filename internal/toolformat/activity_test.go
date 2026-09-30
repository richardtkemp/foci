package toolformat

import "testing"

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
		{"foci command wins over description", "Bash",
			`{"command":"foci_todo get 1110","description":"Fetch the ticket"}`, "Bash: foci_todo"},
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
