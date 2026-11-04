package discord

import (
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/platform"
)

// TestHandoffToAgent_RefusalTellsTheChat pins #2762 (audit A5): a message the
// agent inbox refuses is reported in its own channel. With no worker running,
// the inbox fills and then refuses.
func TestHandoffToAgent_RefusalTellsTheChat(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.agentRef = &agent.Agent{AgentID: "a"}
	qm := platform.QueuedMessage{UserID: "u1", Text: "hello", ChatID: 42}

	for i := 0; i < 256 && len(fs.sends) == 0; i++ {
		b.handoffToAgent(qm)
	}

	if len(fs.sends) != 1 {
		t.Fatalf("sends = %d, want one refusal notice", len(fs.sends))
	}
	got := fs.lastSend(t)
	if got.channelID != "42" {
		t.Errorf("notice went to channel %q, want 42", got.channelID)
	}
	if !strings.Contains(got.content, "not delivered") {
		t.Errorf("notice = %q", got.content)
	}
}
