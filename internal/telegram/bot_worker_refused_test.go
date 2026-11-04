package telegram

import (
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/platform"
)

// TestHandoffToAgent_RefusalTellsTheChat pins #2762 (audit A5): a message the
// agent inbox refuses is reported in its own chat. With no worker running, the
// inbox fills and then refuses.
func TestHandoffToAgent_RefusalTellsTheChat(t *testing.T) {
	b, mock := testBot(nil, command.NewRegistry())
	b.agentRef = &agent.Agent{AgentID: "test"}
	qm := platform.QueuedMessage{UserID: "111", Text: "hello", ChatID: 12345}

	for i := 0; i < 256 && mock.sends == 0; i++ {
		b.handoffToAgent(qm)
	}

	if mock.sends != 1 {
		t.Fatalf("sends = %d, want one refusal notice", mock.sends)
	}
	if mock.lastSendChatID != 12345 {
		t.Errorf("notice went to chat %d, want 12345", mock.lastSendChatID)
	}
	if !strings.Contains(mock.lastSendInjected, "not delivered") {
		t.Errorf("notice = %q", mock.lastSendInjected)
	}
}
