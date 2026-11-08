package agent

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/delegator"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/workspace"
)

// TestBuildSystemBlocks_SkillsLast reproduces #1421: skills (ExtraSystemBlocks)
// must be the LAST block in the assembled system prompt regardless of
// CacheStrategy, because skills can be re-seeded/updated at any time — if
// something else trails them, a skill update busts the cache for everything
// after it too. This must hold for every CacheStrategy value the translate
// layer supports ("auto" and "explicit"), since both mark whatever ends up
// literally last as the cache breakpoint.
func TestBuildSystemBlocks_SkillsLast(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{"auto", "explicit"} {
		t.Run(strategy, func(t *testing.T) {
			dir := t.TempDir()
			fileOrder := []string{"a.md", "b.md"}
			if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("character A"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("character B"), 0o644); err != nil {
				t.Fatal(err)
			}
			bs := workspace.NewBootstrap(dir, fileOrder)
			// A configured secret makes Bootstrap.SystemBlocks() append an
			// extra trailing block (the secrets note) after the character
			// files — the realistic shape of the "last bootstrap block" that
			// skills must still end up after.
			bs.SetSecretNames([]string{"FOO"}, false)

			a := &Agent{
				Bootstrap:     bs,
				CacheStrategy: strategy,
				ExtraSystemBlocks: []provider.SystemBlock{
					{Type: "text", Text: "SKILL CONTENT"},
				},
			}

			result := a.buildSystemBlocks("test/c1")
			if len(result) == 0 {
				t.Fatal("buildSystemBlocks returned no blocks")
			}
			last := result[len(result)-1]
			if last.Text != "SKILL CONTENT" {
				t.Errorf("strategy=%s: last system block = %q, want skills block %q (skills must be last so a skill update doesn't bust the cache for trailing content)",
					strategy, last.Text, "SKILL CONTENT")
			}
		})
	}
}

// TestBuildSystemBlocks_DailyDigestLastAndFrozen (#2827): the per-session
// daily-memory digest is appended after the skills (it is the only
// per-session, daily-changing block, so nothing shared may trail it) and is a
// snapshot: a second build in the same session returns the first text even
// when the digest source has moved on.
func TestBuildSystemBlocks_DailyDigestLastAndFrozen(t *testing.T) {
	t.Parallel()
	digest := "## Recent Memory (headings only)\nv1"
	a := &Agent{
		Bootstrap:         workspace.NewBootstrap(t.TempDir(), nil),
		ExtraSystemBlocks: []provider.SystemBlock{{Type: "text", Text: "SKILL CONTENT"}},
		DailyDigestFunc:   func(string) string { return digest },
	}
	first := a.buildSystemBlocks("test/c1")
	if n := len(first); n < 2 || first[n-1].Text != digest || first[n-2].Text != "SKILL CONTENT" {
		t.Fatalf("want [..., skills, digest], got %+v", first)
	}
	digest = "## Recent Memory (headings only)\nv2"
	again := a.buildSystemBlocks("test/c1")
	if got := again[len(again)-1].Text; got != "## Recent Memory (headings only)\nv1" {
		t.Errorf("digest changed mid-session: %q", got)
	}
	a.DailyDigestFunc = func(string) string { return "" }
	if other := a.buildSystemBlocks("test/c2"); other[len(other)-1].Text != "SKILL CONTENT" {
		t.Errorf("empty digest must add no block, got last %q", other[len(other)-1].Text)
	}
}

// TestSpawnSystemBlocksFor_MainSessionWithSpawnEnv (#2981, #2986): the
// prompt an API character spawn runs on is the main-session composition for
// the same key (environment, character files, skills, digest), with the
// environment block built for a spawn; reading it leaves no session state
// behind for the key.
func TestSpawnSystemBlocksFor_MainSessionWithSpawnEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("character A"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		Bootstrap: workspace.NewBootstrap(dir, []string{"a.md"}),
		EnvironmentBlockFunc: func(k string, spawn bool) string {
			if spawn {
				return "SPAWN ENV " + k
			}
			return "ENV " + k
		},
		ExtraSystemBlocks: []provider.SystemBlock{{Type: "text", Text: "SKILLS"}},
		DailyDigestFunc:   func(k string) string { return "DIGEST " + k },
	}
	const spawnKey, mainKey = "test/c1/b42", "test/c1"
	got := a.SpawnSystemBlocksFor(spawnKey)
	a.metaMu.Lock()
	_, made := a.meta[spawnKey]
	a.metaMu.Unlock()
	if made {
		t.Error("SpawnSystemBlocksFor created session state for the spawn key")
	}
	texts := []string{"SPAWN ENV " + spawnKey, "character A", "SKILLS", "DIGEST " + spawnKey}
	if len(got) != len(texts) {
		t.Fatalf("blocks = %+v, want %v", got, texts)
	}
	for i, w := range texts {
		if got[i].Text != w {
			t.Errorf("block %d = %q, want %q", i, got[i].Text, w)
		}
	}
	// A main session gets the main-session environment block.
	if m := a.buildSystemBlocks(mainKey); len(m) != 4 || m[0].Text != "ENV "+mainKey {
		t.Errorf("main session blocks = %+v", m)
	}
}

// TestBuildSystemBlocks_SpawnBranchGetsSpawnEnv (#2986): an API agent's
// background character spawn runs on a branch of type "spawn" through the
// normal session path; its environment block is built for a spawn. Other
// branches (a fork) and the root chat are not spawns.
func TestBuildSystemBlocks_SpawnBranchGetsSpawnEnv(t *testing.T) {
	t.Parallel()
	store := session.NewStore(t.TempDir())
	a := &Agent{
		Sessions:  store,
		Bootstrap: workspace.NewBootstrap(t.TempDir(), nil),
		EnvironmentBlockFunc: func(_ string, spawn bool) string {
			if spawn {
				return "SPAWN ENV"
			}
			return "MAIN ENV"
		},
	}
	const root = "test/c1"
	spawnKey, err := store.CreateBranchWithOptions(root, session.BranchOptions{NoResetHook: true, BranchType: "spawn", Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	forkKey, err := store.CreateBranchWithOptions(root, session.BranchOptions{BranchType: "fork", Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{spawnKey: "SPAWN ENV", forkKey: "MAIN ENV", root: "MAIN ENV"} {
		got := a.buildSystemBlocks(key)
		if len(got) == 0 || got[0].Text != want {
			t.Errorf("buildSystemBlocks(%s) env = %+v, want %q", key, got, want)
		}
	}
}

// TestIsSpawnSession (#2986): a delegated agent's spawn sessions (a direct
// spawn's batch, a background character spawn's branch) are spawns; foci's
// own batches and unknown sessions are not.
func TestIsSpawnSession(t *testing.T) {
	t.Parallel()
	m := &DelegatedManager{}
	m.setBatchSpec("a/c1/b1", batchSpec{agentPrompt: true, spawn: isSpawnPurpose(delegator.BatchPurposeSpawnCharacter)})
	m.setBatchSpec("a/c1/b2", batchSpec{systemPrompt: "x", spawn: isSpawnPurpose(delegator.BatchPurposeConsolidation)})
	m.SetCharacterSpawnBranch("a/c1/b3", "")
	for key, want := range map[string]bool{"a/c1/b1": true, "a/c1/b2": false, "a/c1/b3": true, "a/c1": false} {
		if got := m.IsSpawnSession(key); got != want {
			t.Errorf("IsSpawnSession(%s) = %v, want %v", key, got, want)
		}
	}
	var nilM *DelegatedManager
	if nilM.IsSpawnSession("a/c1/b1") {
		t.Error("nil manager reports a spawn session")
	}
}
