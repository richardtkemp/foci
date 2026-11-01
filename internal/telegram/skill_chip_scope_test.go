package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/skills"
	"foci/internal/turn"
)

// TestTrackerFullMode_SkillSubFileChip: Telegram shares the root-aware chip
// rule through the bot's agent (#2392): a read of a sub-file inside a skill
// dir under one of the agent's roots lands as its own chip naming the skill
// and the file, and the SKILL.md keeps today's chip.
func TestTrackerFullMode_SkillSubFileChip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
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
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.agentRef = &agent.Agent{SkillDirs: skills.NewLiveDirs(skills.Dirs{Shared: root})}
	tr := newToolCallTracker(b, 12345, turn.TurnDisplay{ShowToolCalls: "full"})

	tr.ObserveToolCall("t1", "Read", json.RawMessage(`{"file_path":"`+filepath.Join(root, "prediction", "method.md")+`"}`))
	if got, want := mock.lastSendInjected, "📖 Loaded skill file: prediction/method.md"; got != want {
		t.Errorf("sub-file chip = %q, want %q", got, want)
	}
	tr.ObserveToolCall("t2", "Read", json.RawMessage(`{"file_path":"`+filepath.Join(root, "prediction", "SKILL.md")+`"}`))
	if got, want := mock.lastSendInjected, "📖 Loaded skill: prediction"; got != want {
		t.Errorf("SKILL.md chip = %q, want %q", got, want)
	}
}
