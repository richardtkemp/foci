package skills

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// skillTree is a real temp filesystem for the #2392 skill-file rules:
//
//	home/shared/skills/                 shared root
//	  prediction/{SKILL.md,method.md,notes.txt,sub/deep.md}
//	  prediction/linkfile.md -> outside/secret.md   (symlinked file, target outside the roots)
//	  linked -> outside/linked                      (symlinked skill dir, target outside the roots)
//	  notaskill/x.md                                (a dir with no SKILL.md)
//	  README.md                                     (a file directly in the root)
//	ws/skills/mine/SKILL.md                         agent root (ws is the workspace)
//	extra-skill/{SKILL.md,doc.md}                   an extra dir that IS one skill
//	elsewhere/other/SKILL.md                        under no root
type skillTree struct {
	base, shared, ws, extra, outside, elsewhere string
	scope                                       ReadScope
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newSkillTree(t *testing.T) skillTree {
	t.Helper()
	base := t.TempDir()
	st := skillTree{
		base:      base,
		shared:    filepath.Join(base, "home", "shared", "skills"),
		ws:        filepath.Join(base, "ws"),
		extra:     filepath.Join(base, "extra-skill"),
		outside:   filepath.Join(base, "outside"),
		elsewhere: filepath.Join(base, "elsewhere"),
	}
	skillMD := func(name string) string { return "---\nname: " + name + "\ndescription: d\n---\nbody\n" }
	writeFile(t, filepath.Join(st.shared, "prediction", "SKILL.md"), skillMD("prediction"))
	writeFile(t, filepath.Join(st.shared, "prediction", "method.md"), "method\n")
	writeFile(t, filepath.Join(st.shared, "prediction", "notes.txt"), "notes\n")
	writeFile(t, filepath.Join(st.shared, "prediction", "sub", "deep.md"), "deep\n")
	writeFile(t, filepath.Join(st.shared, "notaskill", "x.md"), "x\n")
	writeFile(t, filepath.Join(st.shared, "README.md"), "readme\n")
	writeFile(t, filepath.Join(st.outside, "secret.md"), "outside file\n")
	writeFile(t, filepath.Join(st.outside, "linked", "SKILL.md"), skillMD("linked"))
	writeFile(t, filepath.Join(st.outside, "linked", "extra.md"), "linked extra\n")
	writeFile(t, filepath.Join(st.ws, "skills", "mine", "SKILL.md"), skillMD("mine"))
	writeFile(t, filepath.Join(st.extra, "SKILL.md"), skillMD("extra-skill"))
	writeFile(t, filepath.Join(st.extra, "doc.md"), "doc\n")
	writeFile(t, filepath.Join(st.elsewhere, "other", "SKILL.md"), skillMD("other"))
	if err := os.Symlink(filepath.Join(st.outside, "linked"), filepath.Join(st.shared, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(st.outside, "secret.md"), filepath.Join(st.shared, "prediction", "linkfile.md")); err != nil {
		t.Fatal(err)
	}
	st.scope = ReadScope{WorkDir: st.ws, Dirs: ResolveDirs(filepath.Join(base, "home"), st.ws, "", "", []string{st.extra})}
	return st
}

// TestReadScope_FileFromReadPath: a read of any .md file inside a skill
// directory under one of the agent's roots names that file — SKILL.md and
// sub-files alike, by absolute path, workspace-relative path (both forms
// SystemBlock can list) and through a symlinked skill dir — and anything else
// does not (#2392).
func TestReadScope_FileFromReadPath(t *testing.T) {
	st := newSkillTree(t)
	sh := func(p ...string) string { return filepath.Join(append([]string{st.shared}, p...)...) }
	tests := []struct {
		name string
		path string
		want FileRef
		ok   bool
	}{
		{"absolute SKILL.md", sh("prediction", "SKILL.md"), FileRef{"prediction", "SKILL.md", sh("prediction", "SKILL.md")}, true},
		{"absolute sub-file", sh("prediction", "method.md"), FileRef{"prediction", "method.md", sh("prediction", "method.md")}, true},
		{"nested sub-file", sh("prediction", "sub", "deep.md"), FileRef{"prediction", "sub/deep.md", sh("prediction", "sub", "deep.md")}, true},
		{"workspace-relative dot-slash SKILL.md", "./skills/mine/SKILL.md", FileRef{"mine", "SKILL.md", filepath.Join(st.ws, "skills", "mine", "SKILL.md")}, true},
		{"workspace-relative bare SKILL.md", "skills/mine/SKILL.md", FileRef{"mine", "SKILL.md", filepath.Join(st.ws, "skills", "mine", "SKILL.md")}, true},
		{"workspace-relative parent-hop sub-file", "../home/shared/skills/prediction/method.md", FileRef{"prediction", "method.md", sh("prediction", "method.md")}, true},
		{"symlinked skill dir SKILL.md", sh("linked", "SKILL.md"), FileRef{"linked", "SKILL.md", sh("linked", "SKILL.md")}, true},
		{"symlinked skill dir sub-file", sh("linked", "extra.md"), FileRef{"linked", "extra.md", sh("linked", "extra.md")}, true},
		{"symlinked file", sh("prediction", "linkfile.md"), FileRef{"prediction", "linkfile.md", sh("prediction", "linkfile.md")}, true},
		{"extra dir that is itself a skill", filepath.Join(st.extra, "doc.md"), FileRef{"extra-skill", "doc.md", filepath.Join(st.extra, "doc.md")}, true},

		{"non-md file in a skill dir", sh("prediction", "notes.txt"), FileRef{}, false},
		{"md in a dir with no SKILL.md", sh("notaskill", "x.md"), FileRef{}, false},
		{"md directly in a root", sh("README.md"), FileRef{}, false},
		{"SKILL.md under no root", filepath.Join(st.elsewhere, "other", "SKILL.md"), FileRef{}, false},
		{"resolved target of a symlinked skill dir", filepath.Join(st.outside, "linked", "extra.md"), FileRef{}, false},
		{"empty path", "", FileRef{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := st.scope.FileFromReadPath(tt.path)
			if ok != tt.ok || got != tt.want {
				t.Errorf("FileFromReadPath(%q) = (%+v, %v), want (%+v, %v)", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestReadScope_FileFromReadPath_NoWorkDirOrRoots: without a workspace a
// relative path cannot be placed, and the zero scope has no roots at all.
func TestReadScope_FileFromReadPath_NoWorkDirOrRoots(t *testing.T) {
	st := newSkillTree(t)
	noWS := st.scope
	noWS.WorkDir = ""
	if ref, ok := noWS.FileFromReadPath("./skills/mine/SKILL.md"); ok {
		t.Errorf("relative path with no WorkDir matched: %+v", ref)
	}
	if ref, ok := (ReadScope{}).FileFromReadPath(filepath.Join(st.shared, "prediction", "method.md")); ok {
		t.Errorf("zero scope matched: %+v", ref)
	}
}

// TestReadScope_ServableFile: the serve check is on the LOGICAL path — under a
// root, no ".." component, ".md" — and is decided without touching the disk,
// so a refusal reads the same whether or not the file exists (#2392).
func TestReadScope_ServableFile(t *testing.T) {
	st := newSkillTree(t)
	sh := func(p ...string) string { return filepath.Join(append([]string{st.shared}, p...)...) }
	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{"SKILL.md under the shared root", sh("prediction", "SKILL.md"), true},
		{"sub-file under the shared root", sh("prediction", "method.md"), true},
		{"file in the agent root", filepath.Join(st.ws, "skills", "mine", "SKILL.md"), true},
		{"file in an extra skill root", filepath.Join(st.extra, "doc.md"), true},
		{"symlinked skill dir, target outside the roots", sh("linked", "extra.md"), true},
		{"symlinked file, target outside the roots", sh("prediction", "linkfile.md"), true},
		{"missing file under a root is still servable (410 later)", sh("prediction", "gone.md"), true},
		{"duplicate slashes are cleaned", st.shared + "//prediction//method.md", true},

		{"dot-dot escape", sh("x") + "/../../etc/passwd.md", false},
		{"dot-dot that stays inside the root", sh("prediction") + "/../prediction/method.md", false},
		{"non-md under a root", sh("prediction", "notes.txt"), false},
		{"absolute path under no root (exists)", filepath.Join(st.elsewhere, "other", "SKILL.md"), false},
		{"absolute path under no root (missing)", filepath.Join(st.elsewhere, "nope", "SKILL.md"), false},
		{"resolved symlink target outside the roots", filepath.Join(st.outside, "secret.md"), false},
		{"sibling with the root as a name prefix", st.shared + "-evil/x/SKILL.md", false},
		{"the root itself", st.shared + ".md", false},
		{"relative path", "skills/mine/SKILL.md", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := st.scope.ServableFile(tt.path)
			if tt.ok {
				if err != nil || got != filepath.Clean(tt.path) {
					t.Errorf("ServableFile(%q) = (%q, %v), want (%q, nil)", tt.path, got, err, filepath.Clean(tt.path))
				}
				return
			}
			if !errors.Is(err, ErrNotServable) {
				t.Errorf("ServableFile(%q) = (%q, %v), want ErrNotServable", tt.path, got, err)
			}
		})
	}
	if _, err := (ReadScope{}).ServableFile(sh("prediction", "SKILL.md")); !errors.Is(err, ErrNotServable) {
		t.Errorf("zero scope served a path: %v", err)
	}
}

// TestOpenServable follows symlinks (even out of the roots), reports a
// missing file as fs.ErrNotExist, and refuses anything that is not a regular
// file at the end of its links.
func TestOpenServable(t *testing.T) {
	st := newSkillTree(t)
	read := func(path string) (string, error) {
		f, _, err := OpenServable(path)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(f)
		return string(b), err
	}
	if got, err := read(filepath.Join(st.shared, "prediction", "linkfile.md")); err != nil || got != "outside file\n" {
		t.Errorf("symlinked file: (%q, %v), want the target's content", got, err)
	}
	if got, err := read(filepath.Join(st.shared, "linked", "extra.md")); err != nil || got != "linked extra\n" {
		t.Errorf("symlinked skill dir: (%q, %v), want the target's content", got, err)
	}
	if _, err := read(filepath.Join(st.shared, "prediction", "gone.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v, want fs.ErrNotExist", err)
	}
	dirMD := filepath.Join(st.shared, "prediction", "dir.md")
	if err := os.Mkdir(dirMD, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := read(dirMD); !errors.Is(err, ErrNotServable) {
		t.Errorf("directory named .md: %v, want ErrNotServable", err)
	}
}
