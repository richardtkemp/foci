package main

import (
	"strings"
	"testing"
	"time"

	"foci/internal/tools"
)

// TestPrependSpawnOrigin: a permission prompt from a spawn's branch session
// says which spawn asks, by its id and its name, not its prompt (#2721); any
// other session's prompt is unchanged (#2523). A spawn with no name (persisted
// before names) shows the placeholder.
func TestPrependSpawnOrigin(t *testing.T) {
	t.Parallel()
	tr := tools.NewSpawnTracker()
	tr.Add("a/imain", tools.RunningSpawn{
		ID: "spawn-ab12", BranchKey: "a/imain/b1700000000", Name: "deploy\n watcher",
		Description: "wait for the deploy", Started: time.Now(),
	}, func() {})
	tr.Add("a/imain", tools.RunningSpawn{
		ID: "spawn-old", BranchKey: "a/imain/b1600000000",
		Description: "an old task", Started: time.Now(),
	}, func() {})

	got := prependSpawnOrigin(tr, "a/imain/b1700000000", "**Permission Required**\nBash: rm x")
	if !strings.HasPrefix(got, "🐣 **From spawn spawn-ab12:** deploy watcher\n\n**Permission Required**") || strings.Contains(got, "wait for the deploy") {
		t.Errorf("spawn prompt = %q, want it to lead with the spawn's id and name, not its prompt", got)
	}
	if got := prependSpawnOrigin(tr, "a/imain/b1600000000", "body"); !strings.HasPrefix(got, "🐣 **From spawn spawn-old:** "+tools.UnnamedSpawnLabel+"\n\n") {
		t.Errorf("unnamed spawn prompt = %q, want the placeholder", got)
	}
	for _, sk := range []string{"a/imain", "a/imain/b1800000000", ""} {
		if got := prependSpawnOrigin(tr, sk, "body"); got != "body" {
			t.Errorf("prompt from %q = %q, want it unchanged", sk, got)
		}
	}
}
