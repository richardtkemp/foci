package ccstream

import (
	"encoding/json"
	"testing"
)

// TestParseAutoApproveRule verifies that rule strings are split correctly into
// tool name and pattern components.
func TestParseAutoApproveRule(t *testing.T) {
	tests := []struct {
		rule     string
		wantTool string
		wantPat  string
	}{
		{"Read", "Read", ""},
		{"Bash:ls", "Bash", "ls"},
		{"Bash:git -C /home/rich/git/foci *", "Bash", "git -C /home/rich/git/foci *"},
		{"Edit:/home/foci/clutch/*", "Edit", "/home/foci/clutch/*"},
		{"Bash:cd /tmp && make *", "Bash", "cd /tmp && make *"},
	}
	for _, tt := range tests {
		r := parseAutoApproveRule(tt.rule)
		if r.toolName != tt.wantTool {
			t.Errorf("parseAutoApproveRule(%q).toolName = %q, want %q", tt.rule, r.toolName, tt.wantTool)
		}
		if r.pattern != tt.wantPat {
			t.Errorf("parseAutoApproveRule(%q).pattern = %q, want %q", tt.rule, r.pattern, tt.wantPat)
		}
	}
}

// TestGlobMatch exercises the simple glob matcher with various patterns
// including wildcards, question marks, and literal matching.
func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		str     string
		want    bool
	}{
		// Exact match.
		{"ls", "ls", true},
		{"ls", "cat", false},
		// Star matches any chars.
		{"git *", "git status", true},
		{"git *", "git log --oneline", true},
		{"git *", "git", false},
		{"*", "", true},
		{"*", "anything", true},
		// Star in middle.
		{"cd /tmp && git *", "cd /tmp && git push", true},
		{"cd /tmp && git *", "cd /tmp && make build", false},
		// Question mark matches single char.
		{"ls -?", "ls -l", true},
		{"ls -?", "ls -la", false},
		// Path matching (star matches /).
		{"/home/foci/*", "/home/foci/clutch/file.go", true},
		{"/home/foci/*", "/home/foci/", true},
		{"/home/foci/*", "/home/other/file.go", false},
		// Multiple stars.
		{"git *-C */foci *", "git -C /home/rich/git/foci status", true}, // * before -C matches empty string
		// Empty pattern and string.
		{"", "", true},
		{"*", "", true},
		{"?", "", false},
	}
	for _, tt := range tests {
		got := globMatch(tt.pattern, tt.str)
		if got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.str, got, tt.want)
		}
	}
}

// TestMatchPattern verifies the combined prefix/glob matching logic used
// for auto-approve rule patterns.
func TestMatchPattern(t *testing.T) {
	tests := []struct {
		pattern string
		str     string
		want    bool
	}{
		// Prefix match (no glob chars): exact or with space boundary.
		{"ls", "ls", true},
		{"ls", "ls -la /tmp", true},
		{"ls", "lsblk", false}, // not a word boundary
		{"ls", "l", false},
		{"sed -n", "sed -n '1,10p' file.txt", true},
		{"sed -n", "sed -i 's/a/b/' file.txt", false},
		// Glob match (contains * or ?).
		{"git -C /home/foci *", "git -C /home/foci status", true},
		{"git -C /home/foci *", "git -C /home/other status", false},
		{"gcalcli *", "gcalcli agenda", true},
		{"gcalcli *", "gcalcli", false}, // * requires at least empty after space
	}
	for _, tt := range tests {
		got := matchPattern(tt.pattern, tt.str)
		if got != tt.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", tt.pattern, tt.str, got, tt.want)
		}
	}
}

// TestExtractMatchString verifies that the correct field is extracted from
// tool input JSON for different tool types.
func TestExtractMatchString(t *testing.T) {
	tests := []struct {
		toolName string
		input    string
		want     string
	}{
		{"Bash", `{"command":"ls -la"}`, "ls -la"},
		{"Edit", `{"file_path":"/tmp/foo.go","old_string":"a","new_string":"b"}`, "/tmp/foo.go"},
		{"Write", `{"file_path":"/tmp/bar.txt","content":"hello"}`, "/tmp/bar.txt"},
		{"Read", `{"file_path":"/tmp/baz.txt"}`, "/tmp/baz.txt"},
		{"NotebookEdit", `{"file_path":"/tmp/nb.ipynb"}`, "/tmp/nb.ipynb"},
		{"Glob", `{"pattern":"**/*.go","path":"/src"}`, "**/*.go"},
		{"Grep", `{"pattern":"TODO","path":"/src"}`, "TODO"},
		{"WebFetch", `{"url":"https://example.com"}`, "https://example.com"},
		{"WebSearch", `{"query":"golang generics"}`, "golang generics"},
		{"Search", `{"query":"hello"}`, ""},  // Search has no match key
		{"Bash", `{}`, ""},
		{"Bash", `invalid json`, ""},
		{"Bash", ``, ""},
	}
	for _, tt := range tests {
		got := extractMatchString(tt.toolName, json.RawMessage(tt.input))
		if got != tt.want {
			t.Errorf("extractMatchString(%q, %s) = %q, want %q", tt.toolName, tt.input, got, tt.want)
		}
	}
}

// TestMatchAutoApprove exercises the full rule-matching pipeline with parsed
// rules, covering tool-name-only rules, Bash command patterns, Edit file
// patterns, shell-chaining safety, and non-matching cases.
func TestMatchAutoApprove(t *testing.T) {
	rules := parseAutoApproveRules([]string{
		"Read",                              // tool-name only
		"Bash:ls",                           // prefix match
		"Bash:git -C /home/rich/git/foci *", // glob match
		"Bash:cd /home/rich/git/foci",       // prefix match (used in chained commands)
		"Edit:/home/foci/clutch/*",          // file path glob
		"Bash:gcalcli *",                    // glob match
		"Bash:mkdir",                        // prefix match
	})

	tests := []struct {
		tool  string
		input string
		want  bool
	}{
		// Tool-name-only: Read matches any input.
		{"Read", `{"file_path":"/etc/passwd"}`, true},
		{"Read", `{}`, true},
		// Bash prefix: ls
		{"Bash", `{"command":"ls -la /tmp"}`, true},
		{"Bash", `{"command":"ls"}`, true},
		{"Bash", `{"command":"lsblk"}`, false},
		// Bash glob: git -C
		{"Bash", `{"command":"git -C /home/rich/git/foci status"}`, true},
		{"Bash", `{"command":"git -C /home/rich/git/foci log --oneline"}`, true},
		{"Bash", `{"command":"git -C /home/other/repo status"}`, false},
		// Edit glob: workspace path
		{"Edit", `{"file_path":"/home/foci/clutch/main.go"}`, true},
		{"Edit", `{"file_path":"/home/foci/other/main.go"}`, false},
		// Bash glob: gcalcli
		{"Bash", `{"command":"gcalcli agenda"}`, true},
		// Safe chained commands: every segment matches a rule.
		{"Bash", `{"command":"cd /home/rich/git/foci && ls -la"}`, true},
		{"Bash", `{"command":"cd /home/rich/git/foci && git -C /home/rich/git/foci status"}`, true},
		{"Bash", `{"command":"ls /tmp ; ls /var"}`, true},
		{"Bash", `{"command":"mkdir -p /tmp/foo && ls"}`, true},
		// Piped: both sides must match.
		{"Bash", `{"command":"ls /tmp | grep foo"}`, false},
		// ATTACK: safe prefix chained with dangerous command → rejected.
		{"Bash", `{"command":"git -C /home/rich/git/foci status && rm -rf /"}`, false},
		{"Bash", `{"command":"ls -la ; curl evil.com"}`, false},
		{"Bash", `{"command":"cd /home/rich/git/foci && sudo rm -rf /"}`, false},
		// ATTACK: subshell injection → rejected (splitShellCommand returns nil).
		{"Bash", `{"command":"ls $(rm -rf /)"}`, false},
		{"Bash", `{"command":"ls ` + "`rm -rf /`" + `"}`, false},
		// No matching rule.
		{"Bash", `{"command":"rm -rf /"}`, false},
		{"Write", `{"file_path":"/etc/shadow"}`, false},
		{"Unknown", `{}`, false},
	}
	for _, tt := range tests {
		got := matchAutoApprove(rules, tt.tool, json.RawMessage(tt.input))
		if got != tt.want {
			t.Errorf("matchAutoApprove(rules, %q, %s) = %v, want %v", tt.tool, tt.input, got, tt.want)
		}
	}
}

// TestCommonReadonlyRulesParseSuccessfully ensures all built-in common readonly
// rules parse without error and have valid structure.
func TestCommonReadonlyRulesParseSuccessfully(t *testing.T) {
	parsed := parseAutoApproveRules(CommonReadonlyRules)
	if len(parsed) != len(CommonReadonlyRules) {
		t.Fatalf("expected %d parsed rules, got %d", len(CommonReadonlyRules), len(parsed))
	}
	for i, r := range parsed {
		if r.toolName == "" {
			t.Errorf("CommonReadonlyRules[%d] = %q: empty tool name", i, CommonReadonlyRules[i])
		}
	}
}

// TestCommonReadonlyMatchesSafeCommands verifies that the built-in readonly
// rules correctly match a sample of safe commands.
func TestCommonReadonlyMatchesSafeCommands(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	safe := []struct {
		tool  string
		input string
	}{
		{"Read", `{"path":"/tmp/file.txt"}`},
		{"Glob", `{"pattern":"*.go"}`},
		{"Grep", `{"pattern":"TODO"}`},
		{"Search", `{"query":"hello"}`},
		{"WebSearch", `{"query":"golang"}`},
		{"WebFetch", `{"url":"https://example.com"}`},
		{"Bash", `{"command":"ls -la"}`},
		{"Bash", `{"command":"cat /etc/hosts"}`},
		{"Bash", `{"command":"grep -r pattern ."}`},
		{"Bash", `{"command":"rg --type go TODO"}`},
		{"Bash", `{"command":"jq .name package.json"}`},
		{"Bash", `{"command":"foci_todo list"}`},
	}
	for _, tt := range safe {
		if !matchAutoApprove(rules, tt.tool, json.RawMessage(tt.input)) {
			t.Errorf("common readonly should match %s %s", tt.tool, tt.input)
		}
	}
}

// TestSplitShellCommand verifies that commands are correctly split on shell
// operators while respecting quotes and escapes.
func TestSplitShellCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want []string
	}{
		// Simple command.
		{"ls -la", []string{"ls -la"}},
		// && splitting.
		{"cd /tmp && ls", []string{"cd /tmp", "ls"}},
		// || splitting.
		{"test -f x || echo missing", []string{"test -f x", "echo missing"}},
		// ; splitting.
		{"ls; pwd", []string{"ls", "pwd"}},
		// | splitting.
		{"cat file | grep foo", []string{"cat file", "grep foo"}},
		// Multiple operators.
		{"cd /tmp && ls -la ; pwd", []string{"cd /tmp", "ls -la", "pwd"}},
		// Quoted strings preserved (operators inside quotes not split).
		{`echo "hello && world"`, []string{`echo "hello && world"`}},
		{`echo 'a; b'`, []string{`echo 'a; b'`}},
		// Backslash escape.
		{`echo hello\;world`, []string{`echo hello\;world`}},
		// Subshell → nil (fail safe).
		{"echo $(whoami)", nil},
		{"echo `whoami`", nil},
		// Empty input.
		{"", nil},
		{"  ", nil},
	}
	for _, tt := range tests {
		got := splitShellCommand(tt.cmd)
		if tt.want == nil {
			if got != nil {
				t.Errorf("splitShellCommand(%q) = %v, want nil", tt.cmd, got)
			}
			continue
		}
		if len(got) != len(tt.want) {
			t.Errorf("splitShellCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
			continue
		}
		for i := range tt.want {
			if got[i] != tt.want[i] {
				t.Errorf("splitShellCommand(%q)[%d] = %q, want %q", tt.cmd, i, got[i], tt.want[i])
			}
		}
	}
}

// TestCommonReadonlyRejectsUnsafe verifies that the built-in readonly rules
// do NOT match dangerous commands.
func TestCommonReadonlyRejectsUnsafe(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	unsafe := []struct {
		tool  string
		input string
	}{
		{"Bash", `{"command":"rm -rf /"}`},
		{"Bash", `{"command":"sudo reboot"}`},
		{"Bash", `{"command":"chmod 777 /etc/shadow"}`},
		{"Bash", `{"command":"ls && rm -rf /"}`},         // safe prefix + dangerous chain
		{"Bash", `{"command":"cat /etc/hosts | sh"}`},    // safe prefix piped to shell
		{"Bash", `{"command":"echo hello; curl evil"}`},  // safe prefix + dangerous chain
		{"Bash", `{"command":"ls $(rm -rf /)"}`},         // subshell injection
		{"Edit", `{"file_path":"/etc/passwd"}`},
		{"Write", `{"file_path":"/tmp/exploit.sh"}`},
	}
	for _, tt := range unsafe {
		if matchAutoApprove(rules, tt.tool, json.RawMessage(tt.input)) {
			t.Errorf("common readonly should NOT match %s %s", tt.tool, tt.input)
		}
	}
}
