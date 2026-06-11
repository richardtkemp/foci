package main

import (
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/question"
	"foci/internal/tools"
)

// newAskPresentFn builds the presenter for the foci-native `ask` tool. It posts
// one question's options as interactive buttons to the session's chat (the same
// mechanism the permission/AskUserQuestion prompts use) and invokes onResponse
// with the chosen button data when the user clicks. Non-blocking: the tool's
// Execute returns immediately; this fires later on the platform callback.
func newAskPresentFn(agentID string, connMgr platform.ConnectionManager) tools.AskPresentFn {
	return func(sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) {
		conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
		if conn == nil {
			log.Warnf("ask", "no connection for session=%s; question %q dropped", sessionKey, msgID)
			return
		}
		buttons := make([]platform.ButtonChoice, len(choices))
		for i, c := range choices {
			buttons[i] = platform.ButtonChoice{Label: c.Label, Data: c.Data}
		}
		err := platform.SendInteractiveMessageWithID(conn, msgID, text, buttons, func(choice platform.ButtonChoice) string {
			onResponse(choice.Data)
			if choice.Data == question.CancelData {
				return "❌ Cancelled"
			}
			return "✅ " + choice.Label
		}, func() {
			// Expiry: resolve the question as cancelled so the asking session
			// isn't left waiting on an answer that will never come.
			onResponse(question.CancelData)
		})
		if err != nil {
			log.Warnf("ask", "present question for session=%s failed: %v", sessionKey, err)
		}
	}
}

// registerAskTool builds and registers the `ask`/`foci_ask` tool against the
// given registry. deliver injects the assembled answer batch back into the
// asking session as a normal inbound message (waking the agent). Returns the
// AskRouter used by the inbound path to divert typed ("Other") answers.
func registerAskTool(registry *tools.Registry, agentID string, connMgr platform.ConnectionManager, deliver tools.SessionNotifyFn) *tools.AskRouter {
	present := newAskPresentFn(agentID, connMgr)
	tool, router := tools.NewAskTool(present, tools.AskDeliverFn(deliver))
	registry.Register(tool)
	return router
}
