package ccstream

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"foci/internal/ccaccount"
	"foci/internal/delegator"
	"foci/internal/procx"
)

// isolateCCEnv gives a test a single-home install (HOME = a temp dir) with no
// CC variable in this process and no operator overlay; it returns HOME.
func isolateCCEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FOCI_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	_ = os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	prev := procx.OperatorOverlay()
	procx.SetOperatorEnv(nil)
	t.Cleanup(func() {
		m := map[string]string{}
		for _, kv := range prev {
			if k, v, ok := strings.Cut(kv, "="); ok {
				m[k] = v
			}
		}
		procx.SetOperatorEnv(m)
	})
	return home
}

// TestCCStateFollowsAgentConfigDir (#3104 H5, H6): every reader of one
// agent's CC on-disk state resolves under that agent's CLAUDE_CONFIG_DIR from
// its backend_config.env (cfg["env"]), and an agent without one under the
// host folder: the session file path, the transcript the delivery checks and
// the resume baseline read, the subagent files, fork source and target,
// cleanup, and the retention setting. Two agents run side by side and each
// sees only its own folder.
func TestCCStateFollowsAgentConfigDir(t *testing.T) {
	home := isolateCCEnv(t)
	own := filepath.Join(t.TempDir(), "x")
	host := filepath.Join(home, ".claude")
	const workDir = "/work/beta"

	agents := []struct {
		name  string
		env   map[string]string
		dir   string
		other string
	}{
		{"own folder", map[string]string{"CLAUDE_CONFIG_DIR": own}, own, host},
		{"no override", nil, host, own},
	}
	for _, a := range agents {
		b := &Backend{cfg: map[string]any{"env": a.env}, sessionID: "sess-1", workDir: workDir}
		slugDir := filepath.Join(a.dir, "projects", projectSlug(workDir))
		otherSlug := filepath.Join(a.other, "projects", projectSlug(workDir))

		if got, want := b.SessionFilePath(), filepath.Join(slugDir, "sess-1.jsonl"); got != want {
			t.Errorf("%s: SessionFilePath = %q, want %q", a.name, got, want)
		}
		if got, err := b.transcriptPath(workDir, "sess-1"); err != nil || got != filepath.Join(slugDir, "sess-1.jsonl") {
			t.Errorf("%s: transcriptPath = %q, %v", a.name, got, err)
		}
		if got, want := b.subagentFilePath("a1", ".meta.json"), filepath.Join(slugDir, "sess-1", "subagents", "agent-a1.meta.json"); got != want {
			t.Errorf("%s: subagentFilePath = %q, want %q", a.name, got, want)
		}

		// The transcript checker (Spec.TranscriptChecker) reads this agent's
		// transcript: found here, unreadable in the other folder.
		writeTranscriptLines(t, filepath.Join(slugDir, "sess-1.jsonl"), `{"type":"user","uuid":"in-1"}`)
		if found, err := InputInTranscript(a.env, workDir, "sess-1", "in-1"); err != nil || !found {
			t.Errorf("%s: InputInTranscript = %v, %v; want found in the agent's own folder", a.name, found, err)
		}

		// Fork reads its source and writes its target in this folder; cleanup
		// deletes there.
		writeTranscriptLines(t, filepath.Join(slugDir, "parent.jsonl"), `{"sessionId":"parent","type":"user"}`)
		res, err := b.ForkSession(context.Background(), delegator.ForkRequest{ParentSessionID: "parent", WorkDir: workDir})
		if err != nil {
			t.Fatalf("%s: ForkSession: %v", a.name, err)
		}
		forked := filepath.Join(slugDir, res.SessionID+".jsonl")
		if _, err := os.Stat(forked); err != nil {
			t.Errorf("%s: fork not written in the agent's folder: %v", a.name, err)
		}
		if _, err := os.Stat(filepath.Join(otherSlug, res.SessionID+".jsonl")); !os.IsNotExist(err) {
			t.Errorf("%s: fork written in the other folder (stat err %v)", a.name, err)
		}
		if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{SessionID: res.SessionID, WorkDir: workDir}); err != nil {
			t.Fatalf("%s: CleanupSession: %v", a.name, err)
		}
		if _, err := os.Stat(forked); !os.IsNotExist(err) {
			t.Errorf("%s: CleanupSession left %s (stat err %v)", a.name, forked, err)
		}
	}

	// The other agent's transcript is not readable through this agent.
	if _, err := os.Stat(filepath.Join(host, "projects", projectSlug(workDir), "sess-1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(own, "projects", projectSlug(workDir), "sess-1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := InputInTranscript(map[string]string{"CLAUDE_CONFIG_DIR": own}, workDir, "sess-1", "in-1"); err == nil {
		t.Errorf("the own-folder agent read the host folder's transcript")
	}

	// Retention: each agent's settings.json.
	if err := os.WriteFile(filepath.Join(own, "settings.json"), []byte(`{"cleanupPeriodDays": 7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "settings.json"), []byte(`{"cleanupPeriodDays": 30}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CleanupPeriod(map[string]string{"CLAUDE_CONFIG_DIR": own}); got != 7*24*time.Hour {
		t.Errorf("own-folder agent's retention = %v, want 7 days from its settings.json", got)
	}
	if got := CleanupPeriod(nil); got != 30*24*time.Hour {
		t.Errorf("host agent's retention = %v, want 30 days", got)
	}
}

// TestBuildEnvFolderMatchesCCAccount guards the one definition: the folder
// ccaccount.ForAgent resolves for an agent is the folder named by the
// environment buildEnv actually hands CC, with and without an agent override
// and with the operator's shell overlay setting CLAUDE_CONFIG_DIR (F14).
func TestBuildEnvFolderMatchesCCAccount(t *testing.T) {
	isolateCCEnv(t)
	for _, overlay := range []map[string]string{nil, {"CLAUDE_CONFIG_DIR": "/o"}} {
		procx.SetOperatorEnv(overlay)
		for _, extra := range []map[string]string{nil, {"CLAUDE_CONFIG_DIR": "/x", "CLAUDE_SECURESTORAGE_CONFIG_DIR": "/s"}} {
			want, err := ccaccount.ForAgent(extra)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ccaccount.FromEnv(buildEnv(extra, nil), runtime.GOOS)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("overlay %v, agent env %v: buildEnv gives %+v, ForAgent %+v", overlay, extra, got, want)
			}
		}
	}
}

func writeTranscriptLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
