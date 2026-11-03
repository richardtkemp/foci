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
