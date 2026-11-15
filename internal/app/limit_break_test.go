package app

import (
	"encoding/json"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/fap"
)

// sendCommandFrame puts a command frame (what the app sends for any typed
// "/..."; #3128) through the real inbound decoder.
func (s *shellHarness) sendCommandFrame(t *testing.T, id string, seq int, name, args string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"t": fap.TypeCommand, "id": id, "seq": seq, "v": fap.ProtocolVersion,
		"d": map[string]string{"conversationId": "conv-1", "agentId": "ag", "name": name, "args": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.h.dispatchInbound(s.client, data)
}

// TestLimitBreak_AppCommandFrameRunsAsMessage (#3128): the app sends a typed
// "/lb hey" as a command frame; on an agent with /limit_break it reaches the
// agent as a user message whose text is only "hey" (the agent stripped the
// word). A bare /lb starts no turn. On an agent without the command, "/lb hey"
// starts no turn either (an unknown command).
func TestLimitBreak_AppCommandFrameRunsAsMessage(t *testing.T) {
	s := newShellHarness(t, 1000, time.Minute)
	reg := command.NewRegistry()
	reg.Register(command.LimitBreakCommand())
	s.h.agents["ag"].commands = reg

	s.sendCommandFrame(t, "c-1", 1, "lb", "")
	s.sendCommandFrame(t, "c-2", 2, "lb", "hey one last thing")
	env := s.waitEnvelope(t)
	if env.Text != "hey one last thing" {
		t.Errorf("agent got %q, want %q", env.Text, "hey one last thing")
	}
	if n := len(s.envelopes()); n != 1 {
		t.Errorf("%d envelopes, want 1 (a bare /lb runs no turn)", n)
	}

	s.h.agents["ag"].commands = command.NewRegistry()
	s.sendCommandFrame(t, "c-3", 3, "lb", "hey")
	// A message reaches Enqueue inside dispatchInbound (routeUserTurn is
	// synchronous), so a wrong envelope would be visible now.
	if n := len(s.envelopes()); n != 1 {
		t.Errorf("without /limit_break, /lb hey reached the agent (%d envelopes)", n)
	}
}
