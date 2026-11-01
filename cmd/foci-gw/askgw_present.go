package main

import (
	"foci/internal/askgw"
	"foci/internal/platform"
	"foci/internal/question"
)

// askgwPresentFn builds askgw's agent-chat PresentFn (askgwPresenter's chat
// fallback, #2707) over the connection manager: the same prompt body
// setupAskgw inlined before #2459 A1, with one addition — an
// ask presented on a connection whose platform cannot show approval prompts
// (platform.ApprovalRefuser, Slack until part C) fails closed: nothing is
// posted, the denial is logged, and the requesting conversation gets a short
// notice. PresentRefused makes the askgw server deny the ask at once (the
// requester receives the dismissal frame — no answer, nothing approved);
// PresentFailed resolves it unavailable. Neither leaves anything pending.
func askgwPresentFn(connMgr platform.ConnectionManager) askgw.PresentFn {
	return func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, askgw.PresentOutcome) {
		resolve := connResolver(connMgr, sessionKey, agentID)
		conn := resolve()
		if conn == nil {
			return "", askgw.PresentFailed
		}
		if refuseApproval(askgwLog, conn, sessionKey, msgID, summary) {
			return "", askgw.PresentRefused
		}
		platformMsgID, err := platform.SendInteractiveMessageWithID(resolve, msgID, summary, text, askgwButtons(choices), func(choice platform.ButtonChoice) string {
			onResponse(choice.Data)
			if choice.Data == question.CancelData {
				return "❌ Cancelled"
			}
			return "✅ " + choice.Label
		}, func() {
			onResponse(question.CancelData)
		})
		if err != nil {
			return "", askgw.PresentFailed
		}
		return platformMsgID, askgw.Presented
	}
}
