package skills

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/log"
)

// Skill represents a loaded skill from a SKILL.md file.
type Skill struct {
	Name        string // from frontmatter
	Description string // from frontmatter
	Command     string // optional slash command (e.g. "/reheat")
	Script      string // optional script path (absolute, resolved from skill dir)
	Dir         string // absolute path to skill directory
	Path        string // absolute path to SKILL.md
}

// Registry holds loaded skills.
type Registry struct {
	skills []Skill
}

// Load scans directories for subdirectories containing SKILL.md files,
// parses their YAML frontmatter, and returns a registry.
func Load(dirs []string) *Registry {
	r := &Registry{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			log.Warnf("skills", "scan %s: %v", dir, err)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skillDir := filepath.Join(dir, entry.Name())
			skillFile := filepath.Join(skillDir, "SKILL.md")
			skill, err := parseSkillFile(skillFile, skillDir)
			if err != nil {
				log.Warnf("skills", "skip %s: %v", skillFile, err)
				continue
			}
			r.skills = append(r.skills, skill)
			log.Infof("skills", "loaded: %s (%s)", skill.Name, skill.Dir)
		}
	}
	return r
}

// All returns all loaded skills.
func (r *Registry) All() []Skill {
	return r.skills
}

// Len returns the number of loaded skills.
func (r *Registry) Len() int {
	return len(r.skills)
}

// SystemBlock returns a formatted text block listing available skills,
// suitable for injection into the system prompt. Returns empty string
// if no skills are loaded. workDir is the agent's workspace directory;
// if non-empty, skill paths are shown as the shorter of absolute or
// relative-to-workDir.
func (r *Registry) SystemBlock(workDir string) string {
	if len(r.skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Available Skills\n\n")
	b.WriteString("Use the read tool to read a skill's SKILL.md for full instructions.\n\n")
	for _, s := range r.skills {
		b.WriteString(fmt.Sprintf("- %s (%s): %s\n", s.Name, shortPath(s.Path, workDir), s.Description))
	}
	return b.String()
}

// CheckSizes returns warning strings for any skill whose SKILL.md exceeds
// maxChars bytes. Returns nil if maxChars <= 0 (guard disabled) or no skills
// exceed the limit.
func (r *Registry) CheckSizes(maxChars int) []string {
	if maxChars <= 0 {
		return nil
	}
	var warnings []string
	for _, s := range r.skills {
		info, err := os.Stat(s.Path)
		if err != nil {
			continue
		}
		if size := int(info.Size()); size > maxChars {
			warnings = append(warnings, fmt.Sprintf(
				"skill %q SKILL.md is %d bytes, exceeds max_result_chars (%d) — agent will see truncated instructions",
				s.Name, size, maxChars,
			))
		}
	}
	return warnings
}

// shortPath returns the shorter of absPath or its relative form from baseDir.
// If baseDir is empty or filepath.Rel fails, absPath is returned unchanged.
func shortPath(absPath, baseDir string) string {
	if baseDir == "" {
		return absPath
	}
	rel, err := filepath.Rel(baseDir, absPath)
	if err != nil {
		return absPath
	}
	if len(rel) < len(absPath) {
		// Prefix with "./" if the relative path doesn't already start
		// with "../", so it's visually clear it's a relative path.
		if !strings.HasPrefix(rel, "..") {
			rel = "./" + rel
		}
		return rel
	}
	return absPath
}

// parseSkillFile reads a SKILL.md and extracts frontmatter fields.
func parseSkillFile(path, dir string) (Skill, error) {
	f, err := os.Open(path)
	if err != nil {
		return Skill{}, err
	}
	defer func() { _ = f.Close() }()

	fm, err := parseFrontmatter(f)
	if err != nil {
		return Skill{}, err
	}

	name := fm["name"]
	if name == "" {
		return Skill{}, fmt.Errorf("missing required field: name")
	}

	skill := Skill{
		Name:        name,
		Description: fm["description"],
		Command:     fm["command"],
		Dir:         dir,
		Path:        path,
	}

	// Resolve script path relative to skill directory
	if fm["script"] != "" {
		skill.Script = filepath.Join(dir, fm["script"])
	}

	return skill, nil
}

// parseFrontmatter reads YAML frontmatter between --- markers.
// Returns a map of key: value pairs. Only handles simple scalar values.
func parseFrontmatter(f *os.File) (map[string]string, error) {
	scanner := bufio.NewScanner(f)

	// First line must be ---
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return nil, fmt.Errorf("no frontmatter")
	}

	fm := make(map[string]string)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			return fm, nil
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Strip surrounding quotes
		val = strings.Trim(val, `"'`)
		if key != "" {
			fm[key] = val
		}
	}

	return nil, fmt.Errorf("unterminated frontmatter")
}
