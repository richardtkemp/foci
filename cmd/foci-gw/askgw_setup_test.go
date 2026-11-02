package main

import (
	"testing"

	"foci/internal/askgw"
	"foci/internal/platform"
	"foci/internal/question"
)

// askgwPresenter (#2707 review): a question the app shows app-wide never also
// goes to the agent chat; one the app refuses (only old-build devices, or none)
// goes to the agent chat, so a sudo approval is never invisible.
func TestAskgwPresenter_AppFirstThenAgentChat(t *testing.T) {
	choices := []question.Choice{{Label: "Approve", Data: "opt:0"}, {Label: "Cancel", Data: question.CancelData}}

	for _, tc := range []struct {
		name      string
		appShows  bool
		wantChat  bool
		wantMsgID string
	}{
		{name: "capable device: dialog only", appShows: true, wantChat: false, wantMsgID: ""},
		{name: "only old-build devices: agent chat", appShows: false, wantChat: true, wantMsgID: "chat-msg-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var appCalls, chatCalls int
			var appButtons []platform.ButtonChoice
			var appHost string
			app := func(id, header, text, host string, buttons []platform.ButtonChoice, onChoice func(string)) bool {
				appCalls++
				appButtons = buttons
				appHost = host
				return tc.appShows
			}
			chat := func(agentID, sessionKey, msgID, text, summary, origin string, cs []question.Choice, onResponse func(string)) (string, askgw.PresentOutcome) {
				chatCalls++
				if agentID != "clutch" || sessionKey != "sk" || msgID != "askgw-a1-q0" {
					t.Errorf("chat got agent=%q session=%q msg=%q", agentID, sessionKey, msgID)
				}
				return "chat-msg-1", askgw.Presented
			}
			msgID, outcome := askgwPresenter(app, chat)("clutch", "sk", "askgw-a1-q0", "Run ls?", "Sudo", "richards-macbook-pro", choices, func(string) {})
			if outcome != askgw.Presented {
				t.Fatal("present failed")
			}
			if appCalls != 1 {
				t.Errorf("app tried %d times, want 1", appCalls)
			}
			if (chatCalls == 1) != tc.wantChat || chatCalls > 1 {
				t.Errorf("chat calls = %d, want chat=%v", chatCalls, tc.wantChat)
			}
			if msgID != tc.wantMsgID {
				t.Errorf("platform msgID = %q, want %q", msgID, tc.wantMsgID)
			}
			if appHost != "richards-macbook-pro" {
				t.Errorf("app host = %q, want the ask's origin (#2800)", appHost)
			}
			if len(appButtons) != 2 || appButtons[1].Data != question.CancelData {
				t.Errorf("app buttons = %+v", appButtons)
			}
		})
	}
}
