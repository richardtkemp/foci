package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Validation tests ---

func TestIsValidAgentID(t *testing.T) {
	valid := []string{"main", "fotini", "my-agent", "agent1", "a", "x123-test"}
	for _, s := range valid {
		if !IsValidAgentID(s) {
			t.Errorf("IsValidAgentID(%q) = false, want true", s)
		}
	}

	invalid := []string{"", "Main", "-start", "1start", "has space", "has_under", "has.dot"}
	for _, s := range invalid {
		if IsValidAgentID(s) {
			t.Errorf("IsValidAgentID(%q) = true, want false", s)
		}
	}
}

func TestIsValidBotToken(t *testing.T) {
	tests := []struct {
		token string
		want  bool
	}{
		{"123456789:AAF-abcdefghijklmnopqrstuv", true},
		{"7894561230:ABCdefGHIjklMNOpqrSTUvwxyz_-12345", true},
		{"12345:short", false},
		{"notanumber:AAF-abcdefghijklmnopqrstuv", false},
		{"", false},
		{"just-a-string", false},
	}
	for _, tt := range tests {
		got := IsValidBotToken(tt.token)
		if got != tt.want {
			t.Errorf("IsValidBotToken(%q) = %v, want %v", tt.token, got, tt.want)
		}
	}
}

func TestIsValidUserID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"12345678", true},
		{"123", true},
		{"12", false},
		{"", false},
		{"abc", false},
	}
	for _, tt := range tests {
		got := IsValidUserID(tt.id)
		if got != tt.want {
			t.Errorf("IsValidUserID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

// --- Model resolution tests ---

func TestResolveModelAlias(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opus", "claude-opus-4-6"},
		{"sonnet", "claude-sonnet-4-6"},
		{"haiku", "claude-haiku-4-5"},
		{"", "claude-sonnet-4-6"},
		{"claude-custom-model", "claude-custom-model"},
		{"OPUS", "claude-opus-4-6"},
		{"  Sonnet  ", "claude-sonnet-4-6"},
	}

	for _, tt := range tests {
		got := ResolveModelAlias(tt.input)
		if got != tt.want {
			t.Errorf("ResolveModelAlias(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- Config generation tests ---

func TestGenerateAgentBlock(t *testing.T) {
	spec := AgentSpec{
		ID:      "greek-tutor",
		Model:   "claude-sonnet-4-6",
		HomeDir: "/home/foci",
	}

	result := GenerateAgentBlock(spec)

	checks := []string{
		"[[agents]]",
		`id = "greek-tutor"`,
		`model = "claude-sonnet-4-6"`,
		`workspace = "/home/foci/greek-tutor"`,
		`"character/SOUL.md"`,
	}
	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("missing %q in:\n%s", check, result)
		}
	}

	// BotName == ID → no telegram_bot line
	if strings.Contains(result, "telegram_bot") {
		t.Errorf("should not contain telegram_bot when botName == id:\n%s", result)
	}
	// Convention key → no bot_secret line
	if strings.Contains(result, "bot_secret") {
		t.Errorf("should not contain bot_secret:\n%s", result)
	}
	// No memory sources — left to sensible defaults
	if strings.Contains(result, "memory.sources") {
		t.Errorf("should not contain memory.sources:\n%s", result)
	}
}

func TestGenerateAgentBlockCustomSystemFiles(t *testing.T) {
	spec := AgentSpec{
		ID:          "scout",
		Model:       "claude-haiku-4-5",
		HomeDir:     "/home/foci",
		SystemFiles: []string{"character/SOUL.md", "character/CRAFT.md"},
	}

	result := GenerateAgentBlock(spec)

	if !strings.Contains(result, `"character/SOUL.md", "character/CRAFT.md"`) {
		t.Errorf("custom system_files not rendered:\n%s", result)
	}
}

// --- Crontab tests ---

func TestGenerateCrontabFromTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	template := `# Comment line
0 4 * * * foci branch --oneshot -a AGENT_NAME -mf HOMEDIR/shared/prompts/review.md 2>&1 >> HOMEDIR/logs/cron.log
*/30 * * * * foci send -a AGENT_NAME "[keepalive]" 2>&1 >> HOMEDIR/logs/cron.log
`
	os.WriteFile(filepath.Join(tmpDir, "crontab.template"), []byte(template), 0644)

	spec := AgentSpec{
		ID:      "helen",
		HomeDir: "/home/foci",
	}
	lines, err := GenerateCrontab(filepath.Join(tmpDir, "crontab.template"), spec, 0)
	if err != nil {
		t.Fatalf("GenerateCrontab: %v", err)
	}

	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "#") {
		t.Errorf("comments should be stripped:\n%s", joined)
	}
	if strings.Contains(joined, "AGENT_NAME") {
		t.Errorf("AGENT_NAME not replaced:\n%s", joined)
	}
	if !strings.Contains(joined, "foci branch --oneshot -a helen") {
		t.Errorf("missing agent name substitution:\n%s", joined)
	}
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(lines))
	}
}

func TestGenerateCrontabStagger(t *testing.T) {
	tmpDir := t.TempDir()
	template := `0 4 * * * foci branch --oneshot -a AGENT_NAME cmd
*/30 * * * * foci branch --oneshot -a AGENT_NAME keepalive
`
	os.WriteFile(filepath.Join(tmpDir, "crontab.template"), []byte(template), 0644)

	spec := AgentSpec{
		ID:      "fourth",
		HomeDir: "/home/foci",
	}
	lines, err := GenerateCrontab(filepath.Join(tmpDir, "crontab.template"), spec, 3)
	if err != nil {
		t.Fatal(err)
	}

	// "0 4" should become "9 4" (3 agents × 3 = offset 9)
	if !strings.HasPrefix(lines[0], "9 4 ") {
		t.Errorf("expected staggered minute 9, got: %s", lines[0])
	}
	// Interval entry should not be staggered
	if !strings.HasPrefix(lines[1], "*/30 ") {
		t.Errorf("interval should not be staggered: %s", lines[1])
	}
}

func TestGenerateCrontabMissing(t *testing.T) {
	_, err := GenerateCrontab("/nonexistent/crontab.template", AgentSpec{ID: "test"}, 0)
	if err == nil {
		t.Fatal("expected error for missing template")
	}
}

func TestStaggerCrontabLine(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		offset int
		want   string
	}{
		{"absolute minute", "0 4 * * * cmd", 9, "9 4 * * * cmd"},
		{"wrap at 60", "55 4 * * * cmd", 9, "4 4 * * * cmd"},
		{"interval unchanged", "*/30 * * * * cmd", 9, "*/30 * * * * cmd"},
		{"short line unchanged", "# comment", 5, "# comment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StaggerCrontabLine(tt.line, tt.offset)
			if got != tt.want {
				t.Errorf("StaggerCrontabLine(%q, %d) = %q, want %q", tt.line, tt.offset, got, tt.want)
			}
		})
	}
}

// --- File operation tests ---

func TestTemplateSoulFile(t *testing.T) {
	tmpDir := t.TempDir()
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	os.WriteFile(soulPath, []byte("- **Name:** <!-- your name -->\n"), 0644)

	if err := templateSoulFile(soulPath, "Greek Tutor"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(soulPath)
	if !strings.Contains(string(data), "**Name:** Greek Tutor") {
		t.Errorf("name not substituted: %s", data)
	}
}

func TestTemplateSoulFileMissing(t *testing.T) {
	if err := templateSoulFile(filepath.Join(t.TempDir(), "nope.md"), "Name"); err != nil {
		t.Errorf("expected no error for missing file, got: %v", err)
	}
}

func TestTemplateSoulFileEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	os.WriteFile(soulPath, []byte("- **Name:** <!-- your name -->\n"), 0644)

	// Empty display name should not modify
	if err := templateSoulFile(soulPath, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(soulPath)
	if !strings.Contains(string(data), "<!-- your name -->") {
		t.Errorf("empty name should leave placeholder: %s", data)
	}
}

func TestCopyDir(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.md"), []byte("aaa"), 0644)
	os.WriteFile(filepath.Join(src, "b.md"), []byte("bbb"), 0644)
	os.MkdirAll(filepath.Join(src, "subdir"), 0755) // should be skipped

	dst := filepath.Join(t.TempDir(), "target")
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dst, "a.md"))
	if string(data) != "aaa" {
		t.Errorf("a.md = %q", data)
	}
	data, _ = os.ReadFile(filepath.Join(dst, "b.md"))
	if string(data) != "bbb" {
		t.Errorf("b.md = %q", data)
	}
}

// --- Provision integration tests ---

func TestProvisionDefaults(t *testing.T) {
	tmpDir := t.TempDir()

	// Set up defaults directory
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(filepath.Join(defaultsDir, "character"), 0755)
	os.MkdirAll(filepath.Join(defaultsDir, "prompts"), 0755)
	os.WriteFile(filepath.Join(defaultsDir, "character", "SOUL.md"), []byte("- **Name:** <!-- your name -->\n"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "character", "CRAFT.md"), []byte("craft content"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "prompts", "KEEPALIVE.md"), []byte("keepalive"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "crontab.template"), []byte("0 4 * * * foci branch -a AGENT_NAME\n"), 0644)

	homeDir := filepath.Join(tmpDir, "home")
	spec := AgentSpec{
		ID:          "test-agent",
		Model:       "claude-sonnet-4-6",
		DisplayName: "Test Agent",
		HomeDir:     homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    "defaults",
	}

	result, err := Provision(spec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Check workspace dirs
	for _, dir := range []string{"character", "memory", "prompts"} {
		if _, err := os.Stat(filepath.Join(result.Workspace, dir)); os.IsNotExist(err) {
			t.Errorf("directory %s not created", dir)
		}
	}

	// Check SOUL.md was templated
	data, _ := os.ReadFile(filepath.Join(result.Workspace, "character", "SOUL.md"))
	if !strings.Contains(string(data), "**Name:** Test Agent") {
		t.Errorf("SOUL.md not templated: %s", data)
	}

	// Check keepalive was copied
	data, _ = os.ReadFile(filepath.Join(result.Workspace, "prompts", "KEEPALIVE.md"))
	if string(data) != "keepalive" {
		t.Errorf("KEEPALIVE.md = %q", data)
	}

	// Check config block
	if !strings.Contains(result.ConfigBlock, `id = "test-agent"`) {
		t.Errorf("config block missing agent id:\n%s", result.ConfigBlock)
	}

	// Check crontab lines
	if len(result.CrontabLines) != 1 {
		t.Errorf("expected 1 crontab line, got %d", len(result.CrontabLines))
	}
}

func TestProvisionOpenclaw(t *testing.T) {
	tmpDir := t.TempDir()

	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(filepath.Join(defaultsDir, "openclaw"), 0755)
	os.WriteFile(filepath.Join(defaultsDir, "openclaw", "SOUL.md"), []byte("openclaw soul <!-- your name -->\n"), 0644)
	os.WriteFile(filepath.Join(defaultsDir, "openclaw", "IDENTITY.md"), []byte("identity"), 0644)

	homeDir := filepath.Join(tmpDir, "home")
	spec := AgentSpec{
		ID:          "oc-agent",
		Model:       "claude-sonnet-4-6",
		DisplayName: "OC Agent",
		HomeDir:     homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    "openclaw",
	}

	result, err := Provision(spec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Check openclaw files were copied
	data, _ := os.ReadFile(filepath.Join(result.Workspace, "character", "IDENTITY.md"))
	if string(data) != "identity" {
		t.Errorf("IDENTITY.md = %q", data)
	}

	// Check SOUL.md was templated
	data, _ = os.ReadFile(filepath.Join(result.Workspace, "character", "SOUL.md"))
	if !strings.Contains(string(data), "openclaw soul OC Agent") {
		t.Errorf("SOUL.md not templated: %s", data)
	}
}

func TestProvisionBlank(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")

	spec := AgentSpec{
		ID:          "blank-agent",
		Model:       "claude-haiku-4-5",
		HomeDir:     homeDir,
		DefaultsDir: filepath.Join(tmpDir, "nonexistent"),
		CharMode:    "blank",
	}

	result, err := Provision(spec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	for _, name := range DefaultCharacterFileNames {
		path := filepath.Join(result.Workspace, "character", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("missing %s: %v", name, err)
			continue
		}
		if len(data) != 0 {
			t.Errorf("%s should be empty, got %q", name, data)
		}
	}
}

func TestProvisionCopy(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")

	// Create source agent's workspace
	sourceChar := filepath.Join(homeDir, "source-agent", "character")
	os.MkdirAll(sourceChar, 0755)
	os.WriteFile(filepath.Join(sourceChar, "SOUL.md"), []byte("source soul"), 0644)

	spec := AgentSpec{
		ID:          "copy-agent",
		Model:       "claude-sonnet-4-6",
		HomeDir:     homeDir,
		DefaultsDir: filepath.Join(tmpDir, "defaults"),
		CharMode:    "copy",
		CopyFrom:    "source-agent",
	}

	result, err := Provision(spec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(result.Workspace, "character", "SOUL.md"))
	if string(data) != "source soul" {
		t.Errorf("SOUL.md = %q, want source soul", data)
	}
}

// --- SeedDefaults tests ---

func TestSeedDefaults(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "character"), 0755)
	os.WriteFile(filepath.Join(src, "character", "SOUL.md"), []byte("soul"), 0644)
	os.WriteFile(filepath.Join(src, "crontab.template"), []byte("template"), 0644)

	dst := filepath.Join(t.TempDir(), "target")
	if err := SeedDefaults(src, dst); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dst, "character", "SOUL.md"))
	if string(data) != "soul" {
		t.Errorf("SOUL.md = %q", data)
	}
	data, _ = os.ReadFile(filepath.Join(dst, "crontab.template"))
	if string(data) != "template" {
		t.Errorf("crontab.template = %q", data)
	}

	// Run again — existing files should not be overwritten
	os.WriteFile(filepath.Join(dst, "crontab.template"), []byte("edited"), 0644)
	if err := SeedDefaults(src, dst); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dst, "crontab.template"))
	if string(data) != "edited" {
		t.Errorf("existing file should not be overwritten, got %q", data)
	}
}

// --- TitleCase tests ---

func TestTitleCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"greek-tutor", "Greek Tutor"},
		{"main", "Main"},
		{"my-cool-agent", "My Cool Agent"},
		{"a", "A"},
	}
	for _, tt := range tests {
		got := TitleCase(tt.input)
		if got != tt.want {
			t.Errorf("TitleCase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
