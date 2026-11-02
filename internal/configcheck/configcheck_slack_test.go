package configcheck

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckConfig_NoSlackDoesNotReadSecrets pins that foci check config on a
// config WITHOUT a slack platform never loads secrets.toml: a malformed
// secrets file sitting beside the config must not change the verdict.
// (The slack provider's credential check is the first thing that makes
// foci check config read secrets, and it only runs when slack is configured.)
func TestCheckConfig_NoSlackDoesNotReadSecrets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	if err := os.WriteFile(path, []byte(validConfigTOML), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.toml"), []byte("this is [ not toml"), 0644); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	if got := Run(path, io.Discard, io.Discard, "hint"); got != 0 {
		t.Errorf("Run(no slack, malformed secrets) = %d, want 0 (secrets must not be read)", got)
	}
}

// slackCheckConfig writes a config with a slack platform (default helen,
// channel C1 → helen) beside a secrets.toml with the given key/value pairs,
// and runs the config check. prepare (when set) runs inside the config dir
// before the check, so a test can replace secrets.toml with e.g. a symlink.
func slackCheckConfig(t *testing.T, mutate func(body *string), secretsBody string) (int, string) {
	return slackCheckConfigIn(t, mutate, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "secrets.toml"), []byte(secretsBody), 0600); err != nil {
			t.Fatal(err)
		}
	})
}

func slackCheckConfigIn(t *testing.T, mutate func(body *string), prepare func(dir string)) (int, string) {
	t.Helper()
	dir := t.TempDir()
	body := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "helen"

[[platforms]]
id = "slack"
[platforms.slack]
default_agent = "helen"
[[platforms.slack.channels]]
id = "C1"
agent = "helen"
`
	if mutate != nil {
		mutate(&body)
	}
	path := filepath.Join(dir, "foci.toml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	prepare(dir)
	var stderr strings.Builder
	code := runConfigCheckWith(path, io.Discard, &stderr, prodCheckDeps)
	return code, stderr.String()
}

// TestCheckConfig_SlackHealthyPasses proves a working slack config (mapping
// + both tokens in secrets.toml) exits 0.
func TestCheckConfig_SlackHealthyPasses(t *testing.T) {
	t.Parallel()
	code, stderr := slackCheckConfig(t, nil, strings.Join([]string{
		`slack.bot_token = "xoxb-t"`,
		`slack.app_token = "xapp-t"`,
	}, "\n"))
	if code != 0 {
		t.Fatalf("check = %d, want 0; stderr:\n%s", code, stderr)
	}
}

// TestCheckConfig_SlackMissingCredential proves a missing credential fails
// the check, naming the secret KEY (never a value) — the same oracle startup
// logs, so both agree.
func TestCheckConfig_SlackMissingCredential(t *testing.T) {
	t.Parallel()
	code, stderr := slackCheckConfig(t, nil, `slack.bot_token = "xoxb-t"`) // app token missing
	if code != 1 {
		t.Fatalf("check = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "slack.app_token") {
		t.Errorf("stderr misses the app token key:\n%s", stderr)
	}
	if strings.Contains(stderr, "xoxb-t") {
		t.Errorf("stderr leaks a token value:\n%s", stderr)
	}
}

// TestCheckConfig_SlackUnknownAgent proves a mapping that names an unknown
// agent fails the check (mapping errors are load errors).
func TestCheckConfig_SlackUnknownAgent(t *testing.T) {
	t.Parallel()
	code, stderr := slackCheckConfig(t, func(body *string) {
		*body = strings.Replace(*body, `agent = "helen"`, `agent = "ghost"`, 1)
	}, "")
	if code != 1 {
		t.Fatalf("check = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `"ghost"`) {
		t.Errorf("stderr misses the unknown agent:\n%s", stderr)
	}
}

// TestCheckConfig_SlackPerAgentBlockRejected proves a [platforms.slack] block
// on a per-agent [[agents.platforms]] entry fails the check instead of
// loading cleanly and being silently ignored.
func TestCheckConfig_SlackPerAgentBlockRejected(t *testing.T) {
	t.Parallel()
	code, stderr := slackCheckConfig(t, func(body *string) {
		*body = strings.Replace(*body, `id = "helen"`, `id = "helen"`+"\n"+`
[[agents.platforms]]
id = "slack"
[agents.platforms.slack]
default_agent = "nope"
`, 1)
	}, "")
	if code != 1 {
		t.Fatalf("check = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `agents[helen].platforms (slack)`) {
		t.Errorf("stderr misses the per-agent block:\n%s", stderr)
	}
}

// TestCheckConfig_SlackBlockUnderForeignEntryFails proves a
// [platforms.slack] block on a [[platforms]] entry whose id is not "slack"
// fails the check instead of passing and silently disabling Slack (the
// transport reads only the id="slack" entry).
func TestCheckConfig_SlackBlockUnderForeignEntryFails(t *testing.T) {
	t.Parallel()
	code, stderr := slackCheckConfig(t, func(body *string) {
		*body += "\n[[platforms]]\nid = \"telegram\"\n[platforms.telegram]\n[platforms.slack]\ndefault_agent = \"helen\"\n"
	}, "")
	if code != 1 {
		t.Fatalf("check = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `entry with id "telegram"`) {
		t.Errorf("stderr misses the foreign-entry refusal:\n%s", stderr)
	}
}

// TestCheckConfig_SlackBlocklessEntryFails proves a [[platforms]] id="slack"
// entry WITHOUT a [platforms.slack] block does not sail through the check:
// the entry declares the platform, so its default credential keys must be
// present — the exact shape an operator deploys with just the entry and no
// tokens must abort the deploy, not pass and fail at runtime.
func TestCheckConfig_SlackBlocklessEntryFails(t *testing.T) {
	t.Parallel()
	code, stderr := slackCheckConfigIn(t, func(body *string) {
		*body = strings.Replace(*body, "[platforms.slack]\ndefault_agent = \"helen\"\n[[platforms.slack.channels]]\nid = \"C1\"\nagent = \"helen\"\n", "", 1)
	}, func(dir string) {})
	if code != 1 {
		t.Fatalf("check = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "slack.bot_token") || !strings.Contains(stderr, "slack.app_token") {
		t.Errorf("stderr misses the default token keys:\n%s", stderr)
	}
	if !strings.Contains(stderr, "no default_agent and no channels mapped") {
		t.Errorf("stderr misses the nothing-mapped problem:\n%s", stderr)
	}
}

// TestCheckConfig_SlackNonRegularSecretsRefused proves foci check config refuses
// a secrets.toml that is not a regular file (here a directory; the open
// refuses the fifo shape the same way) — the check runs as root, and its
// read goes through one O_NOFOLLOW-opened, regular-file-verified descriptor.
func TestCheckConfig_SlackNonRegularSecretsRefused(t *testing.T) {
	t.Parallel()
	code, stderr := slackCheckConfigIn(t, nil, func(dir string) {
		if err := os.Mkdir(filepath.Join(dir, "secrets.toml"), 0700); err != nil {
			t.Fatal(err)
		}
	})
	if code != 1 {
		t.Fatalf("check = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "not a regular file") {
		t.Errorf("stderr misses the non-regular refusal:\n%s", stderr)
	}
}

// TestCheckConfig_SlackSymlinkedSecretsRefused proves foci check config never
// reads a secrets.toml that is a symlink (the check runs as root during
// deploys; secrets.Load would follow the link), and that the refusal names
// the file without echoing any of the target's content.
func TestCheckConfig_SlackSymlinkedSecretsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("root:x:0:0:leaked parse fragment\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, stderr := slackCheckConfigIn(t, nil, func(dir string) {
		if err := os.Symlink(outside, filepath.Join(dir, "secrets.toml")); err != nil {
			t.Fatal(err)
		}
	})
	if code != 1 {
		t.Fatalf("check = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "symlink") {
		t.Errorf("stderr misses the symlink refusal:\n%s", stderr)
	}
	if strings.Contains(stderr, "leaked parse fragment") {
		t.Errorf("stderr echoed the symlink target's content:\n%s", stderr)
	}
}

// TestCheckConfig_SlackSymlinkedSecretsDirRefused proves the refusal covers
// a symlinked DIRECTORY component of the secrets path: O_NOFOLLOW on the
// final component alone would let a link planted one level up redirect the
// root reader to any secrets.toml it can open (the config directory is
// foci-writable), so the whole path is walked with openat + O_NOFOLLOW and
// a symlinked component fails the check — exit 1, nothing read.
func TestCheckConfig_SlackSymlinkedSecretsDirRefused(t *testing.T) {
	t.Parallel()
	// A real config dir with working config and secrets, reached through a
	// symlink alias: the config itself may load through the alias, but the
	// secrets read must refuse the alias as a path component.
	realDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(realDir, "secrets.toml"), []byte("slack.bot_token = \"xoxb-t\"\nslack.app_token = \"xapp-t\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	alias := filepath.Join(root, "config-alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	aliasConfig := filepath.Join(alias, "foci.toml")
	if err := os.WriteFile(aliasConfig, []byte(validConfigTOML+"\n[[platforms]]\nid = \"slack\"\n[platforms.slack]\ndefault_agent = \"main\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if got := runConfigCheckWith(aliasConfig, io.Discard, &stderr, prodCheckDeps); got != 1 {
		t.Fatalf("check = %d, want 1 (a symlinked directory component must be refused); stderr:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Errorf("stderr misses the symlink refusal:\n%s", stderr.String())
	}
}

// TestCheckConfig_SlackMalformedSecretsNoValueEcho proves a malformed
// secrets.toml fails the check without echoing any of its bytes: the
// parser's fragment would quote (a prefix of) a secret value, so only the
// line number reaches stderr.
func TestCheckConfig_SlackMalformedSecretsNoValueEcho(t *testing.T) {
	t.Parallel()
	code, stderr := slackCheckConfig(t, nil, "[slack]\nbot_token = xoxb-unquoted-secret\n")
	if code != 1 {
		t.Fatalf("check = %d, want 1; stderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "xoxb-unquoted-secret") {
		t.Errorf("stderr echoes a fragment of the malformed value:\n%s", stderr)
	}
	if !strings.Contains(stderr, "line 2") {
		t.Errorf("stderr misses the line-number locator:\n%s", stderr)
	}
}
