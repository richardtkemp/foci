package testharness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// decodedTestConfig mirrors the subset of the generated foci.toml the
// config-writer tests assert on. Pointer fields distinguish "key absent"
// from zero values for the Omit* variants.
type decodedTestConfig struct {
	DataDir            string `toml:"data_dir"`
	SkipSecurityChecks bool   `toml:"skip_security_checks"`
	WelcomeFile        string `toml:"welcome_file"`
	ExtraSection       *struct {
		FooExtra int `toml:"foo_extra"`
	} `toml:"extra_section"`
	HTTP *struct {
		Port int `toml:"port"`
	} `toml:"http"`
	Logging *struct {
		EventFile   string `toml:"event_file"`
		APIFile     string `toml:"api_file"`
		PayloadFile string `toml:"payload_file"`
		ArchiveDir  string `toml:"archive_dir"`
	} `toml:"logging"`
	CCBackend struct {
		Binary string `toml:"binary"`
	} `toml:"cc_backend"`
	Platforms []struct {
		ID       string `toml:"id"`
		Telegram struct {
			APIBase         string `toml:"api_base"`
			LongPollTimeout string `toml:"long_poll_timeout"`
		} `toml:"telegram"`
	} `toml:"platforms"`
	Agents []struct {
		ID            string  `toml:"id"`
		Workspace     *string `toml:"workspace"`
		Backend       string  `toml:"backend"`
		BackendConfig struct {
			Model  string            `toml:"model"`
			Env    map[string]string `toml:"env"`
			Binary string            `toml:"binary"`
		} `toml:"backend_config"`
		Permissions *struct {
			AutoApprove    []string `toml:"auto_approve"`
			CommonReadonly *bool    `toml:"auto_approve_common_readonly"`
			CommonSafe     *bool    `toml:"auto_approve_common_safe_write"`
		} `toml:"permissions"`
		Platforms []struct {
			ID        string  `toml:"id"`
			Bot       *string `toml:"bot"`
			BotSecret string  `toml:"bot_secret"`
			Access    struct {
				AllowedUsers []string `toml:"allowed_users"`
			} `toml:"access"`
		} `toml:"platforms"`
	} `toml:"agents"`
}

// writeAndDecodeConfig runs writeTestConfig into a temp file and decodes
// the result, failing the test on invalid TOML. Returns the decoded
// config plus the raw file contents for line-level assertions.
func writeAndDecodeConfig(t *testing.T, o testConfigOpts) (decodedTestConfig, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	writeTestConfig(t, path, o)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	var cfg decodedTestConfig
	if _, err := toml.Decode(string(raw), &cfg); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\n--- config ---\n%s", err, raw)
	}
	return cfg, string(raw)
}

// baseConfigOpts returns a minimal single-agent option set the variant
// tests mutate.
func baseConfigOpts(agents ...AgentSpec) testConfigOpts {
	workspaces := map[string]string{}
	for _, a := range agents {
		workspaces[a.ID] = "/ws/" + a.ID
	}
	return testConfigOpts{
		DataDir:      "/tmp/test-data",
		LogsDir:      "/tmp/test-logs",
		ClaudeBinary: "/bin/cc-stub",
		TelegramBase: "http://127.0.0.1:1",
		Agents:       agents,
		Workspaces:   workspaces,
		HTTPPort:     12345,
	}
}

// TestWriteTestConfig_Basic proves the happy-path config is valid TOML
// wiring everything to test-scoped paths: data dir, http port, logging
// under LogsDir, cc_backend binary, telegram api_base, and a fully
// populated agent with workspace, bot, and allowed_users.
func TestWriteTestConfig_Basic(t *testing.T) {
	cfg, _ := writeAndDecodeConfig(t, baseConfigOpts(AgentSpec{ID: "alpha", UserID: 7}))

	if cfg.DataDir != "/tmp/test-data" || !cfg.SkipSecurityChecks {
		t.Errorf("data_dir=%q skip_security_checks=%v, want /tmp/test-data + true", cfg.DataDir, cfg.SkipSecurityChecks)
	}
	if cfg.WelcomeFile != "/tmp/test-data/WELCOME.md" {
		t.Errorf("welcome_file = %q, want scoped to DataDir", cfg.WelcomeFile)
	}
	if cfg.HTTP == nil || cfg.HTTP.Port != 12345 {
		t.Errorf("http section = %+v, want port 12345", cfg.HTTP)
	}
	if cfg.Logging == nil {
		t.Fatalf("missing [logging] section")
	}
	for name, got := range map[string]string{
		"event_file":   cfg.Logging.EventFile,
		"api_file":     cfg.Logging.APIFile,
		"payload_file": cfg.Logging.PayloadFile,
		"archive_dir":  cfg.Logging.ArchiveDir,
	} {
		if !strings.HasPrefix(got, "/tmp/test-logs/") {
			t.Errorf("logging.%s = %q, want under LogsDir", name, got)
		}
	}
	if cfg.CCBackend.Binary != "/bin/cc-stub" {
		t.Errorf("cc_backend.binary = %q, want /bin/cc-stub", cfg.CCBackend.Binary)
	}
	if len(cfg.Platforms) != 1 || cfg.Platforms[0].Telegram.APIBase != "http://127.0.0.1:1" {
		t.Errorf("platforms = %+v, want one telegram entry with stub api_base", cfg.Platforms)
	}

	if len(cfg.Agents) != 1 {
		t.Fatalf("agents = %d entries, want 1", len(cfg.Agents))
	}
	a := cfg.Agents[0]
	if a.ID != "alpha" || a.Backend != "claude-code" || a.BackendConfig.Model != "stub" {
		t.Errorf("agent = id=%q backend=%q model=%q, want alpha/claude-code/stub", a.ID, a.Backend, a.BackendConfig.Model)
	}
	if a.Workspace == nil || *a.Workspace != "/ws/alpha" {
		t.Errorf("workspace = %v, want /ws/alpha", a.Workspace)
	}
	if a.Permissions != nil {
		t.Errorf("permissions section emitted with no knobs set: %+v", a.Permissions)
	}
	if len(a.Platforms) != 1 {
		t.Fatalf("agent platforms = %d entries, want 1", len(a.Platforms))
	}
	p := a.Platforms[0]
	if p.Bot == nil || *p.Bot != "alpha" {
		t.Errorf("platform bot = %v, want alpha", p.Bot)
	}
	if len(p.Access.AllowedUsers) != 1 || p.Access.AllowedUsers[0] != "7" {
		t.Errorf("allowed_users = %v, want [\"7\"]", p.Access.AllowedUsers)
	}
}

// TestWriteTestConfig_HTTPPortZeroOmitsSection proves a zero HTTPPort
// skips the [http] override so foci's default applies.
func TestWriteTestConfig_HTTPPortZeroOmitsSection(t *testing.T) {
	o := baseConfigOpts(AgentSpec{ID: "alpha", UserID: 7})
	o.HTTPPort = 0
	cfg, _ := writeAndDecodeConfig(t, o)
	if cfg.HTTP != nil {
		t.Errorf("[http] section emitted for HTTPPort=0: %+v", cfg.HTTP)
	}
}

// TestWriteTestConfig_LoggingOverride proves a test-supplied [logging]
// section in ExtraConfigTOML suppresses the base emission (no duplicate
// table — the file stays valid TOML) and its values win.
func TestWriteTestConfig_LoggingOverride(t *testing.T) {
	o := baseConfigOpts(AgentSpec{ID: "alpha", UserID: 7})
	o.ExtraConfigTOML = "[logging]\nevent_file = \"/custom/foci.log\"\n"
	cfg, raw := writeAndDecodeConfig(t, o)

	if n := strings.Count(raw, "[logging]"); n != 1 {
		t.Errorf("found %d [logging] headers, want exactly 1 (base must be suppressed)", n)
	}
	if cfg.Logging == nil || cfg.Logging.EventFile != "/custom/foci.log" {
		t.Errorf("logging = %+v, want event_file=/custom/foci.log", cfg.Logging)
	}
}

// TestWriteTestConfig_AgentOptions proves the per-agent emission knobs:
// sorted inline env table, per-agent claude_binary override, the full
// [agents.permissions] block, and bot_secret on the platform entry.
func TestWriteTestConfig_AgentOptions(t *testing.T) {
	yes, no := true, false
	cfg, raw := writeAndDecodeConfig(t, baseConfigOpts(AgentSpec{
		ID:                         "alpha",
		UserID:                     7,
		ExtraEnv:                   map[string]string{"ZED": "26", "ALPHA": "1"},
		ClaudeBinary:               "/agent/own-cc",
		AutoApprove:                []string{"Bash(ls *)", "Read"},
		AutoApproveCommonReadonly:  &no,
		AutoApproveCommonSafeWrite: &yes,
		PlatformBotSecret:          "custom.weird_token",
	}))

	a := cfg.Agents[0]
	if a.BackendConfig.Env["ALPHA"] != "1" || a.BackendConfig.Env["ZED"] != "26" {
		t.Errorf("env = %v, want ALPHA=1 ZED=26", a.BackendConfig.Env)
	}
	// Keys must emit sorted for stable snapshots — assert on the raw line.
	if !strings.Contains(raw, `env = {ALPHA = "1", ZED = "26"}`) {
		t.Errorf("env not emitted as sorted inline table; config:\n%s", raw)
	}
	if a.BackendConfig.Binary != "/agent/own-cc" {
		t.Errorf("per-agent binary = %q, want /agent/own-cc", a.BackendConfig.Binary)
	}
	if a.Permissions == nil {
		t.Fatalf("missing [agents.permissions] block")
	}
	if got := a.Permissions.AutoApprove; len(got) != 2 || got[0] != "Bash(ls *)" || got[1] != "Read" {
		t.Errorf("auto_approve = %v, want [Bash(ls *) Read]", got)
	}
	if a.Permissions.CommonReadonly == nil || *a.Permissions.CommonReadonly {
		t.Errorf("auto_approve_common_readonly = %v, want false", a.Permissions.CommonReadonly)
	}
	if a.Permissions.CommonSafe == nil || !*a.Permissions.CommonSafe {
		t.Errorf("auto_approve_common_safe_write = %v, want true", a.Permissions.CommonSafe)
	}
	if a.Platforms[0].BotSecret != "custom.weird_token" {
		t.Errorf("bot_secret = %q, want custom.weird_token", a.Platforms[0].BotSecret)
	}
}

// TestWriteTestConfig_OmitKeys proves the three Omit* flags suppress
// exactly their keys: workspace, platform bot, and allowed_users are all
// absent from the generated agent block.
func TestWriteTestConfig_OmitKeys(t *testing.T) {
	cfg, _ := writeAndDecodeConfig(t, baseConfigOpts(AgentSpec{
		ID:                          "alpha",
		UserID:                      7,
		OmitWorkspaceKey:            true,
		OmitPlatformBotKey:          true,
		OmitPlatformAllowedUsersKey: true,
	}))

	a := cfg.Agents[0]
	if a.Workspace != nil {
		t.Errorf("workspace = %q, want key absent", *a.Workspace)
	}
	if a.Platforms[0].Bot != nil {
		t.Errorf("platform bot = %q, want key absent", *a.Platforms[0].Bot)
	}
	if got := a.Platforms[0].Access.AllowedUsers; got != nil {
		t.Errorf("allowed_users = %v, want key absent", got)
	}
}

// TestWriteTestConfig_ExtraTOMLNewlineWrapping proves extra TOML without
// leading/trailing newlines is still appended as a well-formed trailing
// section (the writer pads both ends).
func TestWriteTestConfig_ExtraTOMLNewlineWrapping(t *testing.T) {
	o := baseConfigOpts(AgentSpec{ID: "alpha", UserID: 7})
	o.ExtraConfigTOML = "[extra_section]\nfoo_extra = 99"
	cfg, raw := writeAndDecodeConfig(t, o)
	if cfg.ExtraSection == nil || cfg.ExtraSection.FooExtra != 99 {
		t.Errorf("extra_section = %+v, want foo_extra=99 from ExtraConfigTOML", cfg.ExtraSection)
	}
	if !strings.HasSuffix(raw, "foo_extra = 99\n") {
		t.Errorf("extra TOML not appended at end with trailing newline; tail: %q", raw[len(raw)-40:])
	}
}

// TestExtraConfigHasSection proves the header matcher accepts exact
// top-level `[name]` lines (with surrounding whitespace and CRLF) and
// rejects subsections, similarly-named tables, and empty input.
func TestExtraConfigHasSection(t *testing.T) {
	tests := []struct {
		name  string
		extra string
		want  bool
	}{
		{"empty extra", "", false},
		{"exact match", "[logging]\nevent_file = \"x\"\n", true},
		{"match mid-string", "x = 1\n[logging]\ny = 2\n", true},
		{"leading whitespace on line", "  [logging]\n", true},
		{"crlf line ending", "[logging]\r\n", true},
		{"subsection only", "[logging.rotation]\nmax = 1\n", false},
		{"similar prefix table", "[logging-other]\n", false},
		{"inner whitespace not canonical", "[ logging ]\n", false},
		{"different section", "[keepalive]\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extraConfigHasSection(tt.extra, "logging"); got != tt.want {
				t.Errorf("extraConfigHasSection(%q, logging) = %v, want %v", tt.extra, got, tt.want)
			}
		})
	}
}

// TestWriteTestSecrets proves the secrets writer emits the anthropic stub
// key and one telegram entry per agent, honours OmitDefaultPlatformSecret,
// and appends extra TOML (unpadded) as a valid trailing section.
func TestWriteTestSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.toml")
	writeTestSecrets(t, path, []AgentSpec{
		{ID: "alpha", BotToken: "tok-alpha"},
		{ID: "beta", BotToken: "tok-beta", OmitDefaultPlatformSecret: true},
	}, "[custom]\nweird_token = \"tok-custom\"")

	var got struct {
		Anthropic struct {
			APIKey string `toml:"api_key"`
		} `toml:"anthropic"`
		Telegram map[string]string `toml:"telegram"`
		Custom   map[string]string `toml:"custom"`
	}
	if _, err := toml.DecodeFile(path, &got); err != nil {
		t.Fatalf("generated secrets are not valid TOML: %v", err)
	}
	if got.Anthropic.APIKey == "" {
		t.Errorf("anthropic.api_key empty, want non-empty stub key")
	}
	if got.Telegram["alpha"] != "tok-alpha" {
		t.Errorf("telegram.alpha = %q, want tok-alpha", got.Telegram["alpha"])
	}
	if _, ok := got.Telegram["beta"]; ok {
		t.Errorf("telegram.beta emitted despite OmitDefaultPlatformSecret")
	}
	if got.Custom["weird_token"] != "tok-custom" {
		t.Errorf("custom.weird_token = %q, want tok-custom from extra TOML", got.Custom["weird_token"])
	}
}

// TestWriteWorkspaces proves each agent gets a workspace dir containing
// character/CRAFT.md and an empty memory/ dir, and the returned map keys
// every agent to its path under <tempDir>/workspaces.
func TestWriteWorkspaces(t *testing.T) {
	tempDir := t.TempDir()
	agents := []AgentSpec{{ID: "alpha"}, {ID: "beta"}}
	out := writeWorkspaces(t, tempDir, agents)

	if len(out) != 2 {
		t.Fatalf("returned map has %d entries, want 2", len(out))
	}
	for _, a := range agents {
		ws, ok := out[a.ID]
		if !ok {
			t.Fatalf("no workspace path returned for %s", a.ID)
		}
		if want := filepath.Join(tempDir, "workspaces", a.ID); ws != want {
			t.Errorf("workspace for %s = %q, want %q", a.ID, ws, want)
		}
		craft, err := os.ReadFile(filepath.Join(ws, "character", "CRAFT.md"))
		if err != nil {
			t.Errorf("missing seeded CRAFT.md for %s: %v", a.ID, err)
		} else if !strings.Contains(string(craft), "CRAFT.md") {
			t.Errorf("CRAFT.md for %s has unexpected content: %q", a.ID, craft)
		}
		info, err := os.Stat(filepath.Join(ws, "memory"))
		if err != nil || !info.IsDir() {
			t.Errorf("memory dir for %s missing or not a dir: %v", a.ID, err)
		}
	}
}
