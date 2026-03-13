package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionDefaults(t *testing.T) {
	// Verifies full agent provisioning from defaults mode.
	// Sets up defaults directory with character templates and crontab, provisions agent,
	// and verifies all files are created and templated correctly.
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
	// Verifies agent provisioning with openclaw character mode.
	// Uses openclaw directory template instead of defaults character directory.
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
	// Verifies agent provisioning with blank character mode.
	// Creates empty character files without copying from defaults.
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")

	spec := AgentSpec{
		ID:          "blank-agent",
		Model:       "claude-haiku-4-5-20251001",
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
	// Verifies agent provisioning in copy mode.
	// Copies character files from an existing agent's workspace.
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

func TestSeedDefaults(t *testing.T) {
	// Verifies seeding a defaults directory from a source.
	// Verifies that existing files are not overwritten.
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

func TestProvisionErrorCreatingWorkspace(t *testing.T) {
	// Tests Provision when workspace creation fails.
	tmpDir := t.TempDir()

	// Create a file where the workspace dir should be
	homeDir := filepath.Join(tmpDir, "home")
	agentPath := filepath.Join(homeDir, "agent-id")
	os.MkdirAll(homeDir, 0755)
	os.WriteFile(agentPath, []byte("conflict"), 0644)

	spec := AgentSpec{
		ID:          "agent-id",
		Model:       "claude-sonnet-4-6",
		HomeDir:     homeDir,
		DefaultsDir: filepath.Join(tmpDir, "defaults"),
		CharMode:    "blank",
	}

	_, err := Provision(spec)
	if err == nil {
		t.Error("expected error when workspace creation fails")
	}
}

func TestProvisionDefaultsCopyError(t *testing.T) {
	// Verifies Provision errors when defaults character dir is missing.
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	defaultsDir := filepath.Join(tmpDir, "defaults")
	// No character dir in defaults → copyDir will fail
	os.MkdirAll(defaultsDir, 0755)

	spec := AgentSpec{
		ID:          "err-agent",
		Model:       "claude-sonnet-4-6",
		HomeDir:     homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    "defaults",
	}

	_, err := Provision(spec)
	if err == nil {
		t.Fatal("expected error when defaults character dir is missing")
	}
	if !strings.Contains(err.Error(), "copy defaults") {
		t.Errorf("error = %q, want to contain 'copy defaults'", err)
	}
}

func TestProvisionOpenclawCopyError(t *testing.T) {
	// Verifies Provision errors when openclaw dir is missing.
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(defaultsDir, 0755) // no openclaw subdir

	spec := AgentSpec{
		ID:          "oc-err",
		Model:       "claude-sonnet-4-6",
		HomeDir:     homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    "openclaw",
	}

	_, err := Provision(spec)
	if err == nil {
		t.Fatal("expected error when openclaw dir is missing")
	}
	if !strings.Contains(err.Error(), "copy openclaw") {
		t.Errorf("error = %q, want to contain 'copy openclaw'", err)
	}
}

func TestProvisionCopySourceMissing(t *testing.T) {
	// Verifies Provision errors when source agent doesn't exist in copy mode.
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")

	spec := AgentSpec{
		ID:          "copy-err",
		Model:       "claude-sonnet-4-6",
		HomeDir:     homeDir,
		DefaultsDir: filepath.Join(tmpDir, "defaults"),
		CharMode:    "copy",
		CopyFrom:    "nonexistent-agent",
	}

	_, err := Provision(spec)
	if err == nil {
		t.Fatal("expected error when source agent doesn't exist")
	}
	if !strings.Contains(err.Error(), "copy from nonexistent-agent") {
		t.Errorf("error = %q, want to contain 'copy from nonexistent-agent'", err)
	}
}

func TestProvisionBlankWriteError(t *testing.T) {
	// Verifies Provision errors when character dir is read-only.
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	workspace := filepath.Join(homeDir, "blank-err")
	charDir := filepath.Join(workspace, "character")
	os.MkdirAll(charDir, 0755)

	// Make character dir read-only so WriteFile fails
	os.Chmod(charDir, 0555)
	t.Cleanup(func() { os.Chmod(charDir, 0755) })

	spec := AgentSpec{
		ID:          "blank-err",
		Model:       "claude-sonnet-4-6",
		HomeDir:     homeDir,
		DefaultsDir: filepath.Join(tmpDir, "defaults"),
		CharMode:    "blank",
	}

	_, err := Provision(spec)
	if err == nil {
		t.Error("expected error when character dir is read-only")
	}
}

func TestProvisionWithoutCrontabTemplate(t *testing.T) {
	// Verifies Provision succeeds with missing crontab template.
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(defaultsDir, 0755)

	spec := AgentSpec{
		ID:          "test-agent",
		Model:       "claude-sonnet-4-6",
		HomeDir:     homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    "blank",
	}

	result, err := Provision(spec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Should succeed with empty crontab lines when template is missing
	if len(result.CrontabLines) != 0 {
		t.Errorf("expected no crontab lines, got %d", len(result.CrontabLines))
	}
	if result.ConfigBlock == "" {
		t.Error("expected config block to be generated")
	}
}

func TestProvisionInvalidCharMode(t *testing.T) {
	// Tests Provision with invalid character mode.
	tmpDir := t.TempDir()
	spec := AgentSpec{
		ID:          "bad-agent",
		Model:       "claude-sonnet-4-6",
		HomeDir:     tmpDir,
		DefaultsDir: filepath.Join(tmpDir, "defaults"),
		CharMode:    "invalid",
	}

	_, err := Provision(spec)
	if err == nil {
		t.Error("expected error for invalid CharMode")
	}
	if !strings.Contains(err.Error(), "unknown character mode") {
		t.Errorf("error = %q, want to contain 'unknown character mode'", err.Error())
	}
}
