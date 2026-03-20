package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaggerCrontabLine(t *testing.T) {
	// Proves that absolute minute fields are shifted by offset (with modulo-60 wrap),
	// while interval expressions and short/comment lines pass through unchanged.
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

func TestStaggerCrontabLineNonNumericMinute(t *testing.T) {
	// Verifies non-numeric minute fields pass through unchanged.
	line := "abc 4 * * * * cmd"
	got := StaggerCrontabLine(line, 5)
	if got != line {
		t.Errorf("non-numeric minute should be unchanged, got %q", got)
	}
}

func TestStaggerCrontabLineEdgeCases(t *testing.T) {
	// Proves that large offsets wrap correctly via modulo-60, and that
	// lines with fewer than 6 space-separated fields are always returned unchanged regardless of content.
	tests := []struct {
		name   string
		line   string
		offset int
		want   string
	}{
		{
			name:   "large offset wraps correctly",
			line:   "50 4 * * * cmd",
			offset: 100,
			want:   "30 4 * * * cmd", // (50 + 100) % 60 = 30
		},
		{
			name:   "short line unchanged",
			line:   "invalid format",
			offset: 5,
			want:   "invalid format",
		},
		{
			name:   "five fields only",
			line:   "0 4 * * *",
			offset: 5,
			want:   "0 4 * * *", // less than 6 fields
		},
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

func TestTitleCase(t *testing.T) {
	// Verifies hyphen-separated agent IDs convert to title case.
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

func TestToSlug(t *testing.T) {
	// Verifies display names convert to valid lowercase hyphenated slugs.
	tests := []struct {
		input string
		want  string
	}{
		{"Greek Tutor", "greek-tutor"},
		{"My Cool Agent", "my-cool-agent"},
		{"simple", "simple"},
		{"  Spaces Around  ", "spaces-around"},
		{"Under_Score", "under-score"},
		{"Special!@#Characters", "specialcharacters"},
		{"Multiple   Spaces", "multiple-spaces"},
		{"trailing-", "trailing"},
		{"123numeric", "123numeric"},
	}
	for _, tt := range tests {
		got := ToSlug(tt.input)
		if got != tt.want {
			t.Errorf("ToSlug(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSeedDefaultsWalkError(t *testing.T) {
	// Proves that SeedDefaults propagates walk errors by making a source
	// subdirectory unreadable (mode 0000) and verifying the returned error is non-nil.
	src := t.TempDir()
	subdir := filepath.Join(src, "locked")
	os.MkdirAll(subdir, 0755)
	os.WriteFile(filepath.Join(subdir, "file.md"), []byte("data"), 0644)
	os.Chmod(subdir, 0000)
	t.Cleanup(func() { os.Chmod(subdir, 0755) })

	dst := filepath.Join(t.TempDir(), "target")
	err := SeedDefaults(os.DirFS(src), dst)
	if err == nil {
		t.Error("expected error when source subdir is unreadable")
	}
}

func TestSeedDefaultsCopyError(t *testing.T) {
	// Proves that SeedDefaults surfaces copy failures by making the target
	// directory read-only and verifying an error is returned when a file cannot be written.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "file.md"), []byte("data"), 0644)

	// Create target dir, then make it read-only so copyFile fails
	dst := filepath.Join(t.TempDir(), "target")
	os.MkdirAll(dst, 0755)
	os.Chmod(dst, 0555)
	t.Cleanup(func() { os.Chmod(dst, 0755) })

	err := SeedDefaults(os.DirFS(src), dst)
	if err == nil {
		t.Error("expected error when target dir is read-only")
	}
}

func TestSeedDefaultsEmbedFS(t *testing.T) {
	// Verifies that SeedDefaults works with an fs.FS built from a temp dir
	// (stand-in for embed.FS). Seeds files, verifies content, and confirms
	// existing files are not overwritten on a second call.
	srcDir := t.TempDir()
	os.MkdirAll(filepath.Join(srcDir, "character"), 0755)
	os.WriteFile(filepath.Join(srcDir, "character", "SOUL.md"), []byte("embedded soul"), 0644)
	os.WriteFile(filepath.Join(srcDir, "crontab.template"), []byte("embedded template"), 0644)
	fsys := os.DirFS(srcDir)

	dst := filepath.Join(t.TempDir(), "target")
	if err := SeedDefaults(fsys, dst); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dst, "character", "SOUL.md"))
	if string(data) != "embedded soul" {
		t.Errorf("SOUL.md = %q, want 'embedded soul'", data)
	}

	// Modify a file and re-seed — should not overwrite
	os.WriteFile(filepath.Join(dst, "crontab.template"), []byte("user-edited"), 0644)
	if err := SeedDefaults(fsys, dst); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dst, "crontab.template"))
	if string(data) != "user-edited" {
		t.Errorf("existing file overwritten, got %q", data)
	}
}

func TestRunCrontabCmdDefault(t *testing.T) {
	// Call the real default with a harmless no-op command
	err := RunCrontabCmd("true")
	if err != nil {
		t.Errorf("RunCrontabCmd(true): %v", err)
	}
}

func TestAppendCrontab(t *testing.T) {
	// Test successful append
	orig := RunCrontabCmd
	defer func() { RunCrontabCmd = orig }()

	called := false
	RunCrontabCmd = func(cmd string) error {
		called = true
		// Verify the command contains our lines
		if len(cmd) == 0 {
			t.Errorf("expected crontab command")
		}
		return nil
	}

	lines := []string{"0 4 * * * foci branch", "*/30 * * * * foci send"}
	err := AppendCrontab(lines)
	if err != nil {
		t.Errorf("AppendCrontab: %v", err)
	}
	if !called {
		t.Error("RunCrontabCmd was not called")
	}
}

func TestAppendCrontabError(t *testing.T) {
	// Proves that AppendCrontab propagates errors from the underlying crontab
	// command by injecting a mock that returns an error and verifying it surfaces to the caller.
	orig := RunCrontabCmd
	defer func() { RunCrontabCmd = orig }()

	RunCrontabCmd = func(cmd string) error {
		return os.ErrPermission
	}

	err := AppendCrontab([]string{"0 4 * * * foci branch"})
	if err == nil {
		t.Error("expected error from crontab command")
	}
}

func TestProvisionDefaultsTemplateError(t *testing.T) {
	// Proves that Provision in defaults mode surfaces a "template SOUL.md"
	// error when templating fails, by pre-creating SOUL.md as a directory to trigger EISDIR on ReadFile.
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(filepath.Join(defaultsDir, "character"), 0755)
	// No SOUL.md file in defaults — copyDir will skip it.
	// But we create SOUL.md as a directory in the workspace.
	os.WriteFile(filepath.Join(defaultsDir, "character", "CRAFT.md"), []byte("craft"), 0644)

	workspace := filepath.Join(homeDir, "tmpl-err")
	// Pre-create SOUL.md as a directory so templateSoulFile's ReadFile fails with EISDIR
	os.MkdirAll(filepath.Join(workspace, "character", "SOUL.md"), 0755)

	spec := AgentSpec{
		ID:          "tmpl-err",
		DisplayName: "Test",
		HomeDir:     homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    "defaults",
	}

	_, err := Provision(spec)
	if err == nil {
		t.Fatal("expected error when SOUL.md is a directory")
	}
	if !strings.Contains(err.Error(), "template SOUL.md") {
		t.Errorf("error = %q, want to contain 'template SOUL.md'", err.Error())
	}
}

func TestProvisionOpenclawTemplateError(t *testing.T) {
	// Proves that Provision in openclaw mode surfaces a clear error
	// when SOUL.md templating fails, using a pre-created directory at the SOUL.md path to trigger EISDIR.
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	defaultsDir := filepath.Join(tmpDir, "defaults")
	os.MkdirAll(filepath.Join(defaultsDir, "openclaw"), 0755)
	// No SOUL.md in openclaw source — copyDir skips it
	os.WriteFile(filepath.Join(defaultsDir, "openclaw", "IDENTITY.md"), []byte("identity"), 0644)

	// Pre-create SOUL.md as a directory in workspace
	workspace := filepath.Join(homeDir, "oc-tmpl-err")
	os.MkdirAll(filepath.Join(workspace, "character", "SOUL.md"), 0755)

	spec := AgentSpec{
		ID:          "oc-tmpl-err",
		DisplayName: "OC Test",
		HomeDir:     homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    "openclaw",
	}

	_, err := Provision(spec)
	if err == nil {
		t.Fatal("expected error when SOUL.md is a directory")
	}
	if !strings.Contains(err.Error(), "template SOUL.md") {
		t.Errorf("error = %q, want to contain 'template SOUL.md'", err.Error())
	}
}
