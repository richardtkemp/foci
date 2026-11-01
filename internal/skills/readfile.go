package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ReadScope is what a read tool call's path is judged against when deciding
// whether it loaded a skill file (#2392): the agent's skill directories and
// the workspace a relative path is relative to (the system prompt may list a
// skill's path relative to it — see Registry.SystemBlock / shortPath). The
// zero ReadScope knows no roots, so only the root-less SKILL.md rule
// (NameFromReadPath) applies.
type ReadScope struct {
	WorkDir string
	Dirs    Dirs
}

// FileRef names one markdown file inside a skill directory.
type FileRef struct {
	Skill string // the skill directory's name — the skill's name as chat shows it
	File  string // the file's path inside the skill directory, slash-separated ("SKILL.md", "method.md", "sub/x.md")
	Path  string // absolute LOGICAL path: as found under the skill root, cleaned, symlinks NOT resolved
}

// IsSkillMD reports whether the file is the skill's own SKILL.md.
func (f FileRef) IsSkillMD() bool { return f.File == "SKILL.md" }

// FileFromReadPath reports whether path, as a read tool call carried it,
// names a markdown file inside a skill directory under one of s's roots, and
// returns which. A relative path is taken relative to s.WorkDir (none = no
// match); the result is cleaned lexically, so the "../shared/skills/x/SKILL.md"
// form the system prompt can list resolves. Nothing is EvalSymlinks'd: a
// symlinked skill directory or file under a root matches by the path it was
// found at, the same identity resolveSkillDir gives a symlinked skill.
//
// A skill directory is what the loader treats as one: a direct child of a
// root holding a SKILL.md, or an extra dir that is itself a skill
// (scanExtraDir). When roots nest, the deepest matching root wins. The file
// must end in ".md".
func (s ReadScope) FileFromReadPath(path string) (FileRef, bool) {
	abs := s.absolute(path)
	if abs == "" || !strings.HasSuffix(abs, ".md") {
		return FileRef{}, false
	}
	var best FileRef
	bestRoot := ""
	consider := func(root string, rootIsSkill bool) {
		rel, ok := within(root, abs)
		if !ok || len(root) <= len(bestRoot) {
			return
		}
		if rootIsSkill {
			best, bestRoot = FileRef{Skill: filepath.Base(root), File: filepath.ToSlash(rel), Path: abs}, root
			return
		}
		skill, file, ok := strings.Cut(filepath.ToSlash(rel), "/")
		if !ok || file == "" || !IsSkillDir(filepath.Join(root, skill)) {
			return
		}
		best, bestRoot = FileRef{Skill: skill, File: file, Path: abs}, root
	}
	for _, root := range s.roots(s.Dirs.Shared, s.Dirs.Agent) {
		consider(root, false)
	}
	for _, root := range s.roots(s.Dirs.Extra...) {
		consider(root, IsSkillDir(root))
	}
	return best, bestRoot != ""
}

// ErrNotServable is ServableFile's refusal: the path is not a markdown file
// under one of the scope's skill roots. It never depends on whether anything
// exists at the path, so a refusal reveals nothing about the filesystem.
var ErrNotServable = errors.New("skills: not a skill markdown file")

// ServableFile reports whether path may be served to the user as a skill
// file's current text (#2392), and returns the cleaned path to open. The
// check is on the LOGICAL path, before any symlink is resolved (Dick
// 2026-10-13: symlinks under a root are followed, even to targets outside the
// roots): path must be absolute, contain no ".." component, end in ".md" and
// lie under one of s's roots. Anything else is ErrNotServable. The check is
// purely lexical (no I/O), so it reads the same whether or not the file
// exists; opening the returned path follows symlinks as usual.
func (s ReadScope) ServableFile(path string) (string, error) {
	if !filepath.IsAbs(path) || !strings.HasSuffix(path, ".md") {
		return "", ErrNotServable
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return "", ErrNotServable
		}
	}
	clean := filepath.Clean(path)
	for _, root := range s.roots(s.Dirs.List()...) {
		if _, ok := within(root, clean); ok {
			return clean, nil
		}
	}
	return "", ErrNotServable
}

// absolute returns path made absolute against s.WorkDir and cleaned, or ""
// when it cannot be (empty path; relative path with no WorkDir).
func (s ReadScope) absolute(path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if s.WorkDir == "" {
		return ""
	}
	return filepath.Join(s.WorkDir, path)
}

// roots returns dirs made absolute (against s.WorkDir) and cleaned, skipping
// empty entries and any that cannot be made absolute.
func (s ReadScope) roots(dirs ...string) []string {
	var out []string
	for _, d := range dirs {
		if r := s.absolute(d); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// within reports whether clean (absolute, cleaned) lies strictly below root
// (absolute, cleaned), returning its path relative to root.
func within(root, clean string) (string, bool) {
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// OpenServable opens the current file behind a ServableFile path for
// reading. It follows symlinks (the ruling above) and requires a regular file
// at the end of them: a directory, FIFO or device named *.md is
// ErrNotServable (checked before opening, so a FIFO cannot block the open);
// a missing file is an fs.ErrNotExist error; anything else is returned as is.
func OpenServable(path string) (*os.File, os.FileInfo, error) {
	if fi, err := os.Stat(path); err != nil {
		return nil, nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, nil, ErrNotServable
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() { // replaced between the Stat and the Open
		_ = f.Close()
		return nil, nil, ErrNotServable
	}
	return f, fi, nil
}
