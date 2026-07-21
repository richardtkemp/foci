package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/session"
	"foci/shared/prompts"
)

// SessionAppender is the interface for session write operations.
// IMPORTANT: Tools must use For(sessionKey) to get a SessionWriter that prevents
// cross-session writes. All write operations on SessionWriter check that the target
// session matches the bound session. Attempting to write to a different session
// will fail with an error.
type SessionAppender interface {
	For(sessionKey string) session.SessionWriter
}

// SessionNotifyFn handles routing a response back to the target session's
// own chat, rather than the calling session.
type SessionNotifyFn func(sessionKey, message string)

// SessionKeyResolverFn resolves a loose session target — anything that does
// not parse as a full session key: a bare agent name ("scout" → the agent's
// default session), a named session ("scout/research"), or a chat alias.
// Returns the resolved key and which resolution rung matched (for the tool's
// receipt), or an error describing why nothing matched.
type SessionKeyResolverFn func(target string) (key, via string, err error)

// SessionWaitFn signals that callerSessionKey has dispatched a reply_to=caller
// send_to_session and is now waiting on targetAgent's reply. Platforms with an
// activity surface (the app) render this as a "waiting" indicator on the caller
// conversation; it is cleared when the caller's next turn begins. Optional.
type SessionWaitFn func(callerSessionKey, targetAgent string)

// CallerSessionTypeFn resolves a session key to its SessionType. Two checks in
// Execute depend on it: the hard bar on reflection/keepalive callers
// (session.SessionType.IsBarredFromSessionSend, #1409) and, for callers that
// aren't barred but are still oneshot (session.SessionType.IsOneshot), the
// reply_to=caller downgrade to fire-and-forget (#1430) — see both checks in
// Execute for why they matter. Optional; a nil fn disables both checks (every
// caller is treated as a normal persistent session, the pre-existing
// behaviour).
type CallerSessionTypeFn func(sessionKey string) session.SessionType

// NewSendToSessionTool creates a tool that injects a user-role message into
// another session and triggers processing via the notifier.
//
// The sessionNotifyFn is called when reply_to="session" — it routes the
// response to the target session's own chat instead of back to the caller.
// The resolveKeyFn is optional — if provided, loose targets (bare agent
// names) are resolved to the active session key.
// onWaitFn, when non-nil, is fired for reply_to=caller dispatches to signal that
// the caller is now waiting on the target agent's reply (see SessionWaitFn).
// callerSessionTypeFn, when non-nil, is used for two caller-type checks: a
// hard refusal when the CALLER is reflection/keepalive (#1409, see the
// IsBarredFromSessionSend check below), and otherwise a downgrade of a
// reply_to=caller relay to fire-and-forget when the CALLER is oneshot (#1430,
// see the oneshotCaller check below).
func NewSendToSessionTool(sessions SessionAppender, notifier *AsyncNotifier, sessionNotifyFn SessionNotifyFn, resolveKeyFn SessionKeyResolverFn, onWaitFn SessionWaitFn, callerSessionTypeFn CallerSessionTypeFn) *Tool {
	return &Tool{
		Name:       "send_to_session",
		ExecExport: true,
		// Generic shell generator supports one positional only — pick
		// session_key (the routing target) and let message come via stdin
		// or --message. Usage: foci_send_to_session <session_key> --message X
		// or: echo "..." | foci_send_to_session <session_key>
		Positional:  []string{"session_key"},
		StdinParam:  "message",
		Description: "Send a message to another session. The message is injected as a user-role message and the target session's agent will see and respond to it. Use this to communicate with branch sessions or the main session. By default the target's reply routes back to you (the caller); set reply_to to \"session\" to have the reply go to the target session's own chat instead.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_key": {
					"type": "string",
					"description": "Target session. Accepts a full session key (e.g. scout/c5970082313), a bare agent name (e.g. scout — resolves to the agent's default session), an agent-qualified session name (e.g. scout/research), or a chat alias."
				},
				"message": {
					"type": "string",
					"description": "Message to send to the target session"
				},
				"reply_to": {
					"type": "string",
					"enum": ["caller", "session"],
					"description": "Where the target's reply goes: 'caller' (back to this session, default) or 'session' (to the target session's own chat)"
				}
			},
			"required": ["session_key", "message"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				SessionKey string `json:"session_key"`
				Message    string `json:"message"`
				ReplyTo    string `json:"reply_to"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}
			if p.SessionKey == "" {
				return ToolResult{}, fmt.Errorf("session_key is required")
			}
			if p.Message == "" {
				return ToolResult{}, fmt.Errorf("message is required")
			}
			if p.ReplyTo == "" {
				p.ReplyTo = "caller"
			}
			if p.ReplyTo != "caller" && p.ReplyTo != "session" {
				return ToolResult{}, fmt.Errorf("reply_to must be 'caller' or 'session', got %q", p.ReplyTo)
			}

			originSession := SessionKeyFromContext(ctx)

			// Hard bar (#1409): a reflection/consolidation/compaction pass or
			// a keepalive cache-warm turn (session.SessionType.
			// IsBarredFromSessionSend) must not message ANOTHER session at
			// all — it runs headless with no human waiting and no context its
			// parent/peers don't already have. A reflection branch once
			// injected an in-flight-bisect status handoff into the main
			// clutch session this way, confusing it. If such a branch has
			// state worth preserving, that belongs in a handoff FILE on disk,
			// not a cross-session message — so refuse before resolving or
			// delivering anything.
			if originSession != "" && callerSessionTypeFn != nil && callerSessionTypeFn(originSession).IsBarredFromSessionSend() {
				return ToolResult{}, fmt.Errorf("tool disabled in oneshot forks, attempting to communicate with your parent or other branches is discouraged, they already have all the same info you have, don't worry!")
			}

			// Resolve loose targets — bare agent names, session names, chat
			// aliases — through the shared route ladder. Full session keys
			// parse cleanly and skip resolution.
			targetKey := p.SessionKey
			resolvedVia := "exact"
			if _, err := session.ParseSessionKey(targetKey); err != nil && resolveKeyFn != nil {
				key, via, rerr := resolveKeyFn(targetKey)
				if rerr != nil {
					return ToolResult{}, fmt.Errorf("could not resolve %q to a session: %w", p.SessionKey, rerr)
				}
				targetKey = key
				resolvedVia = via
				send_to_sessionLog.Infof("resolved %q → %s (via %s)", p.SessionKey, targetKey, via)
			}

			// A reply_to=caller relay only makes sense when the CALLER can
			// actually receive it. A oneshot session (background-task, spawn
			// — see SessionType.IsOneshot; reflection/keepalive are already
			// barred outright above and never reach this line) runs a single
			// headless turn and typically terminates before an async reply
			// could arrive; wiring the reply back to it anyway spins up a
			// fresh, unusable turn whose output has nowhere sane to go — this
			// is exactly how a reflection branch's stale reply leaked into
			// the wrong chat (todo #1430, back when reflection/keepalive
			// still reached this path). Downgrade such callers to the same
			// fire-and-forget delivery reply_to="session" gets: the target's
			// reply lands in the target's OWN chat instead of relaying back.
			// Note 'unknown' (legacy) is deliberately NOT oneshot, so it
			// keeps the pre-existing relay behaviour.
			oneshotCaller := p.ReplyTo == "caller" && originSession != "" &&
				callerSessionTypeFn != nil && callerSessionTypeFn(originSession).IsOneshot()

			// Tag the message with its origin, timestamp, and delivery context.
			// The note tells the receiving agent where its reply will go so it
			// can decide whether to use send_to_chat. A oneshotCaller
			// downgrade gets the "session" tag text, since that's where its
			// reply is actually going now.
			var contextNote string
			if p.ReplyTo == "caller" && !oneshotCaller {
				contextNote = fmt.Sprintf("[SYSTEM INJECTION — a message from ANOTHER AGENT's session (%s), NOT from your human. Your human has NOT seen this and will NOT see your reply to it.\n\n⚠️ REPLY ROUTING — read before you answer: everything you write in reply goes ONLY back to %s. It does NOT reach your human's chat. THE TRAP: if your reply contains an answer, a result, a status, a repro, or a file that YOUR HUMAN is waiting to see, and you just reply here, your human gets NOTHING — it silently goes only to the other agent. To reach your human you MUST call `send_to_chat` yourself (asking %s to relay is fragile).\n\nSo: talking ONLY to the other agent → reply normally. ANYTHING meant for your human → put it in `send_to_chat`. Nothing to say → `[[NO_RESPONSE]]`.]", originSession, originSession, originSession)
			} else {
				contextNote = "[SYSTEM INJECTION — This is a user-role message sent by another session, NOT by the user. The user has not seen it. Your reply will be delivered to the user's chat normally. If the user already knows about it or you don't want to bother them, respond with `[[NO_RESPONSE]]` and nothing else. Otherwise actively *tell* the user about it and explain (e.g. \"I received a notification that...\", \"The system reports...\"). Do NOT passively comment on or observe the content — either `[[NO_RESPONSE]]` or proactively inform the user.]"
			}
			tagged := prompts.FormatInjectedMessage(
				fmt.Sprintf("MESSAGE FROM SESSION %s", originSession),
				time.Now(),
				p.Message,
				contextNote,
			)

			send_to_sessionLog.Infof("from=%s to=%s reply_to=%s oneshot_caller=%v len=%d", originSession, targetKey, p.ReplyTo, oneshotCaller, len(p.Message))

			if p.ReplyTo == "session" {
				// Route the response to the target session's own chat.
				// Don't Append here — sessionNotifyFn calls HandleMessage which
				// loads the session and appends the message itself.
				if sessionNotifyFn != nil {
					sessionNotifyFn(targetKey, tagged)
				}
			} else {
				// Default: route the response back to the caller — unless the
				// caller is a oneshot (see above), in which case downgrade to
				// fire-and-forget by passing replyToSession="" instead of
				// originSession: InjectToAgent then delivers the target's
				// reply to the target's own chat rather than relaying it back.
				// Don't Append here — InjectToAgent triggers HandleMessage which
				// loads the session and appends the message itself.
				if notifier != nil {
					replyToSession := originSession
					if oneshotCaller {
						replyToSession = ""
					}
					notifier.InjectToAgent(targetKey, tagged, replyToSession, "async_notify")
				}
				// reply_to=caller genuinely waits for the target's reply — mark the
				// caller as waiting on the target agent (cleared when the caller's
				// reply turn begins). reply_to=session is fire-and-forget, so no
				// waiting state is set there — and neither is a downgraded
				// oneshotCaller relay, since that caller isn't actually waiting.
				if onWaitFn != nil && originSession != "" && !oneshotCaller {
					onWaitFn(originSession, targetAgentOf(targetKey))
				}
			}

			return TextResult(fmt.Sprintf("Message sent to session %s (resolved via %s, reply_to=%s).", targetKey, resolvedVia, p.ReplyTo)), nil
		},
	}
}

// targetAgentOf derives the target agent id (foci personality) from a resolved
// session key, for the "waiting on <agent>" indicator. Falls back to the key
// itself when it doesn't parse as agentID/typeID.
func targetAgentOf(sessionKey string) string {
	if sk, err := session.ParseSessionKey(sessionKey); err == nil {
		return sk.AgentID
	}
	return sessionKey
}
