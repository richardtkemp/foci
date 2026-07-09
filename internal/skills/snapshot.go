package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SkillSnapshot captures the state of all files across skill directories at a
// point in time. It maps each skill subdirectory (absolute path) to its files
// (path relative to the skill dir) and their modification times.
type SkillSnapshot map[string]map[string]time.Time

// Snapshot scans the given skill directories and returns a snapshot of every
// file in every skill subdirectory. Missing or unreadable directories are
// silently skipped. Each entry in dirs is expected to contain skill
// subdirectories (e.g. shared/skills/{name}/, workspace/skills/{name}/).
func Snapshot(dirs []string) SkillSnapshot {
	out := make(SkillSnapshot)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skillDir := filepath.Join(dir, entry.Name())
			files := scanSkillFiles(skillDir)
			if len(files) > 0 {
				out[skillDir] = files
			}
		}
	}
	return out
}

// scanSkillFiles walks skillDir recursively and returns a map of relative file
// paths to modification times. The directory itself is included even if it has
// no files (empty map) so that an empty skill dir is still tracked as existing.
func scanSkillFiles(skillDir string) map[string]time.Time {
	files := make(map[string]time.Time)
	_ = filepath.WalkDir(skillDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(skillDir, path)
		if err != nil {
			return nil
		}
		files[rel] = info.ModTime()
		return nil
	})
	return files
}

// SkillChange describes a single skill that was created or updated between two
// snapshots.
type SkillChange struct {
	Dir          string   // absolute path to the skill directory
	Name         string   // skill name (from frontmatter, best-effort)
	Description  string   // skill description (from frontmatter, best-effort)
	IsNew        bool     // true = skill directory is newly created
	CreatedFiles []string // files that appeared (relative paths)
	ChangedFiles []string // files whose mtime advanced (relative paths)
}

// Diff compares two snapshots and returns the set of skill-level changes. A
// skill directory present only in after is a creation; one present in both with
// new or modified files is an update. Deleted skills are not reported.
func Diff(before, after SkillSnapshot) []SkillChange {
	var changes []SkillChange

	for dir, afterFiles := range after {
		beforeFiles, existed := before[dir]
		if !existed {
			// New skill directory — all files are "created".
			var created []string
			for rel := range afterFiles {
				created = append(created, rel)
			}
			sort.Strings(created)
			name, desc := parseSkillMeta(dir)
			changes = append(changes, SkillChange{
				Dir:          dir,
				Name:         name,
				Description:  desc,
				IsNew:        true,
				CreatedFiles: created,
			})
			continue
		}

		// Existing skill — check for new or changed files.
		var created, changed []string
		for rel, afterMtime := range afterFiles {
			beforeMtime, had := beforeFiles[rel]
			if !had {
				created = append(created, rel)
			} else if afterMtime.After(beforeMtime) {
				changed = append(changed, rel)
			}
		}
		if len(created) == 0 && len(changed) == 0 {
			continue
		}
		sort.Strings(created)
		sort.Strings(changed)
		name, desc := parseSkillMeta(dir)
		changes = append(changes, SkillChange{
			Dir:          dir,
			Name:         name,
			Description:  desc,
			IsNew:        false,
			CreatedFiles: created,
			ChangedFiles: changed,
		})
	}

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].Name < changes[j].Name
	})
	return changes
}

// parseSkillMeta best-effort parses the SKILL.md frontmatter in dir to extract
// the skill name and description. Returns empty strings if unparseable.
func parseSkillMeta(dir string) (name, desc string) {
	skillFile := filepath.Join(dir, "SKILL.md")
	f, err := os.Open(skillFile)
	if err != nil {
		return filepath.Base(dir), ""
	}
	defer func() { _ = f.Close() }()

	fm, err := parseFrontmatter(f)
	if err != nil {
		return filepath.Base(dir), ""
	}
	name = fm["name"]
	if name == "" {
		name = filepath.Base(dir)
	}
	desc = fm["description"]
	return name, desc
}

// FormatChanges renders skill changes as a human-readable notification message.
// Returns empty string if there are no changes.
func FormatChanges(changes []SkillChange) string {
	if len(changes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range changes {
		if c.IsNew {
			b.WriteString(fmt.Sprintf("New skill created: %s\n%s\n", c.Name, c.Description))
		} else {
			b.WriteString(fmt.Sprintf("Skill updated: %s\n", c.Name))
			if len(c.CreatedFiles) > 0 {
				b.WriteString("Files created:\n")
				for _, f := range c.CreatedFiles {
					b.WriteString(fmt.Sprintf("  %s\n", f))
				}
			}
			if len(c.ChangedFiles) > 0 {
				b.WriteString("Files changed:\n")
				for _, f := range c.ChangedFiles {
					b.WriteString(fmt.Sprintf("  %s\n", f))
				}
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
