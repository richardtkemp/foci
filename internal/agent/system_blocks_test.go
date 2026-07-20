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
