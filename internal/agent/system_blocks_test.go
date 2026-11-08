package agent

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/provider"
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

// TestSystemBlocksFor_EqualsMainSession (#2981): the prompt an API character
// spawn runs on is the main-session composition for the same key —
// environment, character files, skills and digest — and reading it leaves no
// session state behind for the key.
func TestSystemBlocksFor_EqualsMainSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("character A"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		Bootstrap:            workspace.NewBootstrap(dir, []string{"a.md"}),
		EnvironmentBlockFunc: func(k string) string { return "ENV " + k },
		ExtraSystemBlocks:    []provider.SystemBlock{{Type: "text", Text: "SKILLS"}},
		DailyDigestFunc:      func(k string) string { return "DIGEST " + k },
	}
	const spawnKey, mainKey = "test/c1/b42", "test/c1"
	got := a.SystemBlocksFor(spawnKey)
	a.metaMu.Lock()
	_, made := a.meta[spawnKey]
	a.metaMu.Unlock()
	if made {
		t.Error("SystemBlocksFor created session state for the spawn key")
	}
	want := a.buildSystemBlocks(spawnKey)
	if len(got) != len(want) {
		t.Fatalf("SystemBlocksFor = %d blocks, main-session build = %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Text != want[i].Text {
			t.Errorf("block %d = %q, main-session build %q", i, got[i].Text, want[i].Text)
		}
	}
	texts := []string{"ENV " + spawnKey, "character A", "SKILLS", "DIGEST " + spawnKey}
	if len(got) != len(texts) {
		t.Fatalf("blocks = %+v, want %v", got, texts)
	}
	for i, w := range texts {
		if got[i].Text != w {
			t.Errorf("block %d = %q, want %q", i, got[i].Text, w)
		}
	}
	// The main session itself is unchanged by the refactor.
	if m := a.buildSystemBlocks(mainKey); len(m) != 4 || m[0].Text != "ENV "+mainKey {
		t.Errorf("main session blocks = %+v", m)
	}
}
