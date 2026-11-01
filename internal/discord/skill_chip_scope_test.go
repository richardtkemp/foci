package discord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/agent"
	"foci/internal/skills"
	"foci/internal/turn"
)

// TestTrackerFullMode_SkillSubFileChip: Discord shares the root-aware chip
// rule through the bot's agent (#2392): a workspace-relative read of a
// sub-file inside a skill dir under one of the agent's roots lands as its own
// chip naming the skill and the file.
func TestTrackerFullMode_SkillSubFileChip(t *testing.T) {
	ws := t.TempDir()
	root := filepath.Join(ws, "skills")
	for path, body := range map[string]string{
		filepath.Join(root, "prediction", "SKILL.md"):  "---\nname: prediction\n---\n",
		filepath.Join(root, "prediction", "method.md"): "m\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, fs, _ := newTestBot(t, "a")
	b.agentRef = &agent.Agent{Workspace: ws, SkillDirs: skills.NewLiveDirs(skills.Dirs{Agent: root})}
	tracker := newToolCallTracker(b, "42", turn.TurnDisplay{ShowToolCalls: "full"})

	tracker.ObserveToolCall("t1", "read", json.RawMessage(`{"path":"./skills/prediction/method.md"}`))
	if got, want := fs.lastSend(t).content, "📖 Loaded skill file: prediction/method.md"; got != want {
		t.Errorf("sub-file chip = %q, want %q", got, want)
	}
}
