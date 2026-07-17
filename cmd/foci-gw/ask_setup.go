package main

import (
	"time"

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
	return func(sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) string {
		buttons := make([]platform.ButtonChoice, len(choices))
		for i, c := range choices {
			buttons[i] = platform.ButtonChoice{Label: c.Label, Data: c.Data}
		}
		platformMsgID, err := platform.SendInteractiveMessageWithID(connResolver(connMgr, sessionKey, agentID), msgID, summary, text, buttons, func(choice platform.ButtonChoice) string {
			onResponse(choice.Data)
			if choice.Data == question.CancelData {
				return "❌ Cancelled"
			}
			if summary != "" {
				return "✅ " + summary
			}
			return "✅ " + choice.Label
		}, func() {
			// Expiry: resolve the question as cancelled so the asking session
			// isn't left waiting on an answer that will never come.
			onResponse(question.CancelData)
		})
		if err != nil {
			askLog.Warnf("present question for session=%s failed: %v", sessionKey, err)
			return ""
		}
		return platformMsgID
	}
}

// newAskPresentBatchFn builds the BATCHED presenter for the foci-native `ask`
// tool: when the answering connection is the native app (a platform.BatchButtonSender)
// AND that client advertised the "interactiveBatch" capability, it posts EVERY
// question as one on-screen form and invokes onResponse once with all answers.
// It returns false — telling the ask layer to fall back to the sequential
// presenter — for any other transport (Telegram/Discord don't implement
// BatchButtonSender) or when the app client is offline/uncapable. So chat
// platforms and legacy app clients keep the one-question-at-a-time flow unchanged.
func newAskPresentBatchFn(agentID string, connMgr platform.ConnectionManager) tools.AskPresentBatchFn {
	return func(sessionKey, promptID string, qs []question.Question, onResponse func(answers []string)) bool {
		conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
		bs, ok := conn.(platform.BatchButtonSender)
		if !ok {
			return false // not the app (or no addressable conn) — sequential
		}
		batched, err := bs.SendInteractiveBatch(promptID, batchQuestionsFor(qs), onResponse)
		if err != nil {
			askLog.Warnf("present batched questions for session=%s failed: %v", sessionKey, err)
			return false
		}
		return batched
	}
}

// batchQuestionsFor maps the ask layer's questions onto the app's batched-prompt
// payload. The app renders its own layout, so each question carries STRUCTURED
// fields — raw text, optional header, and per-option label+description — rather
// than pre-rendered markdown. Option buttons get "qa:<index>" data and NO Cancel
// button: the app's full-screen ask form supplies its own Cancel (chat platforms,
// which DO need a per-question Cancel, take a separate sequential path via
// question.Choices). An option-less question yields empty Choices (typed-only).
func batchQuestionsFor(qs []question.Question) []platform.BatchQuestion {
	questions := make([]platform.BatchQuestion, len(qs))
	for i := range qs {
		q := &qs[i]
		buttons := make([]platform.ButtonChoice, len(q.Options))
		for j, opt := range q.Options {
			buttons[j] = platform.ButtonChoice{
				Label:       opt.Label,
				Data:        question.OptionData(j),
				Description: opt.Description,
			}
		}
		questions[i] = platform.BatchQuestion{
			Text:    q.Question,
			Header:  q.Header,
			Choices: buttons,
		}
	}
	return questions
}

// newAskRestoreFn builds the restore hook for the foci-native `ask` tool. After a
// restart the question's buttons still live on the platform (the message survived
// in the chat), but foci's in-memory routing entry was lost. This re-registers
// that routing entry against the existing buttons — without sending a new message
// — so a click on the already-displayed buttons resolves the pending ask. The
// platform-side message id is unknown here (we didn't re-send), so proactive
// edits (cancel/expiry) can't touch the message; click-driven routing and the
// "✅ <label>" edit work regardless, since those use the callback's own message.
func newAskRestoreFn(agentID string, connMgr platform.ConnectionManager) tools.AskRestoreFn {
	return func(sessionKey, msgID, platformMsgID string, choices []question.Choice, onResponse func(data string)) {
		buttons := make([]platform.ButtonChoice, len(choices))
		for i, c := range choices {
			buttons[i] = platform.ButtonChoice{Label: c.Label, Data: c.Data}
		}
		// Store the resolver, not a connection grabbed now: at restore time the
		// platform connection usually isn't up yet (this runs early in startup),
		// so an eager lookup would capture nil. The resolver re-queries at edit
		// time, by when the connection is live. platformMsgID (persisted across
		// the restart) lets cancel/expiry edit the on-screen message too.
		platform.RestoreInteractiveCallback(msgID, platformMsgID, connResolver(connMgr, sessionKey, agentID), buttons, func(choice platform.ButtonChoice) string {
			onResponse(choice.Data)
			if choice.Data == question.CancelData {
				return "❌ Cancelled"
			}
			return "✅ " + choice.Label
		}, func() {
			onResponse(question.CancelData)
		}, time.Now())
	}
}
