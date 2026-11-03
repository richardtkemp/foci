package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/timeutil"
)

// TestConfigureDelegated_DailyDigest drives the real configureDelegated wiring
// (#2827): with memory.daily_digest on, a root chat session's rebuilt prompt
// ENDS with the headings of today's memory file; a branch (spawn, fork, batch)
// and an independent session get none; and turning the flag off in the live
// config removes it from the next session start without a rebuild of the
// agent (hot:"session").
func TestConfigureDelegated_DailyDigest(t *testing.T) {
	t.Parallel()
	p, shared := wiringFixture(t)
	p.resolved.MemorySearch.DailyDigest = true

	today := timeutil.Now().Format("2006-01-02")
	memDir := filepath.Join(p.acfg.Workspace, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const heading = "## DIGEST-HEADING-MARKER (09:00)"
	if err := os.WriteFile(filepath.Join(memDir, today+".md"), []byte("# "+today+"\nbody-not-in-digest\n"+heading+"\nmore body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{}
	if _, ok := configureDelegated(ag, p, shared, "claude-code", config.BackendConfig{}); !ok {
		t.Fatal("configureDelegated returned ok=false")
	}
	fn := ag.DelegatedManager.StartOpts.SystemPromptFunc

	chat := fn("wiretester/c1")
	if !strings.Contains(chat, heading) {
		t.Fatalf("chat session prompt lacks the digest heading:\n%s", chat)
	}
	if strings.Contains(chat, "body-not-in-digest") {
		t.Errorf("digest carries body text, not headings only")
	}
	if tail := chat[strings.LastIndex(chat, "## Recent Memory"):]; !strings.Contains(tail, heading) || strings.Contains(tail, "MARKER-") {
		t.Errorf("digest is not the last section of the prompt:\n%s", tail)
	}

	for _, key := range []string{"wiretester/c1/b1793729194", "wiretester/iwork"} {
		if got := fn(key); strings.Contains(got, heading) {
			t.Errorf("%s got the digest; only root chat sessions should", key)
		}
	}

	off := *p.resolvedLive.Load()
	off.MemorySearch.DailyDigest = false
	p.resolvedLive.Store(&off)
	if got := fn("wiretester/c1"); strings.Contains(got, heading) {
		t.Errorf("digest still present after daily_digest turned off live")
	}
}
