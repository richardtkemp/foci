package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateAgentBlock(t *testing.T) {
	// Verifies agent block TOML generation with basic spec.
	spec := AgentSpec{
		ID:      "greek-tutor",
		HomeDir: "/home/foci",
	}

	result := GenerateAgentBlock(spec)

	checks := []string{
		"[[agents]]",
		`id = "greek-tutor"`,
		`workspace = "/home/foci/greek-tutor"`,
		`"character/SOUL.md"`,
	}
	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("missing %q in:\n%s", check, result)
		}
	}

	// No memory sources — left to sensible defaults
	if strings.Contains(result, "memory.sources") {
		t.Errorf("should not contain memory.sources:\n%s", result)
	}
}

func TestGenerateAgentBlockCustomSystemFiles(t *testing.T) {
	// Verifies system_files array in agent block.
	spec := AgentSpec{
		ID:          "scout",
		HomeDir:     "/home/foci",
		SystemFiles: []string{"character/SOUL.md", "character/CRAFT.md"},
	}

	result := GenerateAgentBlock(spec)

	if !strings.Contains(result, `"character/SOUL.md", "character/CRAFT.md"`) {
		t.Errorf("custom system_files not rendered:\n%s", result)
	}
}

func TestGenerateCrontabFromTemplate(t *testing.T) {
	// Verifies crontab template processing with substitutions.
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
	// Verifies staggering of absolute minute times for multiple agents.
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
	// Verifies GenerateCrontab errors on missing template file.
	_, err := GenerateCrontab("/nonexistent/crontab.template", AgentSpec{ID: "test"}, 0)
	if err == nil {
		t.Fatal("expected error for missing template")
	}
}
