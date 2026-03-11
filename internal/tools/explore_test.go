package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLsBasic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0644)
	os.WriteFile(filepath.Join(dir, "world.txt"), []byte("there"), 0644)

	tool := NewLsTool()
	params, _ := json.Marshal(map[string]string{"path": dir})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"hello.txt") {
		t.Errorf("expected hello.txt in output, got %q", result.Text)
	}
	if !strings.Contains(result.Text,"world.txt") {
		t.Errorf("expected world.txt in output, got %q", result.Text)
	}
}

func TestLsFlags(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("data"), 0644)

	tool := NewLsTool()
	params, _ := json.Marshal(map[string]string{
		"path":   dir,
		"params": "-la",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// -l includes permissions, -a includes . and ..
	if !strings.Contains(result.Text,"test.txt") {
		t.Errorf("expected test.txt in output, got %q", result.Text)
	}
}

func TestLsNonexistentPath(t *testing.T) {
	t.Parallel()
	tool := NewLsTool()
	params, _ := json.Marshal(map[string]string{"path": "/nonexistent/path/xyz"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v (should return error in output)", err)
	}
	if !strings.Contains(result.Text,"Error:") {
		t.Errorf("expected error in result for nonexistent path, got %q", result.Text)
	}
}

func TestFindByName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "bar.txt"), []byte("text"), 0644)

	tool := NewFindTool()
	params, _ := json.Marshal(map[string]string{
		"path":   dir,
		"params": `-name "*.go"`,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"foo.go") {
		t.Errorf("expected foo.go in output, got %q", result.Text)
	}
	if strings.Contains(result.Text,"bar.txt") {
		t.Errorf("should not contain bar.txt, got %q", result.Text)
	}
}

func TestFindByType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data"), 0644)

	tool := NewFindTool()
	params, _ := json.Marshal(map[string]string{
		"path":   dir,
		"params": "-type d",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"subdir") {
		t.Errorf("expected subdir in output, got %q", result.Text)
	}
}

func TestFindMaxdepth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0755)
	os.WriteFile(filepath.Join(dir, "a", "b", "deep.txt"), []byte("deep"), 0644)
	os.WriteFile(filepath.Join(dir, "shallow.txt"), []byte("shallow"), 0644)

	tool := NewFindTool()
	params, _ := json.Marshal(map[string]string{
		"path":   dir,
		"params": "-maxdepth 1 -type f",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"shallow.txt") {
		t.Errorf("expected shallow.txt in output, got %q", result.Text)
	}
	if strings.Contains(result.Text,"deep.txt") {
		t.Errorf("should not contain deep.txt, got %q", result.Text)
	}
}

func TestFindBlockedExec(t *testing.T) {
	t.Parallel()
	tool := NewFindTool()
	for _, blocked := range []string{"-exec", "-execdir", "-delete", "-fls"} {
		params, _ := json.Marshal(map[string]string{
			"path":   "/tmp",
			"params": blocked + " rm {} \\;",
		})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Errorf("%s should have been blocked", blocked)
		}
		if !strings.Contains(err.Error(), "blocked predicate") {
			t.Errorf("%s error = %q, want 'blocked predicate'", blocked, err.Error())
		}
	}
}

func TestGrepBasicMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello world\nfoo bar\nhello again"), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "hello",
		"path":    filepath.Join(dir, "test.txt"),
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"hello") {
		t.Errorf("expected 'hello' in output, got %q", result.Text)
	}
}

func TestGrepParams(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("Hello World\nhello lower\nHELLO UPPER"), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "hello",
		"path":    filepath.Join(dir, "test.txt"),
		"params":  "-i -c",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// -i -c should count all 3 matches
	if !strings.Contains(result.Text,"3") {
		t.Errorf("expected count of 3, got %q", result.Text)
	}
}

func TestGrepContextLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "line1\nline2\ntarget\nline4\nline5\n"
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "target",
		"path":    filepath.Join(dir, "test.txt"),
		"params":  "-C 1",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"line2") {
		t.Errorf("expected context line 'line2', got %q", result.Text)
	}
	if !strings.Contains(result.Text,"line4") {
		t.Errorf("expected context line 'line4', got %q", result.Text)
	}
}

func TestGrepNoMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello world"), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "nonexistent_pattern_xyz",
		"path":    filepath.Join(dir, "test.txt"),
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"(no matches)") {
		t.Errorf("expected '(no matches)', got %q", result.Text)
	}
}

func TestGrepRejectedParams(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello world"), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "hello",
		"path":    filepath.Join(dir, "test.txt"),
		"params":  "-i --badopt",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"--badopt was ignored") {
		t.Errorf("expected notice about --badopt, got %q", result.Text)
	}
	if !strings.Contains(result.Text,"hello") {
		t.Errorf("expected match output, got %q", result.Text)
	}
}

func TestGrepGlobFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.go"), []byte("package main\nfunc hello()"), 0644)
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello text"), 0644)

	binary, name := resolveGrepBinary()
	if name != "rg" && name != "grep" {
		t.Skipf("--glob test only meaningful for rg/grep, have %s", name)
	}

	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "hello",
		"path":    dir,
		"params":  "--glob=*.go",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"hello") {
		t.Errorf("expected match, got %q", result.Text)
	}
}

func TestResolveGrepBinary(t *testing.T) {
	t.Parallel()
	path, name := resolveGrepBinary()
	if path == "" {
		t.Error("resolveGrepBinary returned empty path")
	}
	if name == "" {
		t.Error("resolveGrepBinary returned empty name")
	}
	// Must be one of the expected binaries
	valid := map[string]bool{"rg": true, "ack": true, "ag": true, "grep": true}
	if !valid[name] {
		t.Errorf("unexpected binary name: %q", name)
	}
}

func TestTranslateGrepFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		params   string
		binary   string
		wantArgs []string
		wantNote bool // expect at least one notice
	}{
		{"simple -i", "-i", "rg", []string{"-i"}, false},
		{"combined -inl", "-inl", "rg", []string{"-i", "-n", "-l"}, false},
		{"context -C 3", "-C 3", "rg", []string{"-C", "3"}, false},
		{"attached context -C3", "-C3", "rg", []string{"-C", "3"}, false},
		{"-F for ack", "-F", "ack", []string{"-Q"}, false},
		{"-F for rg", "-F", "rg", []string{"-F"}, false},
		{"--hidden for rg", "--hidden", "rg", []string{"--hidden"}, false},
		{"--hidden for grep", "--hidden", "grep", nil, false},
		{"--glob for rg", "--glob=*.go", "rg", []string{"--glob=*.go"}, false},
		{"--glob for grep", "--glob=*.go", "grep", []string{"--include=*.go"}, false},
		{"--glob for ack", "--glob=*.go", "ack", nil, true},
		{"unknown flag", "--foobar", "rg", nil, true},
		{"unknown short", "-Z", "rg", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, notices := translateGrepFlags(tt.params, tt.binary)
			if len(tt.wantArgs) == 0 && len(args) != 0 {
				t.Errorf("expected no args, got %v", args)
			}
			for i, want := range tt.wantArgs {
				if i >= len(args) {
					t.Errorf("missing arg at index %d: want %q", i, want)
					continue
				}
				if args[i] != want {
					t.Errorf("arg[%d] = %q, want %q", i, args[i], want)
				}
			}
			if tt.wantNote && len(notices) == 0 {
				t.Error("expected at least one notice, got none")
			}
			if !tt.wantNote && len(notices) > 0 {
				t.Errorf("unexpected notices: %v", notices)
			}
		})
	}
}

func TestGitAllowedSubcommands(t *testing.T) {
	t.Parallel()
	tool := NewGitTool()

	// Allowed subcommands should not error on parse (they may fail if not in a git repo,
	// but the subcommand itself should be accepted).
	for subcmd := range gitAllowedSubcommands {
		params, _ := json.Marshal(map[string]string{"command": subcmd + " --help"})
		_, err := tool.Execute(context.Background(), params)
		// We only check that it didn't return a "not allowed" error.
		if err != nil && strings.Contains(err.Error(), "not allowed") {
			t.Errorf("subcommand %q should be allowed but got: %v", subcmd, err)
		}
	}
}

func TestGitBlockedSubcommands(t *testing.T) {
	t.Parallel()
	tool := NewGitTool()

	blocked := []string{"push", "pull", "commit", "checkout", "reset", "rebase", "merge", "clean", "rm", "mv", "init", "clone", "fetch", "stash"}
	for _, subcmd := range blocked {
		params, _ := json.Marshal(map[string]string{"command": subcmd})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Errorf("subcommand %q should be blocked", subcmd)
			continue
		}
		if !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("subcommand %q error = %q, want 'not allowed'", subcmd, err.Error())
		}
	}
}

func TestGitEmptyCommand(t *testing.T) {
	t.Parallel()
	tool := NewGitTool()
	params, _ := json.Marshal(map[string]string{"command": ""})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for empty command")
	}
}

func TestGitLogInRepo(t *testing.T) {
	// This test runs in the foci repo itself, so git log should work.
	t.Parallel()
	tool := NewGitTool()
	params, _ := json.Marshal(map[string]string{"command": "log --oneline -3"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Text == "" {
		t.Error("expected non-empty git log output")
	}
}

func TestSplitShellArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  []string
	}{
		{`-name "*.go"`, []string{"-name", "*.go"}},
		{`-type f`, []string{"-type", "f"}},
		{`-name '*.txt' -type f`, []string{"-name", "*.txt", "-type", "f"}},
		{`-maxdepth 1`, []string{"-maxdepth", "1"}},
		{``, nil},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := splitShellArgs(tt.input)
			if err != nil {
				t.Fatalf("splitShellArgs(%q): %v", tt.input, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i, w := range tt.want {
				if got[i] != w {
					t.Errorf("arg[%d] = %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

func TestTailValidate(t *testing.T) {
	// Verify that -f, --follow, and -F are rejected by tailValidate.
	t.Parallel()
	for _, flag := range []string{"-f", "--follow", "-F"} {
		err := tailValidate([]string{flag})
		if err == nil {
			t.Errorf("tailValidate(%q) should have returned an error", flag)
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("tailValidate(%q) error = %q, want 'blocked'", flag, err.Error())
		}
	}
	// Valid flags should pass.
	if err := tailValidate([]string{"-n", "20"}); err != nil {
		t.Errorf("tailValidate(-n 20) should pass: %v", err)
	}
}

func TestNewPathToolBasic(t *testing.T) {
	// Test newPathTool with the 'file' command on a known file.
	t.Parallel()
	binPath, err := exec.LookPath("file")
	if err != nil {
		t.Skip("file binary not in PATH")
	}

	tool := newPathTool("file", binPath, "Identify file type.", true, nil)
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("hello world"), 0644)

	params, _ := json.Marshal(map[string]string{"path": f})
	result, execErr := tool.Execute(context.Background(), params)
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if !strings.Contains(result.Text, "text") {
		t.Errorf("expected 'text' in file output, got %q", result.Text)
	}
}

func TestNewPathToolRequiredPath(t *testing.T) {
	// Test that pathRequired=true rejects empty path.
	t.Parallel()
	tool := newPathTool("test", "/bin/echo", "Test.", true, nil)
	params, _ := json.Marshal(map[string]string{})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for missing required path")
	}
}

func TestNewPathToolOptionalPath(t *testing.T) {
	// Test that pathRequired=false defaults to '.'.
	t.Parallel()
	tool := newPathTool("test", "/bin/echo", "Test.", false, nil)
	params, _ := json.Marshal(map[string]string{})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// echo prints its args — should contain '.'
	if !strings.Contains(result.Text, ".") {
		t.Errorf("expected '.' in output, got %q", result.Text)
	}
}

func TestNewPathToolWithValidation(t *testing.T) {
	// Test that a validator rejecting flags causes an error.
	t.Parallel()
	binPath, err := exec.LookPath("tail")
	if err != nil {
		t.Skip("tail binary not in PATH")
	}

	tool := newPathTool("tail", binPath, "Tail.", true, tailValidate)
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("line1\nline2\nline3\n"), 0644)

	// Blocked flag
	params, _ := json.Marshal(map[string]string{"path": f, "params": "-f"})
	_, execErr := tool.Execute(context.Background(), params)
	if execErr == nil {
		t.Error("expected error for -f flag on tail")
	}

	// Valid usage
	params, _ = json.Marshal(map[string]string{"path": f, "params": "-n 2"})
	result, execErr := tool.Execute(context.Background(), params)
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if !strings.Contains(result.Text, "line") {
		t.Errorf("expected line output, got %q", result.Text)
	}
}

func TestNewFilterToolJQ(t *testing.T) {
	// Test newFilterTool with jq on a JSON file.
	t.Parallel()
	binPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq binary not in PATH")
	}

	tool := newFilterTool("jq", binPath, "Query JSON.", "filter")
	dir := t.TempDir()
	f := filepath.Join(dir, "test.json")
	os.WriteFile(f, []byte(`{"name": "alice", "age": 30}`), 0644)

	params, _ := json.Marshal(map[string]string{"filter": ".name", "path": f})
	result, execErr := tool.Execute(context.Background(), params)
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if !strings.Contains(result.Text, "alice") {
		t.Errorf("expected 'alice' in output, got %q", result.Text)
	}
}

func TestNewFilterToolMissing(t *testing.T) {
	// Test that missing filter/path params produce errors.
	t.Parallel()
	tool := newFilterTool("jq", "/usr/bin/jq", "Query JSON.", "filter")

	// Missing filter
	params, _ := json.Marshal(map[string]string{"path": "/tmp/test.json"})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for missing filter")
	}

	// Missing path
	params, _ = json.Marshal(map[string]string{"filter": ".name"})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for missing path")
	}
}

func TestNewSubcmdToolAllowed(t *testing.T) {
	// Test newSubcmdTool allows permitted subcommands.
	t.Parallel()
	allowed := map[string]bool{"status": true, "version": true}
	tool := newSubcmdTool("test-cmd", "/bin/echo", "Test tool.", allowed)

	params, _ := json.Marshal(map[string]string{"command": "status --all"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "status") {
		t.Errorf("expected 'status' in output, got %q", result.Text)
	}
}

func TestNewSubcmdToolBlocked(t *testing.T) {
	// Test newSubcmdTool blocks non-permitted subcommands.
	t.Parallel()
	allowed := map[string]bool{"status": true}
	tool := newSubcmdTool("test-cmd", "/bin/echo", "Test tool.", allowed)

	params, _ := json.Marshal(map[string]string{"command": "delete --all"})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for blocked subcommand")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %q, want 'not allowed'", err.Error())
	}
}

func TestCheckSQLiteDotCommand(t *testing.T) {
	// Verify dangerous dot-commands are blocked.
	t.Parallel()
	blocked := []string{
		".shell ls", ".system rm -rf /", ".import data.csv table",
		".open other.db", ".output out.txt", ".log debug.txt", ".once output.txt",
	}
	for _, cmd := range blocked {
		err := checkSQLiteDotCommand(cmd)
		if err == nil {
			t.Errorf("checkSQLiteDotCommand(%q) should have returned an error", cmd)
		}
	}
	// Safe dot-commands and regular queries should pass.
	safe := []string{".tables", ".schema", ".headers on", "SELECT 1"}
	for _, cmd := range safe {
		err := checkSQLiteDotCommand(cmd)
		if err != nil {
			t.Errorf("checkSQLiteDotCommand(%q) should pass: %v", cmd, err)
		}
	}
}

func TestCheckSQLiteDDLDML(t *testing.T) {
	// Verify DDL/DML statements are blocked.
	t.Parallel()
	blocked := []string{
		"CREATE TABLE t(id int)",
		"DROP TABLE t",
		"INSERT INTO t VALUES(1)",
		"UPDATE t SET id=2",
		"DELETE FROM t",
		"ALTER TABLE t ADD COLUMN name TEXT",
		"ATTACH DATABASE 'other.db' AS other",
	}
	for _, q := range blocked {
		err := checkSQLiteDDLDML(q)
		if err == nil {
			t.Errorf("checkSQLiteDDLDML(%q) should have returned an error", q)
		}
	}
	// Read-only statements should pass.
	safe := []string{
		"SELECT * FROM t",
		"EXPLAIN QUERY PLAN SELECT 1",
		"PRAGMA table_info(t)",
	}
	for _, q := range safe {
		err := checkSQLiteDDLDML(q)
		if err != nil {
			t.Errorf("checkSQLiteDDLDML(%q) should pass: %v", q, err)
		}
	}
}

func TestCheckSQLiteDDLDMLMultiStatement(t *testing.T) {
	// Verify DDL/DML blocked even in multi-statement queries.
	t.Parallel()
	err := checkSQLiteDDLDML("SELECT 1; DROP TABLE t")
	if err == nil {
		t.Error("multi-statement with DROP should be blocked")
	}
}

func TestSQLiteReadOnly(t *testing.T) {
	// Test SQLite tool with a read-only query against a temp database.
	t.Parallel()
	binPath, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 binary not in PATH")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	// Create the database with sqlite3.
	cmd := exec.CommandContext(context.Background(), binPath, dbPath,
		"CREATE TABLE t(id INTEGER, name TEXT); INSERT INTO t VALUES(1, 'alice'); INSERT INTO t VALUES(2, 'bob');")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create test db: %s: %v", out, err)
	}

	tool := NewSQLiteTool(binPath)
	params, _ := json.Marshal(map[string]string{
		"database": dbPath,
		"query":    "SELECT name FROM t WHERE id = 1",
	})
	result, execErr := tool.Execute(context.Background(), params)
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if !strings.Contains(result.Text, "alice") {
		t.Errorf("expected 'alice' in output, got %q", result.Text)
	}
}

func TestSQLiteBlocksDotCommands(t *testing.T) {
	// Test that the SQLite tool rejects dangerous dot-commands at the tool level.
	t.Parallel()
	tool := NewSQLiteTool("/usr/bin/sqlite3")
	params, _ := json.Marshal(map[string]string{
		"database": "/tmp/test.db",
		"query":    ".shell ls",
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for .shell dot-command")
	}
}

func TestSQLiteBlocksDDL(t *testing.T) {
	// Test that the SQLite tool rejects DDL at the tool level.
	t.Parallel()
	tool := NewSQLiteTool("/usr/bin/sqlite3")
	params, _ := json.Marshal(map[string]string{
		"database": "/tmp/test.db",
		"query":    "DROP TABLE users",
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for DROP statement")
	}
}

func TestIDTool(t *testing.T) {
	// Test id tool returns user identity info.
	t.Parallel()
	binPath, err := exec.LookPath("id")
	if err != nil {
		t.Skip("id binary not in PATH")
	}

	tool := NewIDTool(binPath)
	result, execErr := tool.Execute(context.Background(), nil)
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if !strings.Contains(result.Text, "uid=") {
		t.Errorf("expected 'uid=' in output, got %q", result.Text)
	}
}

func TestCrontabTool(t *testing.T) {
	// Test crontab tool runs without crashing (may have no crontab).
	t.Parallel()
	binPath, err := exec.LookPath("crontab")
	if err != nil {
		t.Skip("crontab binary not in PATH")
	}

	tool := NewCrontabTool(binPath)
	result, execErr := tool.Execute(context.Background(), nil)
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	// crontab -l may return "no crontab for user" — that's fine, it ran.
	if result.Text == "" {
		t.Error("expected non-empty output from crontab -l")
	}
}
