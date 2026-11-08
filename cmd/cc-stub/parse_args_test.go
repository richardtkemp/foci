package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseArgs_UnknownFlagIsAnError: an undeclared flag must fail, not warn.
// Go's flag package stops at the first flag it does not know, so a warning let
// the stub run on with every later flag lost — a44c628aa put
// --prompt-suggestions before --resume and --settings, and L2 reported session
// and nudge bugs (#2925).
func TestParseArgs_UnknownFlagIsAnError(t *testing.T) {
	cases := map[string][]string{
		"unknown flag before resume":    {"--print", "--brand-new-flag", "x", "--resume", "sess-1"},
		"unknown bool flag":             {"--brand-new-flag", "--resume", "sess-1"},
		"unknown flag with = value":     {"--brand-new-flag=x", "--resume", "sess-1"},
		"value given to a boolean flag": {"--verbose", "true", "--resume", "sess-1"},
		"stray positional":              {"--print", "hello", "--resume", "sess-1"},
		"auth with extra argument":      {"auth", "status", "--json"},
	}
	for name, argv := range cases {
		t.Run(name, func(t *testing.T) {
			a, err := parseArgs(argv)
			if err == nil {
				t.Fatalf("parseArgs(%q) = %+v, nil; want an error", argv, a)
			}
		})
	}
}

// TestParseArgs_FociArgv: every flag ccstream's Start can pass parses, and the
// values the stub acts on survive whatever comes before them.
func TestParseArgs_FociArgv(t *testing.T) {
	argv := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
		"--include-hook-events",
		"--verbose",
		"--disallowedTools", "Agent,Task",
		"--prompt-suggestions", "true",
		"--model", "claude-test",
		"--effort", "high",
		"--resume", "sess-1",
		"--restricted",
		"--tools", "Read,Grep",
		"--strict-mcp-config",
		"--permission-mode", "dontAsk",
		"--allowedTools", "Read,Grep",
		"--dangerously-skip-permissions",
		"--append-system-prompt", "extra",
		"--settings", `{"hooks":{}}`,
	}
	a, err := parseArgs(argv)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if a.resume != "sess-1" || a.model != "claude-test" || a.settings != `{"hooks":{}}` ||
		a.permissionMode != "dontAsk" || !a.skipPermissions || a.help || a.authStatus {
		t.Fatalf("parseArgs = %+v; want resume, model, settings, permission mode and skip kept", a)
	}
}

// TestParseArgs_AuthStatus: foci's readiness probe runs `claude auth status`.
func TestParseArgs_AuthStatus(t *testing.T) {
	a, err := parseArgs([]string{"auth", "status"})
	if err != nil || !a.authStatus {
		t.Fatalf("parseArgs(auth status) = %+v, %v; want authStatus", a, err)
	}
}

func TestParseArgs_ErrorNamesTheFlag(t *testing.T) {
	_, err := parseArgs([]string{"--brand-new-flag", "--resume", "x"})
	if err == nil || !strings.Contains(err.Error(), "brand-new-flag") {
		t.Fatalf("err = %v; want it to name brand-new-flag", err)
	}
}

// TestMain lets a test re-exec this binary as the stub itself
// (CCSTUB_TEST_RUN_MAIN=1), so the wiring in main is tested, not just
// parseArgs.
func TestMain(m *testing.M) {
	if os.Getenv("CCSTUB_TEST_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runStub(t *testing.T, recorder string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "CCSTUB_TEST_RUN_MAIN=1", "CCSTUB_RECORDER="+recorder)
	cmd.Stdin = strings.NewReader("")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run stub: %v", err)
	}
	return out.String(), errb.String(), code
}

// TestMain_UnknownFlagExitsLoudly: the stub exits 1 at once, says why on
// stderr, and records an argv_error entry instead of an invocation.
func TestMain_UnknownFlagExitsLoudly(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec.jsonl")
	_, stderr, code := runStub(t, rec, "--print", "--brand-new-flag", "x", "--resume", "sess-1")
	if code != 1 {
		t.Fatalf("exit code = %d; want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "brand-new-flag") || !strings.Contains(stderr, argvErrorHint) {
		t.Errorf("stderr %q does not name the flag and the fix", stderr)
	}
	data, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("read recorder: %v", err)
	}
	if !strings.Contains(string(data), `"kind":"argv_error"`) || strings.Contains(string(data), `"kind":"invocation"`) {
		t.Errorf("recorder = %s; want one argv_error and no invocation", data)
	}
}

// TestMain_AuthStatus: foci's readiness probe gets parseable logged-in JSON
// and no invocation is recorded for it.
func TestMain_AuthStatus(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec.jsonl")
	stdout, stderr, code := runStub(t, rec, "auth", "status")
	if code != 0 {
		t.Fatalf("exit code = %d; want 0 (stderr %q)", code, stderr)
	}
	if strings.TrimSpace(stdout) != authStatusJSON {
		t.Errorf("stdout = %q; want %q", stdout, authStatusJSON)
	}
	var st struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal([]byte(stdout), &st); err != nil || !st.LoggedIn {
		t.Fatalf("stdout %q: loggedIn=%v err=%v; want loggedIn true", stdout, st.LoggedIn, err)
	}
	if _, err := os.Stat(rec); err == nil {
		data, _ := os.ReadFile(rec)
		t.Errorf("auth status recorded %s; want nothing", data)
	}
}
