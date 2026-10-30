package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/secrets"
)

// writeGWSkill writes <dir>/<name>/SKILL.md with a minimal frontmatter.
func writeGWSkill(t *testing.T, dir, name string) {
	t.Helper()
	sd := filepath.Join(dir, name)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + name + " skill\n---\n"
	if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSkillExtraDirsStartupAndLiveApply covers #2657 end to end inside the
// gateway: startup loads global + per-agent extra dirs alongside the
// defaults, and a live edit of either extra_dirs key reaches the reload the
// next session start runs — without a restart, and without dragging in a
// pending edit of the restart-only [skills] dir.
func TestSkillExtraDirsStartupAndLiveApply(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(home, "a")
	shared := filepath.Join(home, "shared", "skills")
	globalExtra := filepath.Join(home, "g-extra")
	agentExtra := filepath.Join(home, "a-extra")
	newExtra := filepath.Join(home, "new-extra")
	writeGWSkill(t, shared, "s-skill")
	writeGWSkill(t, filepath.Join(ws, "skills"), "w-skill")
	writeGWSkill(t, globalExtra, "g-skill")
	writeGWSkill(t, agentExtra, "a-skill")
	writeGWSkill(t, newExtra, "n-skill")

	cfg := &config.Config{Skills: config.SkillsConfig{ExtraDirs: []string{globalExtra}}}
	acfg := config.AgentConfig{ID: "a", Workspace: ws, Skills: config.AgentSkillsConfig{ExtraDirs: []string{agentExtra}}}
	cfg.Agents = []config.AgentConfig{acfg}
	store, err := secrets.Load(filepath.Join(home, "no-secrets.toml"))
	if err != nil {
		t.Fatal(err)
	}
	br := setupBootstrapAndSkills(setupParams{acfg: acfg, cfg: cfg, resolved: config.Resolve(cfg, acfg)}, store)

	got := map[string]bool{}
	for _, s := range br.skillRegistry.All() {
		got[s.Name] = true
	}
	for _, want := range []string{"s-skill", "w-skill", "g-skill", "a-skill"} {
		if !got[want] {
			t.Errorf("startup registry lacks %s; have %v", want, got)
		}
	}

	reload := skillsReloadFn(br.skillsDirs, ws)
	block := func() string {
		blocks, _ := reload()
		var b strings.Builder
		for _, sb := range blocks {
			b.WriteString(sb.Text)
		}
		return b.String()
	}
	if b := block(); !strings.Contains(b, "g-skill") || strings.Contains(b, "n-skill") {
		t.Fatalf("pre-edit reload block wrong:\n%s", b)
	}

	la := newLiveApply("")
	registerLiveAppliers(la, map[string]*agentInstance{"a": {id: "a", skillsDirs: br.skillsDirs}})

	// Global edit: g-extra → new-extra; the [skills] dir is also changed in
	// the file, but it is restart-only and must not apply.
	fresh := &config.Config{Skills: config.SkillsConfig{Dir: filepath.Join(home, "elsewhere"), ExtraDirs: []string{newExtra}}}
	fresh.Agents = []config.AgentConfig{acfg}
	for _, addr := range liveApplySkillsAddrs {
		if la.appliers[addr] == nil {
			t.Fatalf("no applier registered for %s", addr)
		}
	}
	if err := la.appliers["skills.extra_dirs"](fresh); err != nil {
		t.Fatal(err)
	}
	b := block()
	for _, want := range []string{"n-skill", "a-skill", "s-skill", "w-skill"} {
		if !strings.Contains(b, want) {
			t.Errorf("after global edit, reload block lacks %s:\n%s", want, b)
		}
	}
	if strings.Contains(b, "g-skill") {
		t.Errorf("after global edit, removed dir's g-skill still listed:\n%s", b)
	}
	if d := br.skillsDirs.Get(); d.Shared != shared {
		t.Errorf("restart-only [skills] dir applied live: Shared = %q, want %q", d.Shared, shared)
	}

	// Per-agent edit: drop the agent's extra dir.
	fresh2 := &config.Config{Skills: config.SkillsConfig{ExtraDirs: []string{newExtra}}}
	acfg2 := acfg
	acfg2.Skills.ExtraDirs = nil
	fresh2.Agents = []config.AgentConfig{acfg2}
	if err := la.appliers["agent.skills.extra_dirs"](fresh2); err != nil {
		t.Fatal(err)
	}
	if b := block(); strings.Contains(b, "a-skill") || !strings.Contains(b, "n-skill") {
		t.Errorf("after agent edit, want n-skill and no a-skill:\n%s", b)
	}
}
