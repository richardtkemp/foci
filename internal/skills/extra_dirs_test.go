package skills

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// #2657: configurable extra skill directories, loaded IN ADDITION to the
// shared and per-agent dirs.

func TestResolveDirsExtraOrder(t *testing.T) {
	// Extra dirs sit between shared and per-agent, in the order listed.
	d := ResolveDirs("/home/foci", "/home/foci/clutch", "", "", []string{"/x/one", "/x/two"})
	want := []string{"/home/foci/shared/skills", "/x/one", "/x/two", "/home/foci/clutch/skills"}
	if got := d.List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
}

func TestResolveDirsExtraDropsDuplicatesAndDefaults(t *testing.T) {
	// An extra naming the shared or per-agent dir (after Clean), a repeat, or
	// an empty entry is dropped: each dir has exactly one position.
	d := ResolveDirs("/home/foci", "/home/foci/clutch", "", "",
		[]string{"", "/home/foci/shared/skills/", "/x/one", "/x//one", "/home/foci/clutch/skills", "/x/two"})
	if want := []string{"/x/one", "/x/two"}; !reflect.DeepEqual(d.Extra, want) {
		t.Fatalf("Extra = %v, want %v", d.Extra, want)
	}
}

func TestLiveDirsNilSafeAndCopies(t *testing.T) {
	var nilLD *LiveDirs
	if got := nilLD.List(); got != nil {
		t.Errorf("nil LiveDirs List() = %v, want nil", got)
	}
	extra := []string{"/x/one"}
	ld := NewLiveDirs(Dirs{Shared: "/s", Extra: extra, Agent: "/a"})
	extra[0] = "/mutated"
	if got := ld.Get().Extra[0]; got != "/x/one" {
		t.Errorf("LiveDirs aliases caller's slice: Extra[0] = %q", got)
	}
	ld.Set(Dirs{Shared: "/s", Agent: "/a", Extra: []string{"/x/two"}})
	if want := []string{"/s", "/x/two", "/a"}; !reflect.DeepEqual(ld.List(), want) {
		t.Errorf("after Set, List() = %v, want %v", ld.List(), want)
	}
}

// extraFixture builds shared / extra1 / extra2 / agent dirs. Every dir holds a
// unique skill; "clash" sits in all four (with distinct descriptions) so the
// winner is visible, and "se" sits in shared+extra1 only.
func extraFixture(t *testing.T) Dirs {
	t.Helper()
	root := t.TempDir()
	d := Dirs{
		Shared: filepath.Join(root, "shared"),
		Extra:  []string{filepath.Join(root, "extra1"), filepath.Join(root, "extra2")},
		Agent:  filepath.Join(root, "agent"),
	}
	for _, dir := range d.List() {
		tag := filepath.Base(dir)
		writeSkillMD(t, dir, "only-"+tag, "---\nname: only-"+tag+"\ndescription: from "+tag+"\n---\n")
		writeSkillMD(t, dir, "clash", "---\nname: clash\ndescription: from "+tag+"\n---\n")
	}
	writeSkillMD(t, d.Shared, "se", "---\nname: se\ndescription: from shared\n---\n")
	writeSkillMD(t, d.Extra[0], "se", "---\nname: se\ndescription: from extra1\n---\n")
	return d
}

func TestLoadDirsIncludesExtraSkills(t *testing.T) {
	captureSkillsLog(t)
	d := extraFixture(t)
	for name, reg := range map[string]*Registry{
		"LoadDirs":     LoadDirs(d),
		"LoadForAgent": NewLoader().LoadForAgent(d),
	} {
		for _, want := range []string{"only-shared", "only-extra1", "only-extra2", "only-agent"} {
			if !hasSkill(reg, want) {
				t.Errorf("%s: missing %s; have %v", name, want, names(reg))
			}
		}
		// Later dir wins: agent > extra2 > extra1 > shared.
		if got := skillDesc(reg, "clash"); got != "from agent" {
			t.Errorf("%s: clash winner = %q, want from agent", name, got)
		}
		if got := skillDesc(reg, "se"); got != "from extra1" {
			t.Errorf("%s: se winner = %q, want from extra1", name, got)
		}
		if n := countSkill(reg, "clash"); n != 1 {
			t.Errorf("%s: %d clash entries, want 1", name, n)
		}
		// The prompt block lists extra-dir skills with their real path.
		block := reg.SystemBlock("")
		p := filepath.Join(d.Extra[1], "only-extra2", "SKILL.md")
		if !strings.Contains(block, "- only-extra2 ("+p+"): from extra2") {
			t.Errorf("%s: SystemBlock lacks the extra skill line with path %s:\n%s", name, p, block)
		}
	}
}

// logLines returns the log lines containing every needle.
func logLines(out string, needles ...string) []string {
	var hits []string
	for _, l := range strings.Split(out, "\n") {
		ok := true
		for _, n := range needles {
			if !strings.Contains(l, n) {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, l)
		}
	}
	return hits
}

func TestLoadDirsClashLogging(t *testing.T) {
	for _, loader := range []string{"LoadDirs", "LoadForAgent"} {
		t.Run(loader, func(t *testing.T) {
			buf := captureSkillsLog(t)
			d := extraFixture(t)
			if loader == "LoadDirs" {
				LoadDirs(d)
			} else {
				NewLoader().LoadForAgent(d)
			}
			out := buf.String()
			// A clash involving an extra dir is a WARN naming both dirs.
			e1 := filepath.Join(d.Extra[0], "se")
			sh := filepath.Join(d.Shared, "se")
			if len(logLines(out, "WARN", "name clash", e1, sh)) != 1 {
				t.Errorf("want one WARN name clash for se (%s over %s); log:\n%s", e1, sh, out)
			}
			// agent over extra2 (extra on the losing side) is a WARN too.
			if len(logLines(out, "WARN", "name clash", filepath.Join(d.Agent, "clash"), filepath.Join(d.Extra[1], "clash"))) != 1 {
				t.Errorf("want WARN for agent clash over extra2; log:\n%s", out)
			}
		})
	}
}

// TestLoadDirsSharedAgentClashUnchanged pins requirement 2's other half: with
// no extra dir involved, a shared-vs-agent clash keeps its INFO "override"
// line and the agent copy still wins.
func TestLoadDirsSharedAgentClashUnchanged(t *testing.T) {
	buf := captureSkillsLog(t)
	root := t.TempDir()
	d := Dirs{Shared: filepath.Join(root, "shared"), Agent: filepath.Join(root, "agent")}
	writeSkillMD(t, d.Shared, "x", "---\nname: x\ndescription: shared\n---\n")
	writeSkillMD(t, d.Agent, "x", "---\nname: x\ndescription: agent\n---\n")
	reg := LoadDirs(d)
	if got := skillDesc(reg, "x"); got != "agent" {
		t.Fatalf("winner = %q, want agent", got)
	}
	out := buf.String()
	if len(logLines(out, "INFO", "override: x")) != 1 {
		t.Errorf("want one INFO override line; log:\n%s", out)
	}
	if len(logLines(out, "name clash")) != 0 {
		t.Errorf("shared-vs-agent clash must not WARN; log:\n%s", out)
	}
}

func TestLoadDirsMissingExtraWarnsAndContinues(t *testing.T) {
	buf := captureSkillsLog(t)
	root := t.TempDir()
	missing := filepath.Join(root, "nope")
	d := Dirs{Shared: filepath.Join(root, "shared"), Extra: []string{missing}, Agent: filepath.Join(root, "agent")}
	writeSkillMD(t, d.Agent, "a", "---\nname: a\ndescription: a\n---\n")
	reg := LoadDirs(d)
	if !hasSkill(reg, "a") {
		t.Fatalf("a missing extra dir must not stop the load: %v", names(reg))
	}
	out := buf.String()
	if len(logLines(out, "WARN", "extra skills dir", missing)) != 1 {
		t.Errorf("want one WARN naming %s; log:\n%s", missing, out)
	}
	// The (also missing) shared dir stays quiet, as before.
	if len(logLines(out, "WARN", d.Shared)) != 0 {
		t.Errorf("a missing shared dir must not WARN; log:\n%s", out)
	}
}

// An extra dir that is itself a skill (SKILL.md at its root) loads as that one
// skill; its subfolders are not scanned. Reflection's snapshot tracks it too.
func TestExtraDirThatIsASkill(t *testing.T) {
	captureSkillsLog(t)
	root := t.TempDir()
	single := filepath.Join(root, "repo-skill")
	writeSkillMD(t, root, "repo-skill", "---\nname: repo-skill\ndescription: repo root is the skill\n---\n")
	// A subfolder that looks like a skill must NOT load from a single-skill dir.
	writeSkillMD(t, single, "nested", "---\nname: nested\ndescription: should not load\n---\n")
	d := Dirs{Shared: filepath.Join(root, "none-shared"), Extra: []string{single}, Agent: filepath.Join(root, "none-agent")}
	for name, reg := range map[string]*Registry{
		"LoadDirs":     LoadDirs(d),
		"LoadForAgent": NewLoader().LoadForAgent(d),
	} {
		if !hasSkill(reg, "repo-skill") {
			t.Errorf("%s: missing repo-skill; have %v", name, names(reg))
		}
		if hasSkill(reg, "nested") {
			t.Errorf("%s: loaded nested from inside a single-skill dir", name)
		}
	}
	if _, ok := Snapshot(d.List())[single]; !ok {
		t.Errorf("Snapshot does not track the single-skill extra dir %s", single)
	}
}
