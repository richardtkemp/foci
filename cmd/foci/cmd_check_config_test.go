package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validConfigTOML is a minimal config that loads and validates cleanly.
const validConfigTOML = `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "main"
`

// runConfigCheck runs `foci check config --config path` in-process. These
// tests moved here from cmd/foci-gw when foci-gw -check-config was dropped
// (#2788): foci check config is now the one entry point, the deploy gate
// included, so it carries the whole suite.
func runConfigCheck(path string, stdout, stderr io.Writer) int {
	return checkConfig([]string{"--config", path}, stdout, stderr)
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestRunConfigCheck_Clean(t *testing.T) {
	t.Parallel()
	// A config that loads and validates with no unknown keys exits 0.
	path := writeTempConfig(t, validConfigTOML)
	if got := runConfigCheck(path, io.Discard, io.Discard); got != 0 {
		t.Errorf("runConfigCheck(clean) = %d, want 0", got)
	}
}

func TestRunConfigCheck_UnknownKey(t *testing.T) {
	t.Parallel()
	// Strict policy: a config that loads fine but carries an unknown/deprecated
	// key (e.g. a renamed setting) must exit 1 — the old setting is silently
	// lost at startup, which is a real upgrade incompatibility. A renamed key
	// only becomes "unknown" relative to the binary that dropped it, so this
	// fixture uses a section that is unknown at any commit.
	path := writeTempConfig(t, validConfigTOML+"\n[bogus_section]\nfoo = \"bar\"\n")
	if got := runConfigCheck(path, io.Discard, io.Discard); got != 1 {
		t.Errorf("runConfigCheck(unknown key) = %d, want 1", got)
	}
}

func TestRunConfigCheck_ParseError(t *testing.T) {
	t.Parallel()
	// A malformed TOML file fails to load and exits 1.
	path := writeTempConfig(t, "[[agents]\nid = \"main\"\n")
	if got := runConfigCheck(path, io.Discard, io.Discard); got != 1 {
		t.Errorf("runConfigCheck(parse error) = %d, want 1", got)
	}
}

func TestRunConfigCheck_ValidateError(t *testing.T) {
	t.Parallel()
	// A syntactically valid TOML that fails Validate (missing required
	// [groups] powerful) exits 1.
	path := writeTempConfig(t, "[[agents]]\nid = \"main\"\n")
	if got := runConfigCheck(path, io.Discard, io.Discard); got != 1 {
		t.Errorf("runConfigCheck(validate error) = %d, want 1", got)
	}
}

func TestRunConfigCheck_MissingFile(t *testing.T) {
	t.Parallel()
	if got := runConfigCheck("/nonexistent/path/foci.toml", io.Discard, io.Discard); got != 1 {
		t.Errorf("runConfigCheck(missing) = %d, want 1", got)
	}
}

// A missing config must say how to point at one explicitly — a bare
// "no such file" read like the live config was broken (#2117).
func TestRunConfigCheck_MissingFileNamesConfigFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	missing := "/nonexistent/path/foci.toml"
	if got := runConfigCheck(missing, &stdout, &stderr); got != 1 {
		t.Fatalf("runConfigCheck(missing) = %d, want 1", got)
	}
	msg := stderr.String()
	for _, want := range []string{missing, "--config PATH", "foci check config --config "} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing-file message lacks %q:\n%s", want, msg)
		}
	}
}

func TestRunConfigCheck_IgnoredBackendConfigWarnsButPasses(t *testing.T) {
	t.Parallel()
	// A backend_config key the agent's backend does not read (#2178) is
	// listed as a warning, naming the agent, key and backend. It does not
	// fail the check: it was never honoured, so the upgrade loses nothing.
	path := writeTempConfig(t, validConfigTOML+`
[[agents]]
id = "cc"
backend = "claude-code"

[agents.backend_config]
model = "opus"
hostname = "0.0.0.0"
stop_rules = [{ name = "x", text = ["starting"], reason = "r" }]

[[agents]]
id = "oc"
backend = "opencode"

[agents.backend_config]
port = 4096
allowed_tools = ["Read"]
`)
	var stderr strings.Builder
	if got := runConfigCheck(path, io.Discard, &stderr); got != 0 {
		t.Fatalf("runConfigCheck(ignored keys) = %d, want 0; stderr:\n%s", got, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{
		"agents[cc].backend_config.hostname (backend claude-code does not read it)",
		"agents[oc].backend_config.allowed_tools (backend opencode does not read it)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}
	for _, honoured := range []string{".model", ".stop_rules", ".port"} {
		if strings.Contains(out, honoured+" (") {
			t.Errorf("stderr lists honoured key %s as ignored:\n%s", honoured, out)
		}
	}
}

// TestRunConfigCheck_ExampleFailureFails proves a failing example of a rule
// WITHOUT a when-script fails the check (exit 1), naming the agent, rule
// and example, and the rule that matched instead.
func TestRunConfigCheck_ExampleFailureFails(t *testing.T) {
	t.Parallel()
	path := writeTempConfig(t, validConfigTOML+`

[[agents]]
id = "cc"
backend = "claude-code"

[[agents.backend_config.pretool_rules]]
name = "no_sudo"
tool = "Bash"
command = 'sudo( |$)'
reason = "r"

[[agents.backend_config.pretool_rules]]
name = "no_rm"
tool = "Bash"
command = 'rm( |$)'
deny_examples = ["sudo rm /x", "ls"]
reason = "r"
`)
	var stderr strings.Builder
	if got := runConfigCheck(path, io.Discard, &stderr); got != 1 {
		t.Fatalf("runConfigCheck = %d, want 1; stderr:\n%s", got, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{
		`agents[cc].pretool_rules "no_rm": deny example sudo rm /x: expected no_rm, matched no_sudo`,
		`agents[cc].pretool_rules "no_rm": deny example ls: expected no_rm, matched nothing`,
		"config check FAILED: 2 pretool example failure(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}
}

// TestRunConfigCheck_WhenRuleExamplesWarnOnly proves the root-safety rule
// (#2039): a rule WITH a when-script never has it executed by check-config
// (the script's marker must stay absent), gets exactly one WARN line, and
// never changes the exit status — even when a deny example does not match
// the rule's patterns.
func TestRunConfigCheck_WhenRuleExamplesWarnOnly(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "marker")
	path := writeTempConfig(t, validConfigTOML+`

[[agents]]
id = "cc"
backend = "claude-code"

[[agents.backend_config.pretool_rules]]
name = "gated_rm"
tool = "Bash"
command = 'rm( |$)'
when = 'touch `+marker+`; exit 0'
deny_examples = ["rm x", "ls"]
allow_examples = ["trash x"]
reason = "r"
`)
	var stderr strings.Builder
	if got := runConfigCheck(path, io.Discard, &stderr); got != 0 {
		t.Fatalf("runConfigCheck = %d, want 0; stderr:\n%s", got, stderr.String())
	}
	out := stderr.String()
	if want := "when not evaluated by check-config"; strings.Count(out, want) != 1 {
		t.Errorf("%d %q WARN lines, want 1:\n%s", strings.Count(out, want), want, out)
	}
	for _, want := range []string{
		`config check warning: agents[cc].pretool_rules "gated_rm"`,
		"3 example(s) checked against patterns only",
		"1 deny example(s) do not match the rule's patterns",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the when-script ran (marker exists): check-config must never execute one")
	}
}

// TestRunConfigCheck_ExamplesPass proves a config whose examples all hold
// exits 0 with the OK line and no example output.
func TestRunConfigCheck_ExamplesPass(t *testing.T) {
	t.Parallel()
	path := writeTempConfig(t, validConfigTOML+`

[[agents]]
id = "cc"
backend = "claude-code"

[[agents.backend_config.pretool_rules]]
name = "no_rm"
tool = "Bash"
command = 'rm( |$)'
deny_examples  = ["rm -rf /tmp/x"]
allow_examples = ["trash /tmp/x"]
reason = "r"
`)
	var stdout, stderr strings.Builder
	if got := runConfigCheck(path, &stdout, &stderr); got != 0 {
		t.Fatalf("runConfigCheck = %d, want 0; stderr:\n%s", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config check OK") {
		t.Errorf("stdout missing OK:\n%s", stdout.String())
	}
	if out := stderr.String(); strings.Contains(out, "pretool") {
		t.Errorf("stderr mentions pretool:\n%s", out)
	}
}

// TestRunConfigCheck_UnknownToolNameFails proves the config check exits 1 and
// names the bad entry, its scope and the valid names, for a misspelt global
// deny entry and for a foci_-prefixed agent allow entry; and exits 0 when
// every name is valid.
func TestRunConfigCheck_UnknownToolNameFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		wantCode   int
		wantMsg    []string
	}{
		{"global deny typo", "[tools]\ndeny_tools = [\"browsr\"]\n" + validConfigTOML, 1,
			[]string{"tools.deny_tools", `"browsr"`, "valid tool names:", "browser", "send_to_session"}},
		{"agent allow prefixed", validConfigTOML + "[agents.tools]\nallow_tools = [\"foci_todo\"]\n", 1,
			[]string{"agents[main].tools.allow_tools", `"foci_todo"`, "valid tool names:"}},
		{"valid names", "[tools]\ndeny_tools = [\"browser\"]\n" + validConfigTOML + "[agents.tools]\nallow_tools = [\"todo\", \"shell\"]\ndeny_tools = [\"shell\"]\n", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTempConfig(t, tc.body)
			var stderr strings.Builder
			if got := runConfigCheck(path, &strings.Builder{}, &stderr); got != tc.wantCode {
				t.Fatalf("exit %d, want %d; stderr:\n%s", got, tc.wantCode, stderr.String())
			}
			for _, m := range tc.wantMsg {
				if !strings.Contains(stderr.String(), m) {
					t.Errorf("stderr lacks %q:\n%s", m, stderr.String())
				}
			}
		})
	}
}
