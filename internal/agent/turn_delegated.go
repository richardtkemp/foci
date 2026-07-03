package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/compaction"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/provider"
)

// systemInjectRetryInterval bounds each WaitForTurn cycle in RunInference's
// SourceSystem dispatch loop. Turn completion normally wakes the waiter
// immediately; the timeout is purely a lost-signal backstop, after which the
// loop re-checks by retrying the atomic Inject. Not config: an internal
// resilience detail, not a deployment-tunable behaviour.
const systemInjectRetryInterval = 5 * time.Second

// ---------------------------------------------------------------------------
// DelegatedTransport — TurnContract implementations for the delegated transport
// path. Methods that are genuinely no-ops return zero values with a comment
// explaining why. The transport explicitly opts out rather than silently skipping.
// ---------------------------------------------------------------------------

// --- Phase 1: No-ops (CC handles these internally) ---

func (t *DelegatedTransport) RateLimitGate(ts *TurnState) error    { return nil }       // CC has its own rate limiting
func (t *DelegatedTransport) AcquireTurnLock(ts *TurnState) func() { return func() {} } // CC serializes internally

// RegisterTurn adds a TurnDetail so shutdown diagnostics (logBusyAgents) can
// report in-flight delegated turns by session/trigger/tool, same as the API
// path. Per-session in-flight tracking itself is handled by markInFlight in
// the orchestrator.
func (t *DelegatedTransport) RegisterTurn(ts *TurnState) func() {
	td := &TurnDetail{
		SessionKey: ts.SessionKey,
		Trigger:    ts.Trigger,
		StartTime:  time.Now(),
	}
	ts.TurnDetail = td
	ts.TurnID = t.agent.registerTurn(td)
	return func() { t.agent.unregisterTurn(ts.TurnID) }
}

// --- Phase 2: Turn preparation ---

// ResolveModelEffort uses the actual model learned from the JSONL watcher
// (stored in sessionMeta by UpdateSessionMeta), falling back to the
// agent-level model name before the first turn completes.
func (t *DelegatedTransport) ResolveModelEffort(ts *TurnState) {
	if m := t.agent.SessionModel(ts.SessionKey); m != "" {
		ts.TurnModel = m
		return
	}
	ts.TurnModel = t.agent.Model
}

// ComposePrompt builds a flat text prompt via composeTurnText + JoinPrompt.
// Extracted from the original backend_turn.go.
func (t *DelegatedTransport) ComposePrompt(ts *TurnState) error {
	a := t.agent

	parts := a.composeTurnText(ts.Ctx, ts.SessionKey, ts.TurnModel, "", false, ts.Texts, ts.Attachments)
	ts.Prompt = parts.JoinPrompt()

	// First-run onboarding: prepend as a delimited block, then clear. The API
	// path does the equivalent in prepareUserMessage; without this the
	// claude-code backend never delivered onboarding at all (#853).
	if frm := a.consumeFirstRunMessage(); frm != "" {
		ts.Prompt = frm + "\n\n" + ts.Prompt
		log.Infof("delegated", "session=%s injected first-run onboarding (%d chars)", ts.SessionKey, len(frm))
	}

	// Consume branch orientation. ConsumeOrientation is atomic — returns the
	// orientation once and marks it consumed in the DB, same as API transport.
	if a.Sessions != nil {
		if orient := a.Sessions.ConsumeOrientation(ts.SessionKey, a.SessionIndex); orient != "" {
			ts.Prompt = orient + "\n\n" + ts.Prompt
			log.Infof("delegated", "session=%s injected branch orientation (%d chars)", ts.SessionKey, len(orient))
		}
	}

	// Update lastMessageTime AFTER composition so the gap is calculated
	// against the previous message, not the current one.
	ts.SessionMeta.lastMessageTime = ts.UserMessageTime()

	return nil
}

// LoadAndRepairSession is a no-op — CC owns its session file.
func (t *DelegatedTransport) LoadAndRepairSession(ts *TurnState) error { return nil }

// BuildSystemAndTools is a no-op — system prompt and tools are set at Start time.
func (t *DelegatedTransport) BuildSystemAndTools(ts *TurnState) {}

// InjectNudges prepends behavioral nudge reminders to the prompt string.
func (t *DelegatedTransport) InjectNudges(ts *TurnState) {
	a := t.agent
	// Returning before StartTurn means non-user turns (reflection, keepalive,
	// etc.) do not advance the every_n_turns lifetime counter — so the cadence
	// tracks user turns only, and no nudge fires on a system turn. (#815)
	if a.Nudger == nil || len(ts.Texts) == 0 || !nudgesAllowed(ts) {
		return
	}
	a.Nudger.StartTurn(ts.Texts[0])

	var nudges []string
	for _, r := range a.Nudger.CheckTurnInterval() {
		nudges = append(nudges, wrapBundledNudge(r))
	}
	for _, r := range a.Nudger.CheckRegex() {
		nudges = append(nudges, wrapBundledNudge(r))
	}
	if len(nudges) > 0 {
		// Close the nudge region with a single delimiter so the agent can
		// distinguish where the background nudge ends and the user's text
		// begins. Emitted once after the last nudge, not per-nudge.
		ts.Prompt = strings.Join(nudges, "\n") + "\n" + nudgeEndMarker + "\n\n" + ts.Prompt
	}
}

// answerPendingBackendPrompt routes typed text to a pending backend prompt —
// a CC AskUserQuestion or an MCP elicitation free-text field — returning true
// when the text was consumed as the answer. Both prompt kinds share one
// capture semantics, mirroring foci_ask's typed-answer capture where the
// mechanics allow: the batch's first text, whitespace-trimmed; empty text
// falls through to a normal turn; only foldable input is eligible (the caller
// gates on that — system turns and explicitly-queued messages wait instead).
//
// Unlike the foci_ask capture points there is deliberately NO app carve-out
// here: foci_ask is async (the asking turn already ended), so an uncaptured
// message can safely run as a turn — but these prompts block CC mid-turn, and
// anything written to CC while it is asking is taken as the answer anyway.
// Skipping capture would just deliver the same answer with worse bookkeeping.
func answerPendingBackendPrompt(be delegator.Delegator, sessionKey string, texts []string) bool {
	if len(texts) == 0 {
		return false
	}
	text := strings.TrimSpace(texts[0])
	if text == "" {
		return false
	}
	if qr, ok := be.(delegator.QuestionResponder); ok {
		if reqID := qr.HasPendingQuestion(); reqID != "" {
			log.Debugf("delegated", "session=%s intercepting text as question answer: %q", sessionKey, text)
			_ = qr.RespondToQuestion(reqID, text)
			return true
		}
	}
	if er, ok := be.(delegator.ElicitationResponder); ok {
		if reqID := er.HasPendingElicitation(); reqID != "" {
			log.Debugf("delegated", "session=%s intercepting text as elicitation answer: %q", sessionKey, text)
			_ = er.RespondToElicitation(reqID, text)
			return true
		}
	}
	return false
}

// --- Phase 3: Core execution ---

// RunInference sends the composed prompt to the backend with a per-turn
// completion handler that closes CompletionChan and captures results.
//
// Interactive turns (platform message, voice) with a turn already in-flight
// fold into it: the prompt is injected without registering a new callback,
// CC treats both messages as part of one turn, and the original turn's
// handler captures the combined result (this turn's CompletionChan is closed
// immediately so runPostTurn skips it).
//
// System-initiated turns (foci send / HTTP /send, cron, notifications,
// error/restart injections) NEVER fold — they must not steer running work.
// They dispatch as Inject(SourceSystem), and when the backend reports a turn
// in flight they wait gracefully for it to complete before beginning a
// fresh, fully-tracked turn of their own.
func (t *DelegatedTransport) RunInference(ts *TurnState) error {
	a := t.agent
	// foldable = may this turn's text merge into an in-flight turn or answer a
	// pending prompt? Only real-time user input qualifies — and even then the
	// sender can opt out per message (SteerNever, the app's explicit "queue"
	// choice), which gets the full system-turn dispatch guarantees: never
	// folds, never consumed as an answer, waits for backend idle.
	foldable := isInteractiveTrigger(ts.Trigger) && SteerPreferenceFromContext(ts.Ctx) != SteerNever

	log.Debugf("delegated", "RunInference: Get backend start sk=%s", ts.SessionKey)
	be, err := a.DelegatedManager.Get(ts.Ctx, ts.SessionKey)
	if err != nil {
		return err
	}
	log.Debugf("delegated", "RunInference: Get backend done sk=%s", ts.SessionKey)
	ts.Backend = be

	// Typed-answer intercepts apply to foldable input only: a user can
	// answer a pending AskUserQuestion / elicitation field by typing instead
	// of clicking buttons. System text (keepalive, notification, foci send)
	// and explicitly-queued messages (SteerNever) must never be consumed as
	// an answer — they wait below (WaitForPermission + the SourceSystem retry
	// loop) until the prompt resolves and the turn completes.
	if foldable && answerPendingBackendPrompt(be, ts.SessionKey, ts.Texts) {
		close(ts.CompletionChan)
		return nil
	}

	// Wait for any outstanding permission prompt to resolve before sending
	// new input. The backend cannot process messages while blocked on a
	// permission decision. Messages queue naturally in the platform's
	// MessageQueue channel while the worker goroutine blocks here.
	log.Debugf("delegated", "RunInference: WaitForPermission start sk=%s", ts.SessionKey)
	if err := a.DelegatedManager.WaitForPermission(ts.Ctx, ts.SessionKey); err != nil {
		return err
	}
	log.Debugf("delegated", "RunInference: WaitForPermission done sk=%s", ts.SessionKey)

	// Follow-up: a turn is already in-flight. Send the text to CC without
	// creating a new turn pipeline. CC queues it after the current turn,
	// and the backend's SendCommand auto-arms the rearm cascade so the
	// queued response reaches the original handler. Close CompletionChan
	// immediately so runPostTurn exits without doing any post-turn work
	// (the original turn's handler will handle usage, compaction, etc.).
	//
	// Foldable input only: system turns and explicitly-queued messages
	// (SteerNever) skip this and wait via the SourceSystem retry loop below —
	// folding a keepalive / notification / foci send / "queue this" message
	// into running work would steer it.
	//
	// Note: urgent steers (steer_mode=true) on CC backends don't reach
	// this path — they're dispatched immediately by the agent.Inbox
	// (Backend.ImmediateInject(SourceSteer)) so they abort the in-flight turn
	// rather than queuing behind it. This path handles the steer_mode=false
	// case where messages flow through the channel normally and stack as
	// follow-ups.
	if foldable && be.IsTurnInFlight() {
		log.Infof("delegated", "session=%s follow-up message queued behind in-flight turn", ts.SessionKey)
		log.Debugf("delegated", "RunInference: Inject(SourceUser, follow-up) start sk=%s", ts.SessionKey)
		if err := be.ImmediateInject(ts.Ctx, delegator.Inject{
			Source: delegator.SourceUser,
			Text:   ts.Prompt,
		}); err != nil {
			close(ts.CompletionChan)
			return err
		}
		log.Debugf("delegated", "RunInference: Inject(SourceUser, follow-up) done sk=%s", ts.SessionKey)
		// Backend has received the message — signal the inbox so steer
		// routing opens for any further follow-ups arriving on the heels
		// of this one. See WithOnPrimaryWritten / TODO #777.
		OnPrimaryWrittenFromContext(ts.Ctx)()
		close(ts.CompletionChan)
		return nil
	}

	// Cache session file path BEFORE SendToPane — the OnTurnComplete callback
	// may fire inside ensureWatcher (which holds b.mu), and SessionFilePath()
	// also needs b.mu, causing a deadlock.
	ts.sessionFilePath = be.SessionFilePath()

	// Per-turn handler: fires once when the watcher sees end_turn.
	// Captures FinalText/FinalUsage/FinalModel, logs usage, then closes CompletionChan.
	bt := t

	// Cumulative tool-call state for every_n_tools / after_error nudges.
	// These fire via PostToolNudgeFunc, once per tool hook_response — the
	// scheduler's internal cooldown prevents a rule from re-firing on
	// back-to-back tools. Lives in a closure so that each turn starts
	// at zero without polluting DelegatedTransport's long-lived state.
	var toolCount int

	// Pre-answer gate state: when PreAnswerNudgeFunc returns a follow-up,
	// ccstream re-dispatches this handler for a second round. preAnswerFired
	// flips to true on first return so subsequent calls from the second
	// round's OnResult yield "" and break the loop. Round-1 usage is appended
	// to ts.PriorCallUsages (ccstream's beginTurn resets lastUsage between
	// rounds, so we capture it here) and recorded as its OWN api.db row — it is
	// NOT folded into FinalUsage. cache_read is cumulative per call, so summing
	// round 1 + round 2 double-counts the same context as a size signal and
	// trips the compaction trigger; keep the rows separate.
	var (
		preAnswerFired     bool
		preAnswerFirstText string
		thinkingBuf        strings.Builder // accumulates thinking deltas for conversation log
	)

	// Wire session-scoped delivery callbacks via SessionEvents. These live
	// for the session's lifetime, so text/thinking/tool events emitted by
	// the backend are never dropped on per-turn handler nilling — that was
	// the failure mode pre-TODO #747. Re-attached on every RunInference,
	// idempotent (replace) by AttachSessionEvents.
	//
	// The captured sink is the session router (atomic.Pointer-protected,
	// session-scoped, lazy-built once per session). The router forwards to
	// the registered per-turn StreamingSink during a turn and to the
	// fallback SessionSink for late-arriving text outside any turn.
	//
	// ctx for emits is intentionally not captured: turnCtx is per-turn and
	// would be stale by the time late-delivery text fires. context.Background
	// is correct for content events — cancellation logic lives on
	// TurnComplete which is emitted by turn.RunTurn with the per-turn ctx.
	sessionSink := turnevent.SinkFromContext(ts.Ctx)
	sessionEvents := &delegator.SessionEvents{
		OnText: func(text string) {
			sessionSink.Emit(context.Background(), turnevent.TextBlock{Text: text, Phase: turnevent.PhaseIntermediate})
			// Conversation DB log moves to the sink layer in run_turn.go's
			// loggingSink wrapper (per-turn metadata) and inbox.go's
			// lateDeliverySink fallback (session-scoped). See TODO #747.
		},
		OnSubagentText: func(groupKey, text string) {
			sessionSink.Emit(context.Background(), turnevent.SubagentText{GroupKey: groupKey, Text: text})
		},
		OnTextDelta: func(delta string) {
			sessionSink.Emit(context.Background(), turnevent.TextDelta{Delta: delta})
		},
		OnThinkingDelta: func(delta string) {
			sessionSink.Emit(context.Background(), turnevent.ThinkingDelta{Delta: delta})
			thinkingBuf.WriteString(delta)
		},
		OnToolStart: func(id, name, input string) {
			sessionSink.Emit(context.Background(), turnevent.ToolCall{ID: id, Name: name, Args: []byte(input)})
		},
		OnToolEnd: func(id, name, output string, isError bool) {
			sessionSink.Emit(context.Background(), turnevent.ToolResult{ID: id, Name: name, Output: output, IsError: isError})
		},
	}
	be.AttachSessionEvents(sessionEvents)

	// Per-turn bookkeeping callbacks via TurnEvents. These hold per-turn
	// state (preAnswerFired, toolCount, ts) and may legitimately be nil
	// between turns; the backend tolerates that.
	turnEvents := &delegator.TurnEvents{
		PostToolNudgeFunc: func(toolName, toolInput string, isError bool) []string {
			if a.Nudger == nil || !nudgesAllowed(ts) {
				return nil
			}
			toolCount++
			// Record before evaluating so tool_pattern rules see this tool
			// in the ring buffer when shouldFire walks the recent events.
			a.Nudger.RecordToolCall(toolName, toolInput)
			reminders := a.Nudger.CheckAfterTools(toolCount, isError)
			if len(reminders) == 0 {
				return nil
			}
			out := make([]string, 0, len(reminders))
			for _, r := range reminders {
				out = append(out, wrapNudge(r))
			}
			a.logger().Debugf("nudge: injected %d reminder(s) after tool %q (count=%d, err=%v) for session %s",
				len(out), toolName, toolCount, isError, ts.SessionKey)
			return out
		},
		PreAnswerNudgeFunc: func(result *delegator.TurnResult) string {
			if preAnswerFired || a.Nudger == nil || !a.NudgePreAnswerGate || !nudgesAllowed(ts) {
				return ""
			}
			if toolCount < a.NudgePreAnswerMinTools {
				return ""
			}
			reminder := a.Nudger.CheckPreAnswer()
			if reminder == "" {
				return ""
			}
			preAnswerFired = true
			// Stash round-1 state. The original answer has already streamed to
			// the user (OnText delivered PhaseIntermediate), so the sink treats
			// the round-2 result as the authoritative final text. Round-1 usage
			// is recorded as its own api.db row (appended to PriorCallUsages),
			// NOT folded into FinalUsage — see PriorCallUsages doc.
			if result != nil {
				preAnswerFirstText = result.Text
				if result.Usage != nil {
					ts.PriorCallUsages = append(ts.PriorCallUsages, &provider.Usage{
						InputTokens:              result.Usage.InputTokens,
						OutputTokens:             result.Usage.OutputTokens,
						CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
						CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
					})
				}
			}
			a.logger().Infof("nudge: pre-answer gate fired for session %s (tool_count=%d)",
				ts.SessionKey, toolCount)
			return wrapNudge(reminder)
		},
	}
	turnEvents.OnTurnComplete = func(result *delegator.TurnResult) {
		// Guard the whole completion with sync.Once: the delegated backend's
		// normal-result path and its process-exit finalize path can both fire
		// OnTurnComplete for the same turn. Running the body — and the final
		// close(CompletionChan) — at most once makes the loser a no-op instead
		// of a close-of-closed-channel panic that would crash the gateway
		// (P1-8). The first caller wins, so a real result is not clobbered by a
		// late "process exited" finalize.
		ts.completeOnce.Do(func() {
			if result != nil {
				ts.FinalText = result.Text
				ts.FinalModel = result.Model
				if result.Usage != nil {
					ts.FinalUsage = &provider.Usage{
						InputTokens:              result.Usage.InputTokens,
						OutputTokens:             result.Usage.OutputTokens,
						CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
						CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
					}
				}
			}
			// Do NOT fold round-1 usage into FinalUsage. FinalUsage must stay =
			// the last terminal call (round 2) = the true current context size,
			// which the compaction trigger and meta-header read. Round-1 usage
			// is recorded separately (PriorCallUsages -> its own api.db row in
			// LogUsage); turn-total cost is summed there for the sink event.
			// Folding cumulative cache_read across rounds double-counts the same
			// context and trips spurious compactions.
			//
			// If the model chose to echo the sentinel the API path uses to
			// mean "my original answer stands", replace the final text with
			// the round-1 answer so the platform delivery reflects the
			// user-visible intent. Otherwise the round-2 revised answer wins.
			if preAnswerFirstText != "" && strings.TrimSpace(ts.FinalText) == NoResponseSentinel {
				ts.FinalText = preAnswerFirstText
			}
			// Log accumulated thinking to conversation DB.
			if thinking := thinkingBuf.String(); thinking != "" {
				a.logConversationThinking(ts.ConvChatID, ts.Meta, ts.SessionKey, thinking)
			}
			bt.LogUsage(ts)
			close(ts.CompletionChan)
		})
	}
	// Build the attachment list (empty when none); Inject's begin-turn path
	// honors attachments only at idle+SourceUser, and backends that don't
	// support structured content blocks (cctmux) silently drop them with a
	// debug log. The agent layer no longer type-asserts AttachmentSender —
	// the capability decision lives in the backend.
	var atts []delegator.Attachment
	if len(ts.Attachments) > 0 {
		atts = make([]delegator.Attachment, len(ts.Attachments))
		for i, a := range ts.Attachments {
			atts[i] = delegator.Attachment{
				MimeType: a.MimeType,
				Data:     a.Data,
			}
		}
	}

	// Foldable turns begin as SourceUser (idle here — the in-flight case
	// folded above). System turns and explicitly-queued messages begin as
	// SourceSystem: the backend's exclusive begin rejects with
	// ErrTurnInFlight instead of folding, so the prompt can never steer
	// running work. The session inbox worker is the primary serialisation
	// (system entry points enqueue Inject envelopes); the retry loop below
	// is a backstop for turns the worker can't see — backend-only runs
	// (opencode shadow turns) and nested same-session system turns invoked
	// from a turn's post-phase (compaction_memory, session_end_memory)
	// racing one.
	src := delegator.SourceUser
	if !foldable {
		src = delegator.SourceSystem
	}

	log.Debugf("delegated", "RunInference: Inject(%s, begin-turn) start sk=%s attachments=%d", src, ts.SessionKey, len(atts))
	for waited := false; ; waited = true {
		err = be.ImmediateInject(ts.Ctx, delegator.Inject{
			Source:      src,
			Text:        ts.Prompt,
			Attachments: atts,
			Turn:        turnEvents,
		})
		if !errors.Is(err, delegator.ErrTurnInFlight) {
			break
		}
		// Only SourceSystem returns ErrTurnInFlight: a turn is running and
		// system input must not steer it. Wait for completion, then retry
		// the exclusive begin. The timeout bounds a lost completion signal
		// (WaitForTurn wakes a single waiter per completion, and cctmux
		// registers its waiter only at call time) — on timeout the loop
		// simply re-checks via another atomic Inject.
		if !waited {
			log.Infof("delegated", "session=%s system turn (trigger=%q) waiting for in-flight turn to complete before dispatch", ts.SessionKey, ts.Trigger)
		}
		wctx, cancel := context.WithTimeout(ts.Ctx, systemInjectRetryInterval)
		werr := be.WaitForTurn(wctx)
		cancel()
		if werr != nil && ts.Ctx.Err() != nil {
			return ts.Ctx.Err()
		}
	}
	log.Debugf("delegated", "RunInference: Inject(%s, begin-turn) done sk=%s err=%v", src, ts.SessionKey, err)
	if err == nil {
		// Primary has reached the backend. Signal the inbox so turnActive
		// flips true and any further follow-ups can safely steer via
		// Inject(SourceSteer) instead of racing the primary's write. See
		// WithOnPrimaryWritten / TODO #777.
		OnPrimaryWrittenFromContext(ts.Ctx)()
	}
	return err
}

// --- Phase 4: Post-turn ---

// LogConversationSent is a no-op — the delegated path logs each intermediate
// text individually in the OnText callback. The shared implementation would
// log a single concatenated blob of all text from the turn, which is both
// redundant and incorrect (it concatenates separate messages into one row).
func (t *DelegatedTransport) LogConversationSent(ts *TurnState) {}

// SaveSession is a no-op — CC owns its session file.
func (t *DelegatedTransport) SaveSession(ts *TurnState) error { return nil }

// UpdateSessionMeta updates per-session token tracking from the
// JSONL-extracted usage. Cost calculation is not available for delegated
// turns (no logAPIResponse), so prevCost stays zero.
func (t *DelegatedTransport) UpdateSessionMeta(ts *TurnState) {
	if ts.SessionMeta == nil || ts.FinalUsage == nil {
		return
	}
	ts.SessionMeta.lastMessageTime = ts.UserMessageTime()
	// Header token chips: input/output/cache_write sum across all terminal
	// calls (distinct per-call deltas); cache_read is last-call only (cumulative
	// — summing the gate's rounds double-counts the same context). DisplayUsage()
	// encapsulates that rule so this and the FAP/TurnComplete header agree.
	// (FinalUsage != nil is guaranteed by the guard above.)
	du := ts.DisplayUsage()
	ts.SessionMeta.prevInput = du.InputTokens
	ts.SessionMeta.prevOutput = du.OutputTokens
	ts.SessionMeta.prevCacheRead = du.CacheReadInputTokens
	ts.SessionMeta.prevCacheWrite = du.CacheCreationInputTokens

	// Record the actual model reported by the backend so that
	// SessionContextLimit uses the real context window. The modelUserSet flag
	// protects against in-flight turns clobbering a freshly-set /model alias,
	// but is cleared immediately after so that the next turn resolves the alias
	// to its full name (e.g. "sonnet" → "claude-sonnet-4-5-...").
	if ts.FinalModel != "" && !ts.SessionMeta.modelUserSet {
		ts.SessionMeta.model = ts.FinalModel
	}
	ts.SessionMeta.modelUserSet = false
}

// LogUsage records delegated turn usage to the API database. One api.db row is
// written per terminal API call: the pre-answer nudge gate runs two terminal
// calls (round 1, then round 2 after the nudge), so a gated turn emits two
// rows — each with its OWN cost and its OWN un-summed cache_read. Rows are not
// linked (no turn_id); separate records are intentional. cache_read is
// cumulative per call, so summing rounds into one row would double-count the
// same context as a size signal (the bug this replaces).
//
// FinalUsage stays = the last terminal call (= current context size).
// FinalCost is set to the SUM of per-call costs so the TurnComplete sink event
// reports true turn spend.
//
// Self-invoked from the post-turn path after FinalUsage is populated.
func (t *DelegatedTransport) LogUsage(ts *TurnState) {
	if ts.FinalUsage == nil {
		return
	}
	a := t.agent
	model := ts.FinalModel
	if model == "" {
		model = ts.TurnModel
	}

	// Use cached path — SessionFilePath() takes b.mu which may be held
	// by ensureWatcher when this is called from the OnTurnComplete callback.
	sessionFile := ts.sessionFilePath

	// Emit one row per terminal call, prior rounds first then the final call,
	// in chronological order. Sum per-call cost into FinalCost (turn total).
	var turnCost float64
	logCall := func(u *provider.Usage, ts0 time.Time) {
		cost := modelinfo.Cost(model,
			u.InputTokens, u.OutputTokens,
			u.CacheReadInputTokens, u.CacheCreationInputTokens)
		turnCost += cost
		log.API(log.APIEntry{
			Timestamp:   ts0,
			Provider:    "anthropic",
			Session:     ts.SessionKey,
			Model:       model,
			Input:       u.InputTokens,
			Output:      u.OutputTokens,
			CacheRead:   u.CacheReadInputTokens,
			CacheWrite:  u.CacheCreationInputTokens,
			CostUSD:     cost,
			DurationMS:  time.Since(ts.StartedAt).Milliseconds(),
			StopReason:  "end_turn",
			CallType:    "delegated_turn",
			SessionFile: sessionFile,
		})
	}

	// Prior rounds (round-1 of a gated turn) get the turn start time — we don't
	// track per-round completion times for the delegated backend. The final
	// call keeps the current StartedAt-based timestamp.
	for _, u := range ts.PriorCallUsages {
		logCall(u, ts.StartedAt)
	}
	logCall(ts.FinalUsage, ts.StartedAt)

	ts.FinalCost = turnCost

	// Log the last call's context size (FinalUsage = real current size) plus
	// the turn-total cost (summed across all terminal calls this turn).
	a.logger().Infof("session=%s model=%s input=%d output=%d cache_read=%d cache_write=%d calls=%d cost=$%.4f (delegated, last-call size; cost is turn-total)",
		ts.SessionKey, model, ts.FinalUsage.InputTokens, ts.FinalUsage.OutputTokens,
		ts.FinalUsage.CacheReadInputTokens, ts.FinalUsage.CacheCreationInputTokens,
		len(ts.PriorCallUsages)+1, turnCost)
}

// RunCompaction checks whether context compaction is needed and dispatches
// to the shared runDelegatedCompact primitive when the threshold is hit.
// Two triggers: (1) standard threshold (e.g. 80% of context window),
// (2) mana-refresh (lower threshold when Anthropic rate limit resets soon).
// The watcher's TurnUsage provides the token counts; the context window
// comes from model metadata.
func (t *DelegatedTransport) RunCompaction(ts *TurnState) {
	a := t.agent
	if a.Compactor == nil || ts.FinalUsage == nil {
		return
	}

	// last-call context size, NOT turn-total cost — FinalUsage is the final
	// terminal call's usage (= current context size). Never fold prior rounds
	// (e.g. the pre-answer gate's round 1) in here: cache_read is cumulative
	// per call, so summing rounds double-counts the same context and triggers
	// spurious compactions. Prior-round usage lives in ts.PriorCallUsages and
	// is only used for the per-call ledger rows, never for this size check.
	totalTokens := ts.FinalUsage.InputTokens + ts.FinalUsage.CacheReadInputTokens + ts.FinalUsage.CacheCreationInputTokens

	// Lazily learn the real context window from the backend if we don't have
	// it yet. Backends that implement ContextUsageQuerier (CC's
	// get_context_usage; opencode's /config/providers lookup) report the true
	// window; without it we'd fall back to modelinfo's generic 200k, which is
	// wrong by up to 5× for non-Anthropic models (e.g. glm-5.2 = 1M) and fires
	// compaction far too early. Guarded so it runs at most once per session
	// (until the limit is cached) and uses a fresh context — ts.Ctx may be
	// cancelled by the time the post-turn hook runs.
	if !a.SessionContextLimitKnown(ts.SessionKey) {
		a.refreshContextFromBackend(context.Background(), ts.SessionKey)
	}

	ctxLimit := a.SessionContextLimit(ts.SessionKey)
	if ctxLimit <= 0 {
		return
	}

	// Check mana-refresh trigger: compact at a lower threshold when mana
	// reset is imminent so the new window starts with a smaller context.
	isManaRefresh := false
	if a.AutocompactBeforeManaRefresh {
		usageClient := a.SessionUsageClient(ts.SessionKey)
		if usageClient != nil {
			manaRefreshThreshold := parseDurationFallback(a.AutocompactBeforeManaRefreshThreshold, 5*time.Minute)
			if w, err := usageClient.GetUsage(ts.Ctx); err == nil && w != nil && !w.ResetsAt.IsZero() {
				if compaction.ManaResetImminent(w.ResetsAt, manaRefreshThreshold) {
					secondaryThreshold := int(float64(ctxLimit) * a.Compactor.Threshold() * a.AutocompactBeforeManaRefreshFactor)
					if totalTokens > secondaryThreshold {
						isManaRefresh = true
						untilReset := time.Until(w.ResetsAt).Round(time.Minute)
						a.logger().Infof("session=%s mana-refresh compaction (reset in %s, %d/%d tokens, delegated)",
							ts.SessionKey, untilReset, totalTokens, ctxLimit)
					}
				}
			}
		}
	}

	// Standard threshold check (if mana-refresh didn't trigger).
	// Delegated transport has no messages slice — use usage-only check.
	if !isManaRefresh {
		threshold := int(float64(ctxLimit) * a.Compactor.Threshold())
		if totalTokens <= threshold {
			return
		}
		a.logger().Infof("session=%s hit threshold: %d/%d tokens (%d%%, delegated)",
			ts.SessionKey, totalTokens, ctxLimit, int(float64(totalTokens)/float64(ctxLimit)*100))
	}

	if a.SessionNoCompact(ts.SessionKey) {
		percent := int(float64(totalTokens) / float64(ctxLimit) * 100)
		a.logger().Infof("session=%s context at %d%% capacity for no_compact session (delegated)", ts.SessionKey, percent)
		return
	}

	// Memory formation runs only on auto-compaction — the post-turn hook is
	// where the agent records insights before the window shrinks.
	for _, fn := range a.CompactionMemoryFunc {
		fn(ts.SessionKey)
	}

	// Background — not ts.Ctx which may be cancelled (post-turn runs after
	// processAgentMessage returns and the turn context is done).
	if err := a.runDelegatedCompact(context.Background(), ts.Backend, ts.SessionKey); err != nil {
		a.logger().Warnf("session=%s delegated compaction failed: %v", ts.SessionKey, err)
		return
	}

	// Turn-specific post-compact state: per-turn session meta caches are
	// stale now that CC has rewritten the transcript.
	if ts.SessionMeta != nil {
		ts.SessionMeta.systemBlocks = nil
		ts.SessionMeta.prevCacheRead = 0
	}
}
