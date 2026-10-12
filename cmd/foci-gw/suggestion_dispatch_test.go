package main

import (
	"context"
	"testing"

	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/suggestion"
)

// fakeSuggestionConn is the minimal button-capable connection a suggestion press needs:
// it records one button send (per-button data carries the interactive id) and
// every edit applied to it. The rest of the Connection surface panics via the
// embedded nil interface, so the test only reaches what the suggestion path calls.
type fakeSuggestionConn struct {
	platform.Connection

	msgID string
	datas []string
	edits []string
}

func (c *fakeSuggestionConn) SendTextWithButtons(_ string, buttons []platform.ButtonChoice, _ string) (string, error) {
	for _, b := range buttons {
		c.datas = append(c.datas, b.Data)
	}
	return c.msgID, nil
}

func (c *fakeSuggestionConn) EditMessageText(_, text string) error {
	c.edits = append(c.edits, text)
	return nil
}

func (c *fakeSuggestionConn) EditMessageWithButtons(msgID, text string, _ []platform.ButtonChoice, _ string) error {
	return c.EditMessageText(msgID, text)
}

// TestSuggestionPressShowsPartsCommandReply proves the production press path end
// to end (review round 5, Finding 1): a suggestion button whose command answers
// in Response.Parts — the way /config does — edits the pressed message with
// the FULL reply (parts joined by a blank line, command.Response.DisplayText,
// the same rendering every platform command-reply path uses), not a
// flattened "Done". The press goes through the real interactive router and
// the real dispatch core (runAgentCommand), exactly as a live press does.
func TestSuggestionPressShowsPartsCommandReply(t *testing.T) {
	t.Parallel()
	multi := &command.Command{
		Name: "parts",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Parts: []string{"PART-ONE", "PART-TWO"}}, nil
		},
	}
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{multi}})
	inst := d.agents[testAgentID]

	reg := suggestion.NewRegistry()
	if err := reg.Register(suggestion.Spec{
		ID:   "parts.probe",
		Text: "Run it?",
		Actions: []suggestion.Action{
			{Label: "Yes", Command: "/parts"},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	conn := &fakeSuggestionConn{msgID: "suggestion-msg-1"}
	sender, err := suggestion.NewSender(suggestion.SenderDeps{
		Registry: reg,
		Index:    d.sessionIndex,
		ConnFor: func(_, _ string) platform.ConnResolver {
			return func() platform.Connection { return conn }
		},
		Dispatch: suggestionCommandDispatcher(inst.cmds, inst.cc, d.connMgr, inst.id),
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	chatKey := session.NewChatSessionKey(testAgentID, 42)
	res, err := sender.Send(context.Background(), testAgentID, chatKey, "parts.probe")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res != suggestion.ResultSent {
		t.Fatalf("Send = %q, want %q", res, suggestion.ResultSent)
	}

	// Press action 0 ("Yes") through the real router, applying the edit
	// exactly as the platform layers do (telegram bot_callbacks.go).
	edit, _, ok := platform.HandleInteractiveCallback(conn.datas[0])
	if !ok {
		t.Fatal("router did not know the pressed prompt")
	}
	if err := conn.EditMessageText(conn.msgID, edit); err != nil {
		t.Fatalf("edit after press: %v", err)
	}

	if len(conn.edits) != 1 {
		t.Fatalf("edits = %v, want exactly one", conn.edits)
	}
	if want := "PART-ONE\n\nPART-TWO"; conn.edits[0] != want {
		t.Errorf("press edit = %q, want the full parts reply %q", conn.edits[0], want)
	}
}
