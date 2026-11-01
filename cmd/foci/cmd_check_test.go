package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/configcheck"
	"foci/internal/nudge"
)

// checkFixture is a config with one API agent whose workspace has a
// character file, plus that agent's rules file path.
type checkFixture struct {
	configPath, workspace, rulesPath string
}

func newCheckFixture(t *testing.T) checkFixture {
	t.Helper()
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "character"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "foci.toml")
	cfg := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "main"
workspace = "` + ws + `"
system.system_files = ["character/SOUL.md"]
`
	writeCheckFile(t, cfgPath, cfg)
	writeCheckFile(t, filepath.Join(ws, "character", "SOUL.md"), "be careful")
	return checkFixture{configPath: cfgPath, workspace: ws, rulesPath: nudge.RulesPath(ws)}
}

func writeCheckFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// currentHash is the content_hash the fixture's character files give.
func (f checkFixture) currentHash(t *testing.T) string {
	t.Helper()
	h, ok := nudge.CharacterHash(f.workspace, []string{"character/SOUL.md"})
	if !ok {
		t.Fatal("fixture has no character files")
	}
	return h
}

// runCheck runs cmdCheck in-process and returns exit code, stdout, stderr.
func runCheck(args ...string) (int, string, string) {
	var stdout, stderr strings.Builder
	code := cmdCheck(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// snapshot records each file's bytes and mtime so a test can show a command
// changed neither.
type snapshot map[string]struct {
	data  string
	mtime time.Time
}

func takeSnapshot(t *testing.T, paths ...string) snapshot {
	t.Helper()
	s := snapshot{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		s[p] = struct {
			data  string
			mtime time.Time
		}{string(data), st.ModTime()}
	}
	return s
}

func (s snapshot) assertUnchanged(t *testing.T) {
	t.Helper()
	for p, want := range s {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want.data || !st.ModTime().Equal(want.mtime) {
			t.Errorf("%s changed (bytes or mtime): the check must write nothing", p)
		}
	}
}

// TestCheckConfig_SameLinesAndCodesAsCheckConfig proves `foci check config`
// runs the shared check: for a clean config and a config with an unknown
// key it prints exactly the lines, and returns exactly the code, that
// configcheck.Run (the code behind foci-gw -check-config) gives, and it
// changes neither file.
func TestCheckConfig_SameLinesAndCodesAsCheckConfig(t *testing.T) {
	f := newCheckFixture(t)
	bad := filepath.Join(t.TempDir(), "bad.toml")
	writeCheckFile(t, bad, "[bogus_section]\nfoo = 1\n")
	snap := takeSnapshot(t, f.configPath, bad)

	for _, tc := range []struct {
		path     string
		wantCode int
	}{{f.configPath, 0}, {bad, 1}} {
		code, out, errOut := runCheck("config", "--config", tc.path)
		var wantOut, wantErr strings.Builder
		wantCode := configcheck.Run(tc.path, &wantOut, &wantErr, "")
		if code != tc.wantCode || code != wantCode {
			t.Errorf("%s: exit %d, want %d (shared check gives %d); stderr:\n%s", tc.path, code, tc.wantCode, wantCode, errOut)
		}
		if out != wantOut.String() || errOut != wantErr.String() {
			t.Errorf("%s: output differs from the shared check:\n got stdout %q stderr %q\nwant stdout %q stderr %q", tc.path, out, errOut, wantOut.String(), wantErr.String())
		}
	}
	snap.assertUnchanged(t)
}

// TestCheckConfig_MissingFileNamesThisFlag proves a missing config fails
// with the shared verdict and a hint naming this command's --config flag.
func TestCheckConfig_MissingFileNamesThisFlag(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.toml")
	code, _, errOut := runCheck("config", "--config", missing)
	if code != 1 || !strings.Contains(errOut, "no config file at "+missing) || !strings.Contains(errOut, "foci check config --config") {
		t.Errorf("exit %d stderr %q, want 1 with the no-file verdict and a --config hint", code, errOut)
	}
}

// TestCheckNudge_ValidFile proves a valid, current rules file passes with
// no finding and a verdict naming the file and the counts, and is not
// changed.
func TestCheckNudge_ValidFile(t *testing.T) {
	f := newCheckFixture(t)
	writeCheckFile(t, f.rulesPath, `{"content_hash": "`+f.currentHash(t)+`", "rules": [
		{"text": "verify", "priority": "high", "trigger": {"type": "every_n_turns", "n": 3}}]}`)
	snap := takeSnapshot(t, f.rulesPath, f.configPath)

	code, out, errOut := runCheck("nudge", "--config", f.configPath, "--agent", "main")
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout %q stderr %q", code, out, errOut)
	}
	if out != "nudge check OK: "+f.rulesPath+": 0 error(s), 0 warning(s)\n" {
		t.Errorf("stdout = %q", out)
	}
	snap.assertUnchanged(t)
}

// TestCheckNudge_FileWithErrors proves each error is printed on its own line
// and the exit is non-zero, with a FAILED verdict carrying the counts.
func TestCheckNudge_FileWithErrors(t *testing.T) {
	f := newCheckFixture(t)
	writeCheckFile(t, f.rulesPath, `{"content_hash": "`+f.currentHash(t)+`", "rules": [
		{"text": "a", "priority": "high", "trigger": {"type": "regex", "pattern": "(x"}},
		{"text": "b", "priority": "urgent", "trigger": {"type": "every_n_turns", "n": 3}}]}`)
	snap := takeSnapshot(t, f.rulesPath)

	code, out, _ := runCheck("nudge", "--config", f.configPath, "--agent", "main")
	if code == 0 {
		t.Fatalf("exit 0, want non-zero; stdout %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], `error: rule 0 ("a")`) || !strings.HasPrefix(lines[1], `error: rule 1 ("b")`) {
		t.Errorf("want two error lines then a verdict, got:\n%s", out)
	}
	if lines[len(lines)-1] != "nudge check FAILED: "+f.rulesPath+": 2 error(s), 0 warning(s)" {
		t.Errorf("verdict = %q", lines[len(lines)-1])
	}
	snap.assertUnchanged(t)
}

// TestCheckNudge_OnlyWarnings proves warnings alone do not fail the check: a
// stale content_hash prints its warning and exits 0.
func TestCheckNudge_OnlyWarnings(t *testing.T) {
	f := newCheckFixture(t)
	writeCheckFile(t, f.rulesPath, `{"content_hash": "stale", "rules": [
		{"text": "a", "priority": "high", "trigger": {"type": "after_error"}}]}`)
	snap := takeSnapshot(t, f.rulesPath, filepath.Join(f.workspace, "character", "SOUL.md"))

	code, out, _ := runCheck("nudge", "--config", f.configPath, "--agent", "main")
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout %q", code, out)
	}
	if !strings.Contains(out, "warning: content_hash does not match") || !strings.Contains(out, "nudge check OK: "+f.rulesPath+": 0 error(s), 1 warning(s)") {
		t.Errorf("stdout = %q", out)
	}
	snap.assertUnchanged(t)
}

// TestCheckNudge_MissingFile proves an agent with no rules file is not an
// error: the command says so, names the path, exits 0 and creates nothing.
func TestCheckNudge_MissingFile(t *testing.T) {
	f := newCheckFixture(t)
	code, out, _ := runCheck("nudge", "--config", f.configPath, "--agent", "main")
	if code != 0 || out != "agent main has no nudge-rules.json at "+f.rulesPath+": nothing to check\n" {
		t.Errorf("exit %d stdout %q", code, out)
	}
	if _, err := os.Stat(f.rulesPath); !os.IsNotExist(err) {
		t.Errorf("the check created %s", f.rulesPath)
	}
}

// TestCheckNudge_OtherFile proves --file checks another file instead of the
// agent's own, and the verdict names that file.
func TestCheckNudge_OtherFile(t *testing.T) {
	f := newCheckFixture(t)
	other := filepath.Join(t.TempDir(), "draft.json")
	writeCheckFile(t, other, `{"rules": [{"text": "a", "priority": "high", "trigger": {"type": "nope"}}]}`)
	snap := takeSnapshot(t, other)

	code, out, _ := runCheck("nudge", "--config", f.configPath, "--agent", "main", "--file", other)
	if code == 0 || !strings.Contains(out, `trigger type "nope"`) || !strings.Contains(out, "nudge check FAILED: "+other+":") {
		t.Errorf("exit %d stdout %q", code, out)
	}
	snap.assertUnchanged(t)
}

// TestCheckNudge_UnknownAgent proves an agent not in the config is an error
// that names the agent.
func TestCheckNudge_UnknownAgent(t *testing.T) {
	f := newCheckFixture(t)
	code, _, errOut := runCheck("nudge", "--config", f.configPath, "--agent", "ghost")
	if code == 0 || !strings.Contains(errOut, `no agent "ghost"`) {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

// TestCheckNudge_AgentFromSessionKey proves the agent defaults from
// FOCI_SESSION_KEY's first segment, and that with neither that nor --agent
// the command says how to name one.
func TestCheckNudge_AgentFromSessionKey(t *testing.T) {
	f := newCheckFixture(t)
	t.Setenv("FOCI_SESSION_KEY", "main/app/1")
	code, out, errOut := runCheck("nudge", "--config", f.configPath)
	if code != 0 || !strings.Contains(out, "agent main has no nudge-rules.json") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}

	t.Setenv("FOCI_SESSION_KEY", "")
	code, _, errOut = runCheck("nudge", "--config", f.configPath)
	if code == 0 || !strings.Contains(errOut, "--agent <id>") {
		t.Errorf("no agent: exit %d stderr %q", code, errOut)
	}
}

// TestCheckHelp proves `foci -h` lists the check command and `foci check -h`
// prints the usage of both subcommands, through the built binary.
func TestCheckHelp(t *testing.T) {
	out, _ := exec.Command(testBinary, "-h").CombinedOutput()
	if !strings.Contains(string(out), "check config|nudge") {
		t.Errorf("foci -h does not list check config|nudge:\n%s", out)
	}
	out, err := exec.Command(testBinary, "check", "-h").CombinedOutput()
	if err != nil {
		t.Errorf("foci check -h: %v", err)
	}
	for _, want := range []string{"Usage: foci check", "  config ", "  nudge ", "--file <path>"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("foci check -h lacks %q:\n%s", want, out)
		}
	}
	code, _, _ := runCheck("bogus")
	if code == 0 {
		t.Error("unknown subcommand: want non-zero exit")
	}
}
