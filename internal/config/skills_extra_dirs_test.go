package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLoadSkillExtraDirs (#2657): [skills] extra_dirs and [agents.skills]
// extra_dirs decode as known keys, ~ expands to $HOME, relative paths resolve
// against the foci home dir (FOCI_HOME), absolute paths are kept, and the
// agent's list comes after the global one.
func TestLoadSkillExtraDirs(t *testing.T) {
	root := t.TempDir()
	userHome := t.TempDir()
	t.Setenv("FOCI_HOME", root)
	t.Setenv("HOME", userHome)
	path := filepath.Join(root, "foci.toml")
	body := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[skills]
extra_dirs = ["~/git/ai-sudo/skills", "vendored/skills", "/abs/skills"]

[[agents]]
id = "a"

[agents.skills]
extra_dirs = ["~", "mine"]
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, k := range cfg.UndefinedKeys {
		t.Errorf("unknown key %q: extra_dirs must decode", k)
	}
	wantGlobal := []string{filepath.Join(userHome, "git/ai-sudo/skills"), filepath.Join(root, "vendored/skills"), "/abs/skills"}
	if !reflect.DeepEqual(cfg.Skills.ExtraDirs, wantGlobal) {
		t.Errorf("[skills] extra_dirs = %v, want %v", cfg.Skills.ExtraDirs, wantGlobal)
	}
	wantAgent := []string{userHome, filepath.Join(root, "mine")}
	if !reflect.DeepEqual(cfg.Agents[0].Skills.ExtraDirs, wantAgent) {
		t.Errorf("[agents.skills] extra_dirs = %v, want %v", cfg.Agents[0].Skills.ExtraDirs, wantAgent)
	}
	if got, want := cfg.SkillExtraDirs(cfg.Agents[0]), append(append([]string{}, wantGlobal...), wantAgent...); !reflect.DeepEqual(got, want) {
		t.Errorf("SkillExtraDirs = %v, want %v (global first, then agent)", got, want)
	}
}

// TestSkillExtraDirsRegistryRowsHot pins that both keys are registry rows
// (settable via /config) and hot, i.e. not flagged restart-required.
func TestSkillExtraDirsRegistryRowsHot(t *testing.T) {
	want := map[string]bool{"skills.extra_dirs": false, "agent.skills.extra_dirs": false}
	for _, f := range AllFields() {
		addr := f.Section + "." + f.Key
		if _, ok := want[addr]; ok {
			if f.NeedsRestart {
				t.Errorf("%s is marked restart-required", addr)
			}
			if f.Type != FieldStringList {
				t.Errorf("%s type = %v, want FieldStringList", addr, f.Type)
			}
			want[addr] = true
		}
	}
	for addr, seen := range want {
		if !seen {
			t.Errorf("no registry row for %s", addr)
		}
	}
}
