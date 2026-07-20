package ccstream

import (
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/delegator"
	"foci/internal/ratelimit"
	"foci/internal/timeutil"
)

const (
	// syntheticModel is CC's sentinel model for an assistant message it produced
	// WITHOUT an API call. It has no pricing, so costing it warns.
	syntheticModel = "<synthetic>"
	// syntheticNoResponseText is CC's fixed text for such a message when the
	// agent had nothing to say (a NO_RESPONSE keepalive/proactive tick, a
	// compaction resume). Mirrors one of platform.silencingSentinels; duplicated
	// here because a delegator backend must not import the platform layer.
	syntheticNoResponseText = "No response requested."
)

func prefixedModel(prefix, model string) string {
	if model == "" {
		return ""
	}
	// Never prefix the synthetic sentinel: a provider-prefixed "<synthetic>" is
	// meaningless, and — critically — it defeats every downstream exact-match
	// guard (turn_delegated.go's session-model poison filter, SessionModel's
	// fallback, modelinfo.IsSynthetic's zero-cost skip). A prefixed sentinel
	// recorded as the session model relaunches CC with `--model <synthetic>`,
	// which errors into another synthetic result — the self-perpetuating brick
	// the 2026-07-13 keepalive incident documents. Keep the sentinel bare.
	if model == syntheticModel {
		return model
	}
	return prefix + "/" + model
}

// isSyntheticNoResponse reports whether m is CC's synthetic no-response
// placeholder: the sentinel model carrying nothing but that exact text. Keyed on
// BOTH the model and the text so a genuine reply that merely quotes the phrase,
// or a real [[NO_RESPONSE]] the agent emits (which has a real model and cost),
// is never dropped.
func isSyntheticNoResponse(m BetaMessage) bool {
	if m.Model != syntheticModel {
		return false
	}
	var text strings.Builder
	for _, block := range m.Content {
		if block.Type == "tool_use" {
			return false // real work in the turn — not a bare no-response
		}
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return strings.TrimSpace(text.String()) == syntheticNoResponseText
}

// syntheticSessionLimitText returns the text of m when it is CC's synthetic
// "You've hit your session limit …" message (the sentinel model carrying that
// phrase), else "". Unlike a direct-API 429, this arrives as an assistant
// message on the stream and never reaches classifyAPIError, so the delegated
// backend detects it here to engage the rate-limit gate.
func syntheticSessionLimitText(m BetaMessage) string {
	if m.Model != syntheticModel {
		return ""
	}
	var text strings.Builder
	for _, block := range m.Content {
		if block.Type == "tool_use" {
			return "" // real work in the turn — not a bare limit notice
		}
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	s := strings.TrimSpace(text.String())
	if strings.Contains(s, "hit your session limit") {
		return s
	}
	return ""
}

// OnAssistant handles assistant messages from CC's stdout.
//
// Sub-agent messages (ParentToolUseID != nil) are filtered out of the
// turn-state updates and handler callbacks below — sub-agents run their own
// turn via the Agent tool, and their text / tool_use blocks belong to the
// sub-agent's transcript rather than the parent turn the caller is
// observing. Without this guard, sub-agent text would fire OnText onto the
// parent's StreamingSink (rendering nested text twice) and sub-agent
// tool_use blocks would fire OnToolStart onto the parent tracker. Model /
// usage tracking is already gated on isTopLevel to protect the primary
// model name from subagent haiku overrides.
func (b *Backend) OnAssistant(msg *AssistantMessage) {
	b.touchActivity()
	isTopLevel := msg.ParentToolUseID == nil

	// CC's synthetic "No response requested." placeholder is a no-API-call turn,
	// not a real reply: drop it here so it never records the (unpriced, warning-
	// triggering) <synthetic> model, appends to the turn text, reaches delivery,
	// or logs. touchActivity above still runs — a NO_RESPONSE keepalive tick did
	// happen, so liveness/keepalive timing should reflect it.
	if isTopLevel && isSyntheticNoResponse(msg.Message) {
		return
	}

	// A synthetic session-limit message is a signal, not a reply. The reset hint
	// is optional; shared policy supplies the usage fallback when it is absent.
	if isTopLevel {
		if s := syntheticSessionLimitText(msg.Message); s != "" {
			signal := ratelimit.Signal{Kind: ratelimit.KindUsage, Detail: s}
			if parsed, ok := parseSessionLimitReset(s, timeutil.Now()); ok {
				signal = parsed
			}
			b.fireSessionLimit(signal)
			return
		}
	}

	// Block-type breakdown for diagnostics — distinguishes "model
	// produced text but it didn't reach delivery" from "model produced
	// no text block at all" when investigating delivery gaps.
	if isTopLevel {
		var textBlocks, toolUseBlocks, thinkingBlocks, totalTextBytes int
		for _, block := range msg.Message.Content {
			switch block.Type {
			case "text":
				textBlocks++
				totalTextBytes += len(block.Text)
			case "tool_use":
				toolUseBlocks++
			case "thinking":
				thinkingBlocks++
			}
		}
		stopReason := ""
		if msg.Message.StopReason != nil {
			stopReason = *msg.Message.StopReason
		}
		b.logger().Debugf("OnAssistant: text_blocks=%d tool_use_blocks=%d thinking_blocks=%d text_bytes=%d stop_reason=%s",
			textBlocks, toolUseBlocks, thinkingBlocks, totalTextBytes, stopReason)
	}

	b.mu.Lock()
	if isTopLevel && msg.Message.Model != "" {
		b.lastModel = msg.Message.Model
	}
	if isTopLevel {
		u := msg.Message.Usage
		b.lastUsage = &u
	}
	b.mu.Unlock()

	// Delivery callbacks come from the session-scoped SessionEvents — never
	// nil after first AttachSessionEvents, so text/tool blocks always have
	// somewhere to go regardless of per-turn TurnEvents state. This is what
	// kills the "text block dropped: handler nil" failure mode at backend
	// layer; see TODO #747.
	se := b.sessionEvents.Load()

	if !isTopLevel {
		// Surface sub-agent text as blockquoted intermediate replies so
		// the user can follow sub-agent progress. Tool_use blocks are not
		// forwarded — the parent tracker owns tool visibility.
		//
		// Route via OnSubagentText (carrying the parent tool_use id as the
		// group key) when the consumer supports it — that lets the platform
		// attach a rolling "Hide this" control and delete the group on demand.
		// Fall back to OnText for consumers without subagent support.
		groupKey := ""
		if msg.ParentToolUseID != nil {
			groupKey = *msg.ParentToolUseID
		}
		if se != nil {
			for _, block := range msg.Message.Content {
				if block.Type != "text" || block.Text == "" {
					continue
				}
				switch {
				case se.OnSubagentText != nil:
					// Raw text; the label rides SubagentStart, and blockquote is a
					// per-platform choice applied in the renderer, not here.
					se.OnSubagentText(groupKey, block.Text, b.runIndexForGroup(groupKey))
				case se.OnText != nil:
					se.OnText(block.Text)
				}
			}
		}
		// Keep typing indicator alive during sub-agent work.
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
		return
	}

	// Separate this message's text from any text already accumulated by PRIOR
	// assistant messages in this turn (segments split by tool calls) with a
	// blank line — otherwise pre-tool-call narration glues onto the next
	// segment (e.g. "...correctly.Καλημέρα"). Text blocks WITHIN a single
	// message are still concatenated directly: the model may split one sentence
	// across blocks ("Hello " + "world!"). See TODO #819.
	b.turnMu.Lock()
	needSep := b.turnText.Len() > 0
	b.turnMu.Unlock()

	for _, block := range msg.Message.Content {
		switch block.Type {
		case "text":
			b.turnMu.Lock()
			if block.Text != "" {
				if needSep {
					b.turnText.WriteString("\n\n")
					needSep = false
				}
				b.turnText.WriteString(block.Text)
			}
			b.turnMu.Unlock()

			if se != nil && se.OnText != nil {
				se.OnText(block.Text)
			}

		case "tool_use":
			b.turnMu.Lock()
			b.turnTools++
			b.turnMu.Unlock()

			if se != nil && se.OnToolStart != nil {
				inputStr := string(block.Input)
				se.OnToolStart(block.ID, block.Name, inputStr)
			}

			// Track background work for status reporting AND the pending-work gate
			// (spec §4). Agent-tool subagents and run_in_background Bash both
			// outlive their turn and drive a task_notification / autonomous run on
			// completion, so both must count toward Pending(). A synchronous Bash
			// completes inside the turn and is not tracked.
			if block.Name == "Agent" {
				desc := delegator.ExtractAgentDescription(block.Input)
				b.agents.Add(block.ID, desc)
				// Stash the label + prompt by the Agent tool_use_id (= the stable
				// groupKey) so the first task_started can bind the reactivation run
				// state (#1355) and, when the PreToolUse hook never fires, supply
				// the task_started fallback SubagentStart's content (#1425) — the
				// hook path normally reads label/prompt straight off its own
				// payload, but the fallback has only the native task_started event
				// (no prompt field), so it needs this stash instead.
				b.setAgentLabel(block.ID, desc)
				b.setAgentPrompt(block.ID, delegator.ExtractAgentPrompt(block.Input))
				// A FOREGROUND subagent's assistant text never reaches the parent
				// stdout stream, so arm a transcript tail for it (started once
				// task_started supplies the agent_id). Arm HERE — the earliest,
				// race-free point: the model emits this tool_use before the task
				// can start, so expectForeground is set before task_started. A
				// background subagent already streams its text and must NOT be
				// tailed (double-delivery).
				if !delegator.ExtractAgentBackground(block.Input) {
					b.subagentTails().expectForeground(block.ID)
				}
			} else if block.Name == "Bash" && delegator.ExtractBashBackground(block.Input) {
				b.agents.Add(block.ID, "background command")
			} else if block.Name == "TaskStop" {
				// TaskStop kills a background task, but CC emits NO
				// task_notification for a stopped task (only for one that
				// completes naturally) — so the Add above would never be
				// balanced, leaving the entry stuck in Pending() until the
				// 30-min max-age prune and needlessly holding the pending-work
				// gate (spec §4). Decrement one pending entry here. Count-based
				// like the completion path (RemoveOne, not exact-match): the
				// tracker feeds a count for the app badge and the gate, exact
				// tool_use-id matching off a stream/tool event isn't reliable
				// (see SubagentTracker.RemoveOne), and RemoveOne on an empty
				// tracker is a safe no-op.
				if !b.agents.RemoveOne() && b.agents.OnStatus != nil {
					b.agents.OnStatus("")
				}
			} else if block.Name == "SendMessage" {
				// A SendMessage can target a subagent (keyed by task_id == the
				// SendMessage `to`) in either of two states (#1419):
				//  - STILL RUNNING (task_started seen, not yet completed): CC never
				//    refires task_started for this case (verified live) — there is no
				//    later event to hang the prompt on, so surface it immediately as a
				//    mid-run OnSubagentPrompt at the run's CURRENT index. No new chit.
				//  - ALREADY ENDED (#1355): stash the message; the reactivation's
				//    task_started (which bumps runIndex and emits a fresh SubagentStart)
				//    follows this block in the stream and consumes the stash.
				if to, msg := delegator.ExtractSendMessage(block.Input); to != "" {
					if run, active := b.activeRunForTask(to); active {
						b.logger().Infof("subagent_prompt signal=send_message group=%s run=%d", run.groupKey, run.runIndex)
						if se := b.sessionEvents.Load(); se != nil && se.OnSubagentPrompt != nil {
							se.OnSubagentPrompt(run.groupKey, msg, run.runIndex)
						}
					} else {
						b.stashResumePrompt(to, msg)
					}
				}
			}

		case "thinking":
			// Thinking blocks are informational; optionally log.
		}
	}

	// Restart typing indicator if the turn hasn't ended.
	if msg.Message.StopReason == nil || *msg.Message.StopReason != "end_turn" {
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
	}
}

// OnResult handles a result message. Under the idle-keyed lifecycle a result
// is NOT the turn boundary — it is one internal ask cycle's accounting. CC
// mints 0, 1 or N results per logical turn (a "now" steer aborts the current
// ask and adds one; a steer landing mid-tool folds and adds none; results are
// withheld while background agents run), so the result is stashed (latest
// wins; output tokens accumulate across cycles) and the turn completes when
// CC's session_state_changed:idle arrives — see onSessionIdle.
//
// Legacy fallback: when CC has emitted no session-state events this session
// (env unset, older binary), complete on the result as the pre-idle design
// did. See docs/WIRING.md → "Idle-keyed turn completion".
func (b *Backend) OnResult(msg *ResultMessage) {
	b.touchActivity()

	b.turnMu.Lock()
	turnActive := b.turnActive
	turnText := b.turnText.String()
	turnTools := b.turnTools
	b.turnMu.Unlock()

	// Build TurnResult. Prefer turnText (accumulated from all assistant
	// messages in the turn) over msg.Result (which only contains the last
	// segment). Multi-segment turns (text → tool → text) need the full text.
	text := turnText
	if text == "" {
		text = msg.Result
	}

	// Detect a 401 auth failure surfaced as an error result and trigger
	// automated re-login (#843). Firing here and on the subprocess exit path is
	// safe — the re-login gate single-flights.
	if msg.IsError && isAuthFailure(text) {
		b.fireAuthFailure(text)
	}

	// Determine model from lastModel (set by OnAssistant, filtered to top-level
	// messages only — subagent models are excluded). Use per-call usage from
	// the last assistant message (not the result's accumulated total) — this
	// matches what the tmux watcher reports and gives compaction the actual
	// context window fill, not a sum of all calls.
	b.mu.Lock()
	resultModel := b.lastModel
	lastUsage := b.lastUsage
	b.lastUsage = nil // reset for next turn
	b.mu.Unlock()

	// Pick context window from ModelUsage deterministically: prefer the
	// entry matching resultModel (the primary model from assistant messages);
	// otherwise take the largest context window to avoid spurious compaction
	// from subagent models (e.g. haiku) winning the random map iteration.
	if usage, ok := msg.ModelUsage[resultModel]; ok {
		b.mu.Lock()
		b.contextWindow = usage.ContextWindow
		b.mu.Unlock()
	} else {
		var bestCW int
		for _, usage := range msg.ModelUsage {
			if usage.ContextWindow > bestCW {
				bestCW = usage.ContextWindow
			}
		}
		if bestCW > 0 {
			b.mu.Lock()
			b.contextWindow = bestCW
			b.mu.Unlock()
		}
	}

	// Input/cache come from the last assistant message — the FINAL call's
	// context fill, which compaction needs (not a sum of all calls). Fall back
	// to the result's accumulated usage if no assistant messages were seen.
	var turnUsage *delegator.TurnUsage
	if lastUsage != nil {
		turnUsage = &delegator.TurnUsage{
			InputTokens:              lastUsage.InputTokens,
			OutputTokens:             lastUsage.OutputTokens,
			CacheCreationInputTokens: lastUsage.CacheCreationInputTokens,
			CacheReadInputTokens:     lastUsage.CacheReadInputTokens,
		}
	} else {
		turnUsage = &delegator.TurnUsage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		}
	}

	// OUTPUT tokens must NOT be trusted from lastUsage: the last assistant
	// message's usage in the live stream is an early/partial snapshot (often
	// output_tokens≈1) that is never refreshed to the final count before this
	// result arrives, so lastUsage.OutputTokens massively undercounts a
	// substantive reply — a ~2000-token answer logged as output=4 (#721),
	// undercounting api.db delegated-turn cost. The result's per-model
	// accounting (msg.ModelUsage[resultModel]) is CC's authoritative end-of-turn
	// total for the primary model — subagent models are separate keys, so they
	// stay excluded from the primary's cost. On a key miss, fall back to the
	// result's accumulated total (msg.Usage, all models). Apply as a floor so
	// it can only correct an undercount, never regress a good value.
	authoritativeOutput := msg.Usage.OutputTokens
	if mu, ok := msg.ModelUsage[resultModel]; ok {
		authoritativeOutput = mu.OutputTokens
	}
	if authoritativeOutput > turnUsage.OutputTokens {
		turnUsage.OutputTokens = authoritativeOutput
	}

	result := &delegator.TurnResult{
		Text:      text,
		Model:     prefixedModel("claude", resultModel),
		ToolCalls: turnTools,
		Usage:     turnUsage,
	}

	// Stash this cycle's result; the turn total for output tokens is the sum
	// across cycles (each result's usage is per-ask-cycle, probe-verified),
	// while text (turnText spans the whole turn), tool count, model and
	// input/cache (the FINAL cycle's context fill — what compaction needs)
	// are latest-wins. A fresh result also satisfies any pre-answer
	// re-dispatch that was holding the turn open at idle.
	b.turnMu.Lock()
	b.turnCalls++
	cycle := b.turnCalls
	b.turnOutputTokens += result.Usage.OutputTokens
	result.Usage.OutputTokens = b.turnOutputTokens
	b.stashedResult = result
	b.stashedResultMsg = msg
	b.redispatchInFlight = false
	stateSeen := b.stateEventsSeen
	b.turnMu.Unlock()

	b.logger().Debugf("OnResult: stashed ask-cycle result (turn_active=%v cycle=%d textlen=%d out_total=%d)",
		turnActive, cycle, len(text), result.Usage.OutputTokens)
	b.logger().Debugf("turn_lifecycle event=result_stash cycle=%d turn_active=%v subtype=%s textlen=%d out_total=%d",
		cycle, turnActive, msg.Subtype, len(text), result.Usage.OutputTokens)

	// A non-success result (error_during_execution — includes a user /stop
	// interrupt, per Backend.Interrupt/SendInterrupt; error_max_turns;
	// error_max_budget_usd; error_max_structured_output_retries) means this
	// ask cycle was cut short. Any subagent this cycle spawned in the
	// background is now orphaned exactly like the finalizeExit case just
	// below (subprocess gone: pending agents can never complete) — except
	// here the PROCESS survives, only the current ask aborted, so
	// finalizeExit's ClearAll() never runs. Without this, an interrupted
	// background subagent's tracker entry sits pending for the full
	// defaultAgentMaxAge (30m), blocking the session via the sink-delivery
	// gate (#767) and the pending-work gate (spec §4) the whole time — clutch
	// #1350, 2026-07-17: a /stop mid-subagent-spawn wedged the session for
	// ~29 minutes waiting on a task_notification that never arrived because
	// the subagent it was tracking had already been killed by the interrupt.
	if msg.Subtype != "" && msg.Subtype != "success" {
		b.agents.ClearAll()
	}

	if !turnActive {
		// Autonomous turn (no foci turn open — e.g. a background-agent or Bash
		// completion triggers a task-notification run). Its text already
		// delivered via the always-live SessionEvents; nothing to complete.
		return
	}

	if !stateSeen {
		// CC is not emitting session-state events — no idle will ever come.
		// Complete on the result, the pre-idle-keyed behaviour (including the
		// pre-answer verification gate, which normally runs at idle).
		b.turnMu.Lock()
		warned := b.fallbackWarned
		b.fallbackWarned = true
		turn := b.turnEvents
		b.turnMu.Unlock()
		if !warned {
			b.logger().Warnf("no session_state_changed events from CC (CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS unset or unsupported); falling back to complete-on-result — steers folded mid-turn may complete early")
		}
		if b.tryPreAnswerRedispatch(turn, result) {
			return
		}
		b.completeTurn("result-fallback")
	}
}

// OnSystem handles system messages (init, status, compact_boundary, etc.).
func (b *Backend) OnSystem(subtype string, raw json.RawMessage) {
	b.touchActivity()
	switch subtype {
	case "init":
		var init InitMessage
		if err := json.Unmarshal(raw, &init); err != nil {
			b.logger().Warnf("drop init message (unmarshal failed): %v — WaitReady will stall", err)
			return
		}
		b.mu.Lock()
		b.sessionID = init.SessionID
		b.initMsg = &init
		b.permMode = init.PermissionMode
		b.lastModel = init.Model
		b.mu.Unlock()
		b.readyOnce.Do(func() { close(b.readyCh) })
		if b.onSessionReady != nil {
			b.onSessionReady(init.SessionID)
		}

	case "status":
		var status StatusMessage
		if err := json.Unmarshal(raw, &status); err != nil {
			b.logger().Warnf("drop status message (unmarshal failed): %v — compaction-start waiter may stall", err)
			return
		}
		if status.Status != nil && *status.Status == "compacting" {
			if b.onCompactionStart != nil {
				b.onCompactionStart()
			}
			// Signal any armed compaction start waiter (one-shot).
			b.turnMu.Lock()
			sch := b.compactStartCh
			b.compactStartCh = nil
			b.turnMu.Unlock()
			if sch != nil {
				select {
				case sch <- struct{}{}:
				default:
				}
			}
		}

	case "compact_boundary":
		var cb CompactBoundaryMessage
		if err := json.Unmarshal(raw, &cb); err != nil {
			b.logger().Warnf("drop compact_boundary message (unmarshal failed): %v — compaction-done waiter may stall", err)
			return
		}
		if b.onCompactionDone != nil {
			b.onCompactionDone(cb.CompactMetadata.PreTokens)
		}
		// Signal any armed compaction waiter (one-shot; clear after firing).
		// Clear the abort channel too so the following idle's abort check
		// (signalCompactionAbort) sees the wait already satisfied and no-ops.
		b.turnMu.Lock()
		ch := b.compactDoneCh
		b.compactDoneCh = nil
		b.compactAbortCh = nil
		b.turnMu.Unlock()
		if ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}

	case "session_state_changed":
		// CC's authoritative run-loop boundary (opt-in via
		// CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1, set at launch). `running`
		// fires at run() entry; `idle` fires at run() exit, AFTER the held-back
		// result flush — it is the turn-completion signal. See OnResult and
		// docs/WIRING.md → "Idle-keyed turn completion".
		var ss SessionStateMessage
		if err := json.Unmarshal(raw, &ss); err != nil {
			b.logger().Warnf("drop session_state_changed (unmarshal failed): %v — turn completion may fall to the orchestrator timeout", err)
			return
		}
		b.turnMu.Lock()
		b.stateEventsSeen = true
		turnActive := b.turnActive
		autonomousOpen := false
		if ss.State == "running" && !turnActive && b.onAutonomousOpen != nil {
			// CC opened a run foci didn't (autonomous: background-agent
			// completion, task-notification, continuation). Adopt it as a
			// first-class turn. Enqueue the open so it fires off turnMu but still
			// synchronously on this reader goroutine (via drainEdgeCallbacks
			// below, before the next stream event is read) — so the streaming
			// sink is registered with no early deltas lost (#1261).
			b.edgeCallbacks = append(b.edgeCallbacks, b.onAutonomousOpen)
			autonomousOpen = true
		}
		b.turnMu.Unlock()
		b.drainEdgeCallbacks()
		b.logger().Debugf("turn_lifecycle event=session_state state=%s turn_active=%v autonomous_open=%v", ss.State, turnActive, autonomousOpen)
		if ss.State == "idle" {
			// If a compaction wait is still armed at idle, no compact_boundary
			// arrived — the backend declined to compact (e.g. "Not enough
			// messages to compact"). Unblock WaitForCompaction now so the
			// caller doesn't stall out the full timeout with the session's
			// inbox held (#1267). A real compaction fires compact_boundary
			// before idle, which already cleared the wait, so this no-ops there.
			b.turnMu.Lock()
			b.signalCompactionAbort()
			b.turnMu.Unlock()
			b.onSessionIdle()
		}

	case "task_started", "task_progress", "task_notification":
		var task TaskEvent
		if err := json.Unmarshal(raw, &task); err != nil {
			b.logger().Warnf("drop %s message (unmarshal failed): %v — task tracker may not clear", subtype, err)
			return
		}
		switch subtype {
		case "task_started":
			// A foreground subagent's assistant text never reaches the parent
			// stdout stream (CC filters it), so start tailing its transcript to
			// forward that text into the chit. maybeStart is a no-op unless a
			// foreground Agent PreToolUse was recorded for this tool_use id, so
			// background subagents (whose text already streams) are skipped.
			if task.ToolUseID != "" && task.TaskID != "" {
				if path := b.subagentTranscriptPath(task.TaskID); path != "" {
					b.subagentTails().maybeStart(task.ToolUseID, path)
				}
			}
			// Bind or advance the reactivation run state (#1355). The FIRST
			// task_started binds the run — run 1's SubagentStart is NORMALLY
			// hook-driven (fires earlier, at the Agent tool_use itself), but the
			// PreToolUse hook drops for ~7% of background subagents (#1423), so
			// this also emits a FALLBACK start, guarded by markSubagentStarted so
			// exactly one goes out regardless of which signal wins the race
			// (#1425). A SUBSEQUENT task_started for the same task_id is a
			// SendMessage resume: re-Add to the tracker so the activity chip
			// re-opens, and emit a fresh SubagentStart for the new run so the app
			// draws a new chit — unconditional, since no hook exists for
			// SendMessage. groupKey stays the ORIGINAL Agent tool_use_id (the
			// subagent's text keeps it as parent_tool_use_id across resumes), so all
			// runs collapse into one continuous view.
			if run, reactivated, prompt := b.onTaskStarted(task.TaskID, task.ToolUseID); reactivated {
				b.agents.Add(run.groupKey, run.label)
				b.logger().Infof("subagent_reactivate task_id=%s group=%s run=%d", task.TaskID, run.groupKey, run.runIndex)
				if se := b.sessionEvents.Load(); se != nil && se.OnSubagentStart != nil {
					se.OnSubagentStart(run.groupKey, run.label, prompt, run.runIndex)
				}
			} else if run != nil && !b.markSubagentStarted(run.groupKey) {
				b.logger().Infof("subagent_start signal=task_started_fallback group=%s run=%d (PreToolUse hook missing/late)", run.groupKey, run.runIndex)
				if se := b.sessionEvents.Load(); se != nil && se.OnSubagentStart != nil {
					se.OnSubagentStart(run.groupKey, run.label, prompt, run.runIndex)
				}
			}
		case "task_notification":
			if task.Status == "completed" {
				// Remove one pending subagent. If the tracker had nothing
				// (e.g. tool_use detection missed it), the resolved state is
				// already "no subagents running" — signal that with an empty
				// detail so any stale indicator clears.
				if !b.agents.RemoveOne() && b.agents.OnStatus != nil {
					b.agents.OnStatus("")
				}
				// The subagent RUN's true end, for foreground AND background alike (a
				// background Agent tool_use resolves at launch, so its PostToolUse end
				// is premature; this fires at actual completion). Map task_id -> the
				// stable groupKey + runIndex (#1355): a resumed run's task_notification
				// carries the SendMessage tool_use_id, NOT the group key, so ending on
				// task.ToolUseID would close a group the app never opened. Fall back to
				// the raw tool_use_id (runIndex 0) only when task_started was missed.
				// endRunForTask also flips the run inactive (#1419): a SendMessage
				// arriving AFTER this point (before any reactivation) must stash for
				// the eventual resume, not try to surface immediately.
				groupKey, runIndex := task.ToolUseID, 0
				if run := b.endRunForTask(task.TaskID); run != nil {
					groupKey, runIndex = run.groupKey, run.runIndex
				}
				if groupKey != "" {
					b.logger().Infof("subagent_end signal=task_notification group=%s run=%d", groupKey, runIndex)
					if se := b.sessionEvents.Load(); se != nil && se.OnSubagentEnd != nil {
						se.OnSubagentEnd(groupKey, runIndex)
					}
				}
			}
		}

	case "api_retry":
		// CC handles its own API retries internally; we parse the message
		// for symmetry with the protocol but do not surface it to the user.
		// The turnevent.RetryNotice / RetrySuccess UI is for the API tool
		// loop's own retries, which don't apply when CC owns inference.
		var retry APIRetryMessage
		if err := json.Unmarshal(raw, &retry); err != nil {
			return
		}
		_ = retry

	case "hook_response":
		// PostToolUse / PostToolUseFailure hook completions. Parsed and
		// dispatched to the sessions SessionEvents.OnToolEnd via the
		// helper defined in hooks.go.
		b.handleHookResponse(raw)

	case "elicitation_complete":
		// CC re-broadcasts an MCP server's elicitation_complete notification
		// when a URL-mode flow was completed externally. Match by
		// elicitation_id and auto-accept so the user doesn't have to click
		// Done after already finishing in the browser.
		var done ElicitationCompleteMessage
		if err := json.Unmarshal(raw, &done); err != nil {
			b.logger().Warnf("drop elicitation_complete message (unmarshal failed): %v — URL elicitation will not auto-resolve", err)
			return
		}
		b.OnElicitationComplete(&done)
	}
}

// OnPermissionRequest handles can_use_tool control requests from CC.
// Dispatches to tool-specific handlers (e.g. AskUserQuestion) or the
// standard permission prompt flow.
func (b *Backend) OnPermissionRequest(msg *PermissionRequest) {
	b.touchActivity()
	b.handleToolRequest(msg)
}

// OnControlResponse handles responses to our control requests (e.g. initialize,
// get_context_usage). Routes to pending waiters by request_id.
//
// For fresh sessions (no --resume), CC responds to the initialize control
// request with a control_response rather than emitting a system/init message.
// When we detect the initialize response, we close readyCh so WaitReady
// unblocks.
func (b *Backend) OnControlResponse(raw json.RawMessage) {
	b.touchActivity()
	var env controlResponseInbound
	if err := json.Unmarshal(raw, &env); err != nil {
		b.logger().Debugf("unmarshal control_response: %v", err)
		return
	}
	reqID := env.Response.RequestID
	if reqID == "" {
		return
	}

	// Check if this is the response to our initialize request.
	b.mu.Lock()
	isInit := b.initReqID != "" && reqID == b.initReqID
	if isInit {
		b.initReqID = "" // consume — only match once
	}
	b.mu.Unlock()
	if isInit {
		b.readyOnce.Do(func() { close(b.readyCh) })
	}

	b.pendingControlMu.Lock()
	ch, ok := b.pendingControls[reqID]
	if ok {
		delete(b.pendingControls, reqID)
	}
	b.pendingControlMu.Unlock()
	if ok {
		select {
		case ch <- raw:
		default:
		}
	}
}

// OnControlCancelRequest handles CC cancelling a pending control request.
func (b *Backend) OnControlCancelRequest(reqID string) {
	b.touchActivity()
	b.handleControlCancel(reqID)
}

// OnKeepAlive handles heartbeat events. Touches activity so the idle/timeout
// tracker sees the stream as alive during periods where CC is blocked (e.g.
// waiting for a permission prompt response) and not emitting work events.
//
// NOTE: As of CC 1.x, keep_alive frames are only sent on WebSocket transports
// (remote control sessions). In --pipe mode (stdin/stdout, which foci uses),
// CC never sends keep_alive — so this handler is effectively dead code.
// The idle tracker must be kept alive by other means (e.g. touchActivity on
// permission request arrival). See also runKeepAlive which sends keep_alive
// TO CC (also a no-op: CC silently ignores them in pipe mode).
func (b *Backend) OnKeepAlive() {
	b.touchActivity()
}

// OnRateLimit handles CC's structured rate_limit_event: CC emits it on status
// transitions (allowed → allowed_warning → rejected) with the API's
// utilization. Past the "allowed" threshold we surface a WARNING via the
// rate-limit hook — informational only, it does NOT gate periodic work.
//
// CC repeats the event, so a warning per event would flood (#1211/#1238).
// We throttle via a shared RateLimitThrottle (one per agent, so main + facet
// sessions don't each fire independently) keyed by status|type, re-armed when
// resetsAt changes. Only warn when utilization climbs to a new bucket: one
// warning per 5% below 95%, then every 1% at/above 95% — see rateLimitBucket.
func (b *Backend) OnRateLimit(ev *RateLimitEvent) {
	b.touchActivity()
	if ev == nil {
		return
	}
	info := ev.RateLimitInfo
	if info.Status == "" || info.Status == "allowed" {
		return
	}
	resetsAt := int64(0)
	if info.ResetsAt != nil {
		resetsAt = int64(*info.ResetsAt)
	}
	key := fmt.Sprintf("%s|%s", info.Status, info.RateLimitType)
	bucket := rateLimitBucket(info.Utilization)

	utilStr := "nil"
	if info.Utilization != nil {
		utilStr = fmt.Sprintf("%.4f", *info.Utilization)
	}

	// Only fire on boundary crossings (every 5% below 95%, every 1% at/above).
	// Intermediate values like 87% are logged but never delivered to the user.
	// Nil utilization (e.g. a rejected event without usage data) bypasses this.
	if info.Utilization != nil && !rateLimitOnBoundary(info.Utilization) {
		b.logger().Debugf("rate_limit_event off-boundary (skip): key=%s util=%s bucket=%d resetsAt=%d",
			key, utilStr, bucket, resetsAt)
		return
	}

	res := b.rlThrottle.evaluate(key, resetsAt, bucket)
	switch {
	case !res.fire:
		b.logger().Debugf("rate_limit_event suppressed: key=%s util=%s bucket=%d resetsAt=%d prev{bucket=%d,resetsAt=%d}",
			key, utilStr, bucket, resetsAt, res.prev.bucket, res.prev.resetsAt)
	case !res.seen:
		b.logger().Debugf("rate_limit_event FIRED (first-seen): key=%s util=%s bucket=%d resetsAt=%d",
			key, utilStr, bucket, resetsAt)
	case res.prev.resetsAt != resetsAt:
		b.logger().Debugf("rate_limit_event FIRED (resetsAt changed %d->%d): key=%s util=%s bucket=%d",
			res.prev.resetsAt, resetsAt, key, utilStr, bucket)
	default:
		b.logger().Debugf("rate_limit_event FIRED (bucket climbed %d->%d): key=%s util=%s resetsAt=%d",
			res.prev.bucket, bucket, key, utilStr, resetsAt)
	}

	if !res.fire {
		return
	}
	b.fireRateLimited(FormatRateLimitNotice(info))
}

// OnToolProgress handles heartbeats during long-running tool execution.
func (b *Backend) OnToolProgress(msg *ToolProgressMessage) {
	b.touchActivity()
	// Keep typing indicator alive during tool execution.
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
}

// OnStreamEvent handles token-level streaming events. CC wraps Anthropic
// SDK stream parts in these envelopes (services/api/claude.ts:2300), so the
// event payload is a verbatim SDK `content_block_delta` with subtypes like
// `text_delta` and `thinking_delta` that we extract separately.
//
// Sub-agent stream events (ParentToolUseID != nil) are filtered out, matching
// the guard in OnAssistant. Sub-agent text is delivered as complete blocks
// (blockquoted) via OnAssistant instead. Without this filter, sub-agent
// deltas leak into the parent turn's StreamWriter — accumulating text that
// is never Finish()ed by OnReply, which corrupts the parent's stream message
// and silently discards the parent's reply text.
func (b *Backend) OnStreamEvent(raw json.RawMessage) {
	b.touchActivity()
	var env struct {
		ParentToolUseID *string `json:"parent_tool_use_id,omitempty"`
		Event           struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Event.Type != "content_block_delta" {
		return
	}
	if env.ParentToolUseID != nil {
		return
	}
	// Deltas route through SessionEvents so they survive across stacked
	// turns / post-OnResult emission, same reasoning as OnAssistant text.
	se := b.sessionEvents.Load()
	if se == nil {
		return
	}
	switch env.Event.Delta.Type {
	case "text_delta":
		if env.Event.Delta.Text != "" && se.OnTextDelta != nil {
			se.OnTextDelta(env.Event.Delta.Text)
		}
	case "thinking_delta":
		// Fire on event presence, not content: this model streams thinking with
		// empty plaintext (only the signature), so gating on non-empty text would
		// never light the indicator. renderer.OnThinkingDelta no-ops on empty.
		if se.OnThinkingDelta != nil {
			se.OnThinkingDelta(env.Event.Delta.Thinking)
		}
	}
}
