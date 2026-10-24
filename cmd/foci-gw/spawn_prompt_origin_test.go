package main

import (
	"strings"
	"testing"
	"time"

	"foci/internal/tools"
)

// TestPrependSpawnOrigin: a permission prompt from a spawn's branch session
// says which spawn asks; any other session's prompt is unchanged (#2523).
func TestPrependSpawnOrigin(t *testing.T) {
	t.Parallel()
	tr := tools.NewSpawnTracker()
	tr.Add("a/imain", tools.RunningSpawn{
		ID: "spawn-ab12", BranchKey: "a/imain/b1700000000",
		Description: "wait for the deploy", Started: time.Now(),
	}, func() {})

	got := prependSpawnOrigin(tr, "a/imain/b1700000000", "**Permission Required**\nBash: rm x")
	if !strings.HasPrefix(got, "🐣 **From spawn spawn-ab12:** wait for the deploy\n\n**Permission Required**") {
		t.Errorf("spawn prompt = %q, want it to lead with the spawn's id and task", got)
	}
	for _, sk := range []string{"a/imain", "a/imain/b1800000000", ""} {
		if got := prependSpawnOrigin(tr, sk, "body"); got != "body" {
			t.Errorf("prompt from %q = %q, want it unchanged", sk, got)
		}
	}
}
