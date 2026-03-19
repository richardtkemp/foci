// Package provision implements shared agent creation logic used by both
// foci first-run (first-run wizard) and /agents new (runtime command).
// It is a leaf package with no dependencies beyond the standard library.
package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultSystemFiles is the list of workspace-relative character file paths
// written to system_files in generated config.
var DefaultSystemFiles = []string{
	"character/SOUL.md",
	"character/COHERENCE.md",
	"character/CRAFT.md",
	"character/USER.md",
	"character/MEMORY.md",
}

// DefaultCharacterFileNames is the set of known character file basenames.
var DefaultCharacterFileNames = []string{
	"SOUL.md",
	"COHERENCE.md",
	"CRAFT.md",
	"USER.md",
	"MEMORY.md",
}

// AgentSpec describes the inputs needed to provision a new agent workspace.
type AgentSpec struct {
	ID          string // slug: "greek-tutor"
	DisplayName string // "Greek Tutor" (optional)
	HomeDir     string // workspace parent: /home/foci
	DefaultsDir string // shared/ root (in repo or on disk)
	CharMode    string // "defaults", "openclaw", "copy", "import", "blank"
	CopyFrom    string // source agent ID when CharMode=="copy"
	SystemFiles []string // nil → DefaultSystemFiles
}

// Result holds the outputs of a successful Provision call.
type Result struct {
	Workspace    string   // full path to workspace dir
	ConfigBlock  string   // [[agents]] TOML fragment
	CrontabLines []string // generated crontab entries (may be empty)
}

// workspacePath returns the full workspace directory path.
func (s AgentSpec) workspacePath() string {
	return filepath.Join(s.HomeDir, s.ID)
}

// Provision creates an agent workspace and returns config/crontab data.
// The caller is responsible for appending config and installing crontab.
func Provision(spec AgentSpec) (*Result, error) {
	workspace := spec.workspacePath()

	// 1. Create workspace directories
	for _, dir := range []string{"character", "memory", "prompts", ".data"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	// 2. Character files based on CharMode
	switch spec.CharMode {
	case "defaults":
		if err := copyDefaultFiles(spec.DefaultsDir, workspace); err != nil {
			return nil, fmt.Errorf("copy defaults: %w", err)
		}
		if err := templateSoulFile(filepath.Join(workspace, "character", "SOUL.md"), spec.DisplayName); err != nil {
			return nil, fmt.Errorf("template SOUL.md: %w", err)
		}

	case "openclaw":
		openclawDir := filepath.Join(spec.DefaultsDir, "openclaw")
		if err := copyDir(openclawDir, filepath.Join(workspace, "character")); err != nil {
			return nil, fmt.Errorf("copy openclaw: %w", err)
		}
		if err := templateSoulFile(filepath.Join(workspace, "character", "SOUL.md"), spec.DisplayName); err != nil {
			return nil, fmt.Errorf("template SOUL.md: %w", err)
		}

	case "copy":
		sourceWorkspace := filepath.Join(spec.HomeDir, spec.CopyFrom)
		if err := copyDir(filepath.Join(sourceWorkspace, "character"), filepath.Join(workspace, "character")); err != nil {
			return nil, fmt.Errorf("copy from %s: %w", spec.CopyFrom, err)
		}

	case "import":
		// Dirs already created above; caller handles the interactive file import.

	case "blank":
		for _, name := range DefaultCharacterFileNames {
			path := filepath.Join(workspace, "character", name)
			if err := os.WriteFile(path, []byte(""), 0644); err != nil {
				return nil, fmt.Errorf("create %s: %w", name, err)
			}
		}

	default:
		return nil, fmt.Errorf("unknown character mode: %q", spec.CharMode)
	}

	// 3. Generate [[agents]] TOML config block
	configBlock := GenerateAgentBlock(spec)

	// 4. Generate crontab entries (best-effort)
	var crontabLines []string
	templatePath := filepath.Join(spec.DefaultsDir, "crontab.template")
	if lines, err := GenerateCrontab(templatePath, spec, 0); err == nil {
		crontabLines = lines
	}

	return &Result{
		Workspace:    workspace,
		ConfigBlock:  configBlock,
		CrontabLines: crontabLines,
	}, nil
}

// copyDefaultFiles copies default character files to a new workspace.
func copyDefaultFiles(defaultsDir, workspace string) error {
	charSrc := filepath.Join(defaultsDir, "character")
	charDst := filepath.Join(workspace, "character")
	return copyDir(charSrc, charDst)
}

// SeedDefaults copies the shared/ directory tree from the repo to
// a target directory on disk, creating it if needed. Skips files that already exist.
func SeedDefaults(repoDefaultsDir, targetDefaultsDir string) error {
	return filepath.Walk(repoDefaultsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(repoDefaultsDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(targetDefaultsDir, rel)

		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		// Skip if target already exists
		if _, err := os.Stat(target); err == nil {
			return nil
		}

		return copyFile(path, target)
	})
}

// TitleCase converts a hyphenated slug to title case.
// "greek-tutor" → "Greek Tutor"
func TitleCase(slug string) string {
	words := strings.Split(slug, "-")
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// ToSlug converts a display name to a lowercase hyphenated slug.
// "Greek Tutor" → "greek-tutor"
// Non-alphanumeric characters (except hyphens) are stripped.
func ToSlug(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '-' || r == '_':
			if b.Len() > 0 && !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	s := b.String()
	return strings.TrimRight(s, "-")
}
