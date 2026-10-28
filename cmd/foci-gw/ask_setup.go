package main

import (
	"time"

	"foci/internal/platform"
	"foci/internal/question"
	"foci/internal/route"
	"foci/internal/tools"
)

// newAskPresentFn builds the presenter for the foci-native `ask` tool. It posts
// one question's options as interactive buttons to the ASKING SESSION'S chat
// (via sessionChatResolver — a question the agent asks its own user must reach
// that user, not the bot's default chat, #2275; the mechanism is otherwise the
// same one permission prompts use) and invokes onResponse with the chosen
// button data when the user clicks. Non-blocking: the tool's Execute returns
// immediately; this fires later on the platform callback. The stored resolver
// also addresses every later proactive edit (post-click, cancel, expiry) to
// the session's chat. An ask from a spawn's branch is shown in the spawn's
// owner's chat instead, or the next of chats that can show it (#2630), named
// as the spawn's.
func newAskPresentFn(agentID string, connMgr platform.ConnectionManager, chats askChats) tools.AskPresentFn {
	return func(sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) string {
		buttons := make([]platform.ButtonChoice, len(choices))
		for i, c := range choices {
			buttons[i] = platform.ButtonChoice{Label: c.Label, Data: c.Data}
		}
		onClick := func(choice platform.ButtonChoice) string {
			onResponse(choice.Data)
			if choice.Data == question.CancelData {
				return "❌ Cancelled"
			}
			if summary != "" {
				return "✅ " + summary
			}
			return "✅ " + choice.Label
		}
		// Expiry: resolve the ask so the asking session isn't left waiting on
		// an answer that will never come — as EXPIRED, not as a user cancel (#2091).
		onExpire := func() { onResponse(question.ExpiredData) }
		// #2630: a spawn's question goes to its current owner, falling back
		// up the chain when that chat cannot show it. A failed send leaves no
		// prompt entry, so msgID is free for the next chat.
		for _, target := range chats.of(sessionKey) {
			body := text
			if target != sessionKey {
				body = prependSpawnOrigin(tools.Spawns, sessionKey, body)
			}
			platformMsgID, err := platform.SendInteractiveMessageWithID(sessionChatResolver(connMgr, target, agentID), msgID, summary, body, buttons, onClick, onExpire)
			if err != nil {
				askLog.Warnf("present question for session=%s in %s failed: %v", sessionKey, target, err)
				continue
			}
			if target != sessionKey {
				askLog.Infof("question for session=%s shown in %s (the spawn's owner chain)", sessionKey, target)
			}
			return platformMsgID
		}
		return ""
	}
}

// askChats lists, in order, the sessions whose chats an ask from a session
// may be shown in (#2630); nil = only the asking session's own.
type askChats func(sessionKey string) []string

func (c askChats) of(sessionKey string) []string {
	if c == nil {
		return []string{sessionKey}
	}
	if ts := c(sessionKey); len(ts) > 0 {
		return ts
	}
	return []string{sessionKey}
}

// first is the chat an ask's later hooks (restore, remove, reveal) address:
// the first of its chats.
func (c askChats) first(sessionKey string) string { return c.of(sessionKey)[0] }

// newAskChats is the gateway's askChats (#2630, Dick 2026-10-28: "it should
// appear in whichever session is the current parent of the spawn, i.e. walk
// according to the same logic as the spawn"). An ask from a spawn's branch
// goes to the spawn's current owner (Spawner.BranchOwner: the starting
// session, or the depth node it was handed over to, by SpawnRoute), then up
// the chain from there as a backend prompt does (promptTargets: live
// ancestors, the root, the default session). The owner itself is skipped
// when it is not live (a reported leaf whose fan-in is still pending). Any
// other session's ask stays in its own chat. owner is Spawner.BranchOwner
// in the gateway (toolDeps.askChats).
func newAskChats(owner func(branchKey string) (string, bool), d backendPromptDeps) askChats {
	return func(sessionKey string) []string {
		owner, ok := owner(sessionKey)
		if !ok {
			return nil
		}
		ts := promptTargets(d, owner)
		if d.live != nil && !d.live(owner) && len(ts) > 1 {
			ts = ts[1:]
		}
		return ts
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
func newAskPresentBatchFn(agentID string, connMgr platform.ConnectionManager, chats askChats) tools.AskPresentBatchFn {
	return func(sessionKey, promptID string, qs []question.Question, onResponse func(answers []string)) bool {
		// #2630: the first of the ask's chats that can take the form; none =
		// sequential, which walks the same chats.
		for _, target := range chats.of(sessionKey) {
			bs, ok := connMgr.ForSessionOrPrimary(target, agentID).(platform.BatchButtonSender)
			if !ok {
				continue // not the app (or no addressable conn)
			}
			bqs := batchQuestionsFor(qs)
			if target != sessionKey && len(bqs) > 0 {
				bqs[0].Text = prependSpawnOrigin(tools.Spawns, sessionKey, bqs[0].Text)
			}
			batched, err := bs.SendInteractiveBatch(promptID, bqs, onResponse)
			if err != nil {
				askLog.Warnf("present batched questions for session=%s in %s failed: %v", sessionKey, target, err)
				continue
			}
			if batched {
				return true
			}
		}
		return false
	}
}

// batchQuestionsFor maps the ask layer's questions onto the app's batched-prompt
// payload. The app renders its own layout, so each question carries STRUCTURED
// fields — raw text, optional header, per-option label+description, and the
// question's quiz data verbatim (#1316: the app frame carries the answer the
// form hides until the user reveals it) and its multiSelect flag (#2308: the
// form renders checkboxes) — rather than pre-rendered markdown.
// Option buttons get "qa:<index>" data and NO Cancel button: the app's
// full-screen ask form supplies its own Cancel (chat platforms, which DO need a
// per-question Cancel, take a separate sequential path via question.Choices).
// An option-less question yields empty Choices (typed-only).
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
			Text:        q.Question,
			Header:      q.Header,
			Choices:     buttons,
			Quiz:        q.Quiz,
			MultiSelect: q.IsMulti(),
		}
	}
	return questions
}

// newAskRevealFn builds the ask layer's quiz-reveal hook (#1316): after the
// user answers a quiz question on the SEQUENTIAL (chat) path, it posts the
// reveal — the correct option(s)/model answer and explanation the prompt
// withheld — to the asking session's chat via route.NotifySessionChat (the
// SessionNotifier path lands it in the asking user's chat on Telegram/Discord,
// #2275; the app's own notification surface otherwise). The text must be PLAIN
// TEXT (it is question.FormatReveal output): Telegram's notification path sends
// without a parse mode, so a markdown marker would render literally (round-3
// finding F1).
func newAskRevealFn(agentID string, connMgr platform.ConnectionManager, chats askChats) tools.AskRevealFn {
	return func(sessionKey, text string) {
		route.NotifySessionChat(connMgr, agentID, chats.first(sessionKey), text)
	}
}

// newAskRestoreBatchFn builds the BATCHED restore hook: after a restart it
// re-registers the server-side callback for an ask that was presented as a native-
// app form, WITHOUT sending a new frame (the app still shows the form). This is the
// batched analogue of newAskRestoreFn — the hub's in-memory batched registration is
// lost on restart, so without it the app's batched answer (InteractiveResponse.
// Answers) matches nothing and is silently dropped (#1473). Returns for any
// transport that can't batch-restore (no app conn / no live binding); the app hub's
// answer-time fallback (fix B) then catches the answer.
func newAskRestoreBatchFn(agentID string, connMgr platform.ConnectionManager, chats askChats) tools.AskRestoreBatchFn {
	return func(sessionKey, promptID string, questionCount int, onResponse func(answers []string)) {
		conn := connMgr.ForSessionOrPrimary(chats.first(sessionKey), agentID)
		br, ok := conn.(platform.BatchButtonRestorer)
		if !ok {
			return // not the app (or no addressable conn) — nothing to re-register
		}
		br.RegisterInteractiveBatch(promptID, questionCount, onResponse)
	}
}

// newAskRemoveFn builds the ask layer's prompt-deletion hook: on a connection that
// can delete a prompt outright (platform.InteractiveRemover — the app) it removes
// the on-screen question of an ask that died unanswered, so it disappears rather
// than lingering (#2080). Any other transport keeps its prompt as it was.
func newAskRemoveFn(agentID string, connMgr platform.ConnectionManager, chats askChats) tools.AskRemoveFn {
	return func(sessionKey, msgID string) {
		r, ok := connMgr.ForSessionOrPrimary(chats.first(sessionKey), agentID).(platform.InteractiveRemover)
		if !ok {
			return
		}
		if err := r.RemoveInteractive(msgID); err != nil {
			askLog.Warnf("remove dead ask prompt %s (session=%s): %v", msgID, sessionKey, err)
		}
	}
}

// newAskRestoreFn builds the restore hook for the foci-native `ask` tool. After a
// restart the question's buttons still live on the platform (the message survived
// in the chat), but foci's in-memory routing entry was lost. This re-registers
// that routing entry against the existing buttons — without sending a new message
// — so a click on the already-displayed buttons resolves the pending ask. The
// platform-side message id is unknown here (we didn't re-send), so proactive
// edits (cancel/expiry) can't touch the message; click-driven routing and the
// "✅ <label>" edit work regardless, since those use the callback's own message.
// The resolver is the session-chat one (see newAskPresentFn): proactive edits of
// a restored ask address the asking session's chat too (#2275). Asks presented
// BEFORE #2275 were posted in the default chat; their restored cancel/expiry
// edits now address the session's chat and fail with a logged warning — click
// handling still works (it uses the callback's own message).
func newAskRestoreFn(agentID string, connMgr platform.ConnectionManager, chats askChats) tools.AskRestoreFn {
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
		platform.RestoreInteractiveCallback(msgID, platformMsgID, sessionChatResolver(connMgr, chats.first(sessionKey), agentID), buttons, func(choice platform.ButtonChoice) string {
			onResponse(choice.Data)
			if choice.Data == question.CancelData {
				return "❌ Cancelled"
			}
			return "✅ " + choice.Label
		}, func() {
			onResponse(question.ExpiredData)
		}, time.Now())
	}
}

// askChats builds the path's askChats (#2630) from the gateway's pieces: the
// spawner (read at call time; nil when the agent has no spawn tool), the
// session index's parent chain and the agent's default session.
func (d *toolDeps) askChats() askChats {
	idx := d.p.sessionIndex
	agentID := d.p.acfg.ID
	// The spawner is read at call time: it is built after the ask tool.
	owner := func(branchKey string) (string, bool) {
		if sp := d.spawner; sp != nil {
			return sp.BranchOwner(branchKey)
		}
		return "", false
	}
	return newAskChats(owner, backendPromptDeps{
		sessionIdx: idx,
		agentID:    agentID,
		live:       depthSessionLive,
		defaultSession: func() string {
			if idx == nil || d.p.cfg == nil {
				return ""
			}
			return idx.DefaultSessionKeyForAgentOn(agentID, d.p.cfg.DefaultPlatformFor(agentID))
		},
	})
}
