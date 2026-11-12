package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Documents that read() panics on missing embedded files. This path is
// unreachable in production (all callers use hardcoded literals validated by
// TestEmbeddedFilesLoadNonEmpty) but exists as a developer safety net.
func TestReadPanicsOnMissingFile(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for missing embedded file")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "missing embedded file") {
			t.Errorf("unexpected panic value: %v", r)
		}
	}()
	read("nonexistent-file.md")
}

func TestEmbeddedFilesLoadNonEmpty(t *testing.T) {
	tests := []struct {
		name string
		fn   func() string
	}{
		{"BranchOrientationHeadless", BranchOrientationHeadless},
		{"BranchOrientationFacet", BranchOrientationFacet},
		{"CompactionSummary", CompactionSummary},
		{"CompactionHandoff", CompactionHandoff},
		{"Keepalive", Keepalive},
		{"Background", Background},
		{"Reflection", Reflection},
		{"MemoryConsolidation", MemoryConsolidation},
		{"FirstRun", FirstRun},
		{"WeeklyCharacterReview", WeeklyCharacterReview},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.fn()
			if got == "" {
				t.Errorf("%s() returned empty string", tt.name)
			}
		})
	}
}

func TestBranchOrientationHeadlessHasVars(t *testing.T) {
	text := BranchOrientationHeadless()
	for _, v := range []string{"{branch_type}", "{branch_key}", "{parent_key}"} {
		if !strings.Contains(text, v) {
			t.Errorf("headless orientation missing template var %s", v)
		}
	}
}

func TestBranchOrientationFacetHasVars(t *testing.T) {
	text := BranchOrientationFacet()
	for _, v := range []string{"{branch_type}", "{branch_key}", "{parent_key}"} {
		if !strings.Contains(text, v) {
			t.Errorf("facet orientation missing template var %s", v)
		}
	}
}

// TestPlatformSlackPrompt pins the Slack platform prompt's contract (#2459
// A1): it exists, tells the model its replies are already delivered, and
// names the two interim limits the deployment actually enforces — no tool
// approvals (denied at once) and no slash commands.
func TestPlatformSlackPrompt(t *testing.T) {
	text := Platform("slack")
	if text == "" {
		t.Fatal("Platform(\"slack\") is empty — platform-slack.md missing from the embed")
	}
	for _, want := range []string{"already delivered", "approval", "slash commands"} {
		if !strings.Contains(text, want) {
			t.Errorf("slack platform prompt missing %q:\n%s", want, text)
		}
	}
}

func TestResolvePromptEmptyPath(t *testing.T) {
	got := ResolvePrompt("", "test", "embedded-default")
	if got != "embedded-default" {
		t.Errorf("empty path: got %q, want %q", got, "embedded-default")
	}
}

func TestResolvePromptDefaultKeyword(t *testing.T) {
	got := ResolvePrompt("default", "test", "embedded-default")
	if got != "embedded-default" {
		t.Errorf("default keyword: got %q, want %q", got, "embedded-default")
	}
}

func TestResolvePromptNoneKeyword(t *testing.T) {
	got := ResolvePrompt("none", "test", "embedded-default")
	if got != "" {
		t.Errorf("none keyword: got %q, want empty", got)
	}
}

func TestResolvePromptFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.md")
	os.WriteFile(path, []byte("  custom content  "), 0644)

	got := ResolvePrompt(path, "test", "embedded-default")
	if got != "custom content" {
		t.Errorf("file exists: got %q, want %q", got, "custom content")
	}
}

func TestResolvePromptFileMissing(t *testing.T) {
	got := ResolvePrompt("/nonexistent/path/prompt.md", "test", "embedded-default")
	if got != "embedded-default" {
		t.Errorf("file missing: got %q, want %q", got, "embedded-default")
	}
}

func TestResolvePromptSearchDirsFirst(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("from-dir"), 0644)

	got := ResolvePrompt("", "prompt.md", "embedded-default", dir)
	if got != "from-dir" {
		t.Errorf("search dirs: got %q, want %q", got, "from-dir")
	}
}

func TestResolvePromptSearchDirsPriority(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir1, "prompt.md"), []byte("from-dir1"), 0644)
	os.WriteFile(filepath.Join(dir2, "prompt.md"), []byte("from-dir2"), 0644)

	got := ResolvePrompt("", "prompt.md", "embedded-default", dir1, dir2)
	if got != "from-dir1" {
		t.Errorf("search dir priority: got %q, want %q", got, "from-dir1")
	}
}

func TestResolvePromptSearchDirsFallthrough(t *testing.T) {
	dir := t.TempDir() // empty dir, no prompt.md
	got := ResolvePrompt("", "prompt.md", "embedded-default", dir)
	if got != "embedded-default" {
		t.Errorf("search dir fallthrough: got %q, want %q", got, "embedded-default")
	}
}

func TestResolvePromptExplicitPathOverridesSearchDirs(t *testing.T) {
	searchDir := t.TempDir()
	os.WriteFile(filepath.Join(searchDir, "prompt.md"), []byte("from-dir"), 0644)

	fileDir := t.TempDir()
	path := filepath.Join(fileDir, "explicit.md")
	os.WriteFile(path, []byte("explicit-content"), 0644)

	got := ResolvePrompt(path, "prompt.md", "embedded-default", searchDir)
	if got != "explicit-content" {
		t.Errorf("explicit path: got %q, want %q", got, "explicit-content")
	}
}

// TestFirstRunImportTextCarriesRequirements checks the import text (#3049)
// carries the points of the spec a test can read, and names no install.
func TestFirstRunImportTextCarriesRequirements(t *testing.T) {
	text := FirstRunImport("ask")
	lower := strings.ToLower(text)
	for _, want := range []string{
		"claude code", "cursor", "codex", // named tools
		"one `ask` call",                                           // one ask
		"one yes/no question per item",                             // one question per item
		"most impactful to least impactful",                        // ordering
		"stop at any time by pressing submit",                      // stop early
		"never name a destination file",                            // no destination named
		"never copy a credential",                                  // credentials
		"weave it into the existing text",                          // weave
		"remove any old content that an approved item contradicts", // drop contradicted
		"only after the basic introduction",                        // order in the interview
		"which other ai tools they use",                            // ask about other tools
		"only read",                                                // read-only
		"never use sudo",                                           // no elevated privileges
		"foci's own files are not a source",                        // foci files excluded
		"left unanswered, is not approved",                         // unanswered = no
		"send no ask",                                              // nothing found
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("import text lacks %q", want)
		}
	}
	// Generic: no person, host or user name of this install.
	for _, bad := range []string{"dick", "rich", "/home/", "nuc", "foci@"} {
		if strings.Contains(lower, bad) {
			t.Errorf("import text names something install-specific: %q", bad)
		}
	}
}
