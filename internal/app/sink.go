package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"foci/internal/agent"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/ratelimit"
	"foci/internal/skills"
	"foci/internal/toolformat"
	"foci/internal/turn"
	"foci/internal/turnevent"
	"foci/internal/voice"
)

// appSink is the app provider's per-turn turnevent.Sink. It is a thin wrapper
// around the shared turn.StreamingSink (the same delivery coordination Telegram
// and Discord use): all text streaming, intermediate-vs-final dedup, and
// per-segment stream reset are handled by StreamingSink + TurnRenderer driving
// an appBackend (see render.go). The wrapper adds only what is genuinely
// app-specific and has no home in the platform renderer:
//
//   - the agent activity indicator, driven off the turn boundary as structured
//     fap.Activity frames (the renderer's SendTyping is a no-op for the app);
//   - the structured fap.Meta frame on turn completion (model/cost/tokens —
//     the typed replacement for the [meta] text blob the Telegram bridge injects);
//   - tool.call transcript frames when show_tool_calls is preview/full (#999,
//     toolcalls.go). The renderer's own tool display stays off: it renders
//     Telegram-style text, and the app gets structured frames instead.
//
// One appSink per turn on one conversation.
type appSink struct {
	b       *convBinding
	inner   *turn.StreamingSink
	backend *appBackend

	// cleanup finishes the renderer's stream buffer (stops the pump goroutine).
	// Returned from NewTurnSink for the agent to defer, so an abandoned turn
	// (no TurnComplete) doesn't leak the pump.
	cleanup func()

	// statusFn supplies the meta-frame gap chip.
	// nil = that field is omitted (e.g. a sink with no agent context).
	statusFn func() string

	// cacheExpiryFn returns the prompt-cache expiry (unix ms) as of now, pushed on
	// turn completion. nil = no agent context, so the frame is skipped.
	cacheExpiryFn func() int64

	// compactionLimitFn returns the session's current auto-compaction token
	// threshold (agent.Agent.CompactionLimitTokens), pushed on turn completion
	// as fap.Meta.CompactionLimitTokens. nil = no agent context, so the field
	// is omitted (0).
	compactionLimitFn func() int64

	// skillScopeFn returns the agent's live skill scope, which decides whether
	// a read is a "Loaded skill" chip and whether the app can open it (#2392).
	// nil = no agent context: the root-less SKILL.md rule only, nothing to open.
	skillScopeFn func() skills.ReadScope

	// tts is the agent's voice.TTS provider (#1439), resolved per call from the
	// live [[tts]] voices like the send_to_chat voice-note path. nil, or one
	// that is not voice.Configured, means no TTS: voice-mode bundling is
	// skipped and the turn degrades to a text-only reply — no error.
	tts voice.TTS

	// attachVoice uploads synthesized reply audio and emits it as a
	// VoiceMode-tagged fap.Media frame on this turn's binding, naming the
	// text message it reads aloud (textMessageID, #2985; "" = none was
	// delivered). Set by appConn.NewTurnSink (which alone holds the hub's
	// blob store); nil-safe (checked before use) so a sink built directly in
	// a test without it still behaves like "no TTS".
	attachVoice func(audio []byte, textMessageID string) error

	// voiceBlockDelivered is set once an intermediate TextBlock has been
	// delivered as a bubble this turn, spoken or not (#1444; "or not" since
	// #3047, whose persistent voice mode can start mid-turn). It gates the
	// TurnComplete synthesis: a backend (e.g. ccstream) may accumulate
	// FinalText as the concatenation of EVERY assistant message across
	// tool-call boundaries, even though each one was already delivered — and
	// spoken — as its own app bubble via an intermediate TextBlock. Without
	// this gate, TurnComplete would re-synthesize that whole accumulation as
	// one clip, reading past the bubble it belongs to and into the next.
	voiceBlockDelivered bool

	// voiceModeOn reports, at each bubble, whether a device has this chat in
	// persistent voice mode (#3047): then every bubble is spoken, whatever the
	// turn's trigger. nil = never (a sink built without a hub).
	voiceModeOn func() bool

	// speechText prepares a bubble's text for its clip (#3050): each diagram,
	// table and code block becomes a fast-model sentence or its marker
	// (Hub.voiceSpeechText). nil = markers only (a sink built without a hub).
	speechText func(ctx context.Context, text string) string

	// toolCalls sends this turn's tool calls as tool.call transcript frames
	// (#999). nil = the session's resolved show_tool_calls is off (the app
	// default): only the transient activity frame is sent, as before #999.
	// Set by appConn.NewTurnSink, which resolves the gate once per turn.
	toolCalls *toolCallEmitter

	// afterTurn runs at the end of cleanup, once every frame of the turn is
	// sent: appConn.NewTurnSink points it at Hub.finishTurnRead, which applies
	// a close_session mark_read deferred to this turn (#2592). nil = none.
	afterTurn func()

	// info measures the turn for the app's "Message info" (#2992) and, on
	// TurnComplete, records it against every message the turn delivered
	// (b.store). Same goroutine as Emit, so no lock.
	info turnInfoMeter
}

// newAppSink builds the per-turn app sink: an appBackend (turn.Platform) wrapped
// by a TurnRenderer and StreamingSink, all driving FAP frames on the binding.
// Typing is owned by appSink (conn passed as nil to StreamingSink), so the
// indicator tracks the turn boundary as structured frames.
func newAppSink(b *convBinding) *appSink {
	backend := newAppBackend(b)
	d := turn.TurnDisplay{StreamOutput: true, ShowThinking: "off", ShowToolCalls: "off"}
	tracker := noopTracker{}
	newSB := func() *turn.StreamBuffer {
		return turn.NewStreamBuffer(backend.OpenStream(), appStreamInterval, d.StreamOutput)
	}
	renderer := turn.NewTurnRenderer(backend, tracker, d, newSB)
	inner := turn.NewStreamingSink(renderer, tracker, nil)
	s := &appSink{b: b, inner: inner, backend: backend}
	// A turn abandoned without TurnComplete would strand the indicator; cleanup is
	// deferred by the agent on every turn, so clearing the turn-scoped activity
	// here is the backstop. Session-scoped states (subagents/waiting) are NOT
	// cleared here — they outlive the turn by design.
	s.cleanup = func() {
		b.setTurnActivity(fap.ActivityKindIdle)
		renderer.Cleanup()
		if s.afterTurn != nil {
			s.afterTurn()
		}
	}
	return s
}

// DeliversToPlatform implements turnevent.Sink — output is always user-facing.
func (s *appSink) DeliversToPlatform() bool { return true }

// Emit implements turnevent.Sink. It forwards to the shared StreamingSink for all
// text coordination and layers on the app-specific typing + meta frames.
func (s *appSink) Emit(ctx context.Context, ev turnevent.Event) {
	s.info.observe(ev, time.Now())
	switch e := ev.(type) {
	case turnevent.TurnStart:
		// A fresh turn means this conversation's caller is active again — clear any
		// session-scoped "waiting on another agent" state before the turn opens.
		s.b.setWaitingDetail("")
		s.b.setTurnActivity(fap.ActivityKindWarming)
		s.inner.Emit(ctx, ev)

	case turnevent.SubagentText:
		// Route to the renderer: OnSubagentReply sends it as a distinct
		// fap.SubagentText frame (the app is a raw SubagentDeliverer, #1067),
		// NOT through OnReply, so it delivers the subagent's content to its chip
		// without fragmenting the in-flight main reply stream.
		s.inner.Emit(ctx, ev)

	case turnevent.ThinkingDelta, turnevent.ThinkingBlock:
		s.b.setTurnActivity(fap.ActivityKindThinking)
		s.inner.Emit(ctx, ev)

	case turnevent.ToolCall:
		s.b.setTurnTool(fap.ActivityKindTool, toolformat.ActivityLabel(e.Name, e.Args), toolformat.FullCommand(e.Name, e.Args), toolformat.IsShellTool(e.Name))
		// A read of a skill file leaves the same "Loaded skill" line in the
		// transcript that Telegram and Discord show (#1299, #2335). The app has no
		// compact tool lines, so it travels as an ambient notification: stored and
		// replayed like any system row, but never a push or a list preview. A file
		// under one of the agent's skill roots also carries SkillURL, so the app
		// can open its current text (#2392).
		if n, ok := s.skillLoadNotification(e); ok {
			s.b.send(n)
		}
		if s.toolCalls != nil {
			s.toolCalls.call(e)
		}
		s.inner.Emit(ctx, ev)

	case turnevent.ToolResult:
		// Tool finished; the model is processing its result with no output token
		// yet — back to the "warming" (working) state until the next event.
		s.b.setTurnActivity(fap.ActivityKindWarming)
		if s.toolCalls != nil {
			s.toolCalls.result(e)
		}
		s.inner.Emit(ctx, ev)

	case turnevent.TextDelta, turnevent.TextBlock:
		s.b.setTurnActivity(fap.ActivityKindTyping)
		var audio []byte
		if tb, ok := ev.(turnevent.TextBlock); ok && tb.Phase == turnevent.PhaseIntermediate {
			// Voice-mode per-block synthesis (#1444): speak THIS block before
			// forwarding it — same "synthesize before delivery" ordering as
			// TurnComplete below — so the resulting clip covers exactly the
			// bubble this block is about to render as (renderer.OnReply), one
			// clip per delivered app bubble rather than one clip per turn.
			audio = s.synthesizeVoiceModeBlock(ctx, tb.Text)
			if platform.StripSilencingSuffix(platform.StripSpuriousPrefix(tb.Text)) != "" {
				// Delivered as a bubble, spoken or not. Persistent voice
				// mode can start mid-turn (#3047): the turn-end FinalText
				// repeats this bubble, so it must not be spoken then.
				s.voiceBlockDelivered = true
			}
		}
		s.backend.lastMsgID = ""
		s.inner.Emit(ctx, ev)
		// A note the renderer did not deliver (silent block) must not leak
		// onto a later, unrelated bubble.
		s.backend.voiceUnavailable = ""
		// The clip follows the bubble it reads, naming it (#2985).
		s.attachVoiceClip(audio)

	case turnevent.BackendRetry:
		// Claude Code is waiting to retry the main thread's API call (#2260):
		// shown until the next content or tool event, or the turn's end, clears
		// it (setTurnTool). Heartbeats and subagent events do not clear it.
		s.b.setRetrying(retryDetail(e.Status, e.Attempt, e.Delay))

	case turnevent.TurnComplete:
		// A final text that is foci's notice (e.g. "Max tool call depth
		// reached.") is not the agent's reply (#3005): the renderer sees an
		// empty final text, and the notice goes out as a system message after
		// it — also when the turn already streamed a reply, since it reports
		// how the turn ended rather than repeating the reply.
		notice := ""
		if e.FinalNotice {
			notice = platform.StripSilencingSuffix(platform.StripSpuriousPrefix(e.FinalText))
			e.FinalText = ""
			ev = e
		}
		// Voice-mode bundling (#1439): synthesize BEFORE forwarding so the reply
		// waits on synthesis — text and its voice-mode attachment land together,
		// not text now / audio as a separate follow-up once TTS finishes.
		audio := s.synthesizeVoiceMode(ctx, e)
		// Forward first so the final text is delivered (TextEnd / ServerMessage),
		// then close out the turn-scoped activity (→ idle) and emit the meta frame.
		s.backend.lastMsgID = ""
		s.inner.Emit(ctx, ev)
		s.backend.voiceUnavailable = ""
		s.attachVoiceClip(audio)
		if notice != "" {
			s.b.send(fap.ServerMessage{ConversationID: s.b.convID, MessageID: fap.NewULID(), Role: "system", Text: notice})
		}
		s.b.setTurnActivity(fap.ActivityKindIdle)
		s.emitMeta(e)
		if s.cacheExpiryFn != nil {
			s.b.setCacheExpiry(s.cacheExpiryFn())
		}
		s.recordMessageInfo(e)

	default:
		s.inner.Emit(ctx, ev)
	}
}

// ttsFailed handles a voice-mode TTS synthesis failure: it logs it and stages
// a short, user-facing reason for the bubble about to be delivered text-only
// (#1809), which appBackend.Deliver carries as the frame's VoiceUnavailable.
//
// A rate limit (e.g. Groq's per-day token cap) is an expected transient: the
// turn already delivered its text and only the spoken clip is missing, so log
// it at info — keeping it OFF the WARN-triggered warning-injection queue (no
// noisy client-facing notice, no raw body / billing URL). Every other failure
// stays a WARN. The client-facing reason likewise never carries the raw error:
// only the rate-limit wait, or a generic "TTS provider error".
func (s *appSink) ttsFailed(err error) {
	var tooLong *voice.TextTooLongError
	if errors.As(err, &tooLong) {
		appLog.Infof("voice-mode: TTS %v — delivered text-only (conv=%s)", tooLong, s.b.convID)
		s.backend.voiceUnavailable = "text too long"
		return
	}
	var rl *ratelimit.Error
	if errors.As(err, &rl) {
		appLog.Infof("voice-mode: TTS %v — delivered text-only (conv=%s)", rl, s.b.convID)
		s.backend.voiceUnavailable = "rate limited"
		if rl.RetryAfter > 0 {
			s.backend.voiceUnavailable += " for another " + rl.RetryAfter.Round(time.Second).String()
		}
		return
	}
	appLog.Warnf("voice-mode: TTS synthesis (conv=%s): %v", s.b.convID, err)
	s.backend.voiceUnavailable = "TTS provider error"
}

// synthesizeVoiceMode returns synthesized reply audio for a voice-triggered
// app turn (#1439), or nil when bundling doesn't apply: no TTS configured
// (graceful text-only degradation), the turn errored, the turn is not spoken
// (speaks: neither a voice turn nor a chat in persistent voice mode, #3047 —
// so a normal typed app turn never gets an unsolicited voice note), or the reply has no real content to speak (cleaned the same way the
// renderer cleans delivered text — an empty/fully-silent, e.g. [[NO_RESPONSE]],
// reply is never synthesized). Text is run through voice.NormalizeForSpeech
// (#1444) before synthesis so markdown/symbol-heavy tokens (a "#1443" ticket
// ref, an em-dash, a "path/like/slash") don't mangle the TTS model's output.
// A synthesis error is logged and swallowed — same graceful degrade as
// no-TTS-configured, never fails the turn.
func (s *appSink) synthesizeVoiceMode(ctx context.Context, e turnevent.TurnComplete) []byte {
	if !voice.Configured(s.tts) || e.Err != nil {
		return nil
	}
	if !s.speaks(ctx) {
		return nil
	}
	if s.voiceBlockDelivered {
		// #1444: one or more intermediate TextBlocks were already
		// synthesized-and-attached individually this turn. FinalText at this
		// point is the backend's whole-turn accumulation (spans tool-call
		// boundaries) and would duplicate/read past what per-block synthesis
		// already spoke — there is nothing new left to say.
		return nil
	}
	text := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(e.FinalText))
	if text == "" {
		return nil
	}
	audio, err := s.synthesize(ctx, text)
	if err != nil {
		s.ttsFailed(err)
		return nil
	}
	return audio
}

// speaks reports whether this bubble is spoken: the turn is a voice turn
// (#1439: trigger "voice", the reply to spoken input), or a device has the
// chat in persistent voice mode right now (#3047), which speaks every agent
// message, background turns included.
func (s *appSink) speaks(ctx context.Context) bool {
	return agent.TriggerFromContext(ctx) == "voice" || (s.voiceModeOn != nil && s.voiceModeOn())
}

// synthesize speaks text through s.tts, its diagrams, tables and code blocks
// first replaced by a spoken sentence or marker (speechText, #3050), noting which voice spoke and how long
// it took for the clip's Message info (#2992); attachVoiceClip files the note
// under the message the clip reads.
func (s *appSink) synthesize(ctx context.Context, text string) ([]byte, error) {
	if s.speechText != nil {
		text = s.speechText(ctx, text)
	} else {
		text = voice.SpeakBlocks(ctx, text, nil, 0)
	}
	var served string
	start := time.Now()
	audio, err := s.tts.Synthesize(voice.WithServedRecorder(ctx, func(id string) { served = id }), voice.NormalizeForSpeech(text))
	if err == nil {
		s.info.pendingVoice = &messageVoiceRecord{provider: served, ms: time.Since(start).Milliseconds()}
	}
	return audio, err
}

// attachVoiceClip sends synthesized audio (nil = nothing to send) as the
// voice-mode clip of the text segment the event just forwarded delivered:
// backend.lastMsgID, which the caller cleared before forwarding (#2985). When
// that event delivered nothing the clip goes out unlinked, as before #2985.
func (s *appSink) attachVoiceClip(audio []byte) {
	pending := s.info.pendingVoice
	s.info.pendingVoice = nil
	if len(audio) == 0 || s.attachVoice == nil {
		return
	}
	if err := s.attachVoice(audio, s.backend.lastMsgID); err != nil {
		appLog.Warnf("voice-mode: attach synthesized audio (conv=%s): %v", s.b.convID, err)
		return
	}
	if pending != nil && s.backend.lastMsgID != "" {
		if s.info.voice == nil {
			s.info.voice = map[string]messageVoiceRecord{}
		}
		s.info.voice[s.backend.lastMsgID] = *pending
	}
}

// synthesizeVoiceModeBlock synthesizes the voice clip for ONE intermediate
// text block about to be delivered (#1444) — the per-bubble counterpart to
// synthesizeVoiceMode. Same gating as synthesizeVoiceMode (TTS configured,
// speaks, non-silent after stripping), minus the e.Err check (a mid-turn
// TextBlock never carries a turn error). The caller marks voiceBlockDelivered
// for every delivered block, spoken or not, so the terminal TurnComplete does
// not also speak the whole-turn accumulation this block is part of. Text is run through
// voice.NormalizeForSpeech (#1444) before synthesis, same as
// synthesizeVoiceMode. A synthesis error is logged and swallowed, matching
// synthesizeVoiceMode's graceful degrade. The caller attaches the returned
// audio after the block is delivered, so the clip can name its bubble (#2985).
func (s *appSink) synthesizeVoiceModeBlock(ctx context.Context, rawText string) []byte {
	if !voice.Configured(s.tts) || !s.speaks(ctx) {
		return nil
	}
	text := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(rawText))
	if text == "" {
		return nil
	}
	audio, err := s.synthesize(ctx, text)
	if err != nil {
		s.ttsFailed(err)
		return nil
	}
	return audio
}

// emitMeta sends the user-facing status chips (model, cost, tokens) the app
// renders in the conversation header — the structured replacement for the
// [meta] text blob the Telegram bridge injects.
func (s *appSink) emitMeta(e turnevent.TurnComplete) {
	meta := fap.Meta{ConversationID: s.b.convID, Model: e.Model}
	if e.Cost > 0 {
		cost := e.Cost
		meta.PrevCostUsd = &cost
	}
	if e.Usage != nil {
		meta.Tokens = &fap.Tokens{
			In:  int64(e.Usage.InputTokens),
			Out: int64(e.Usage.OutputTokens),
			CR:  int64(e.Usage.CacheReadInputTokens),
			CW:  int64(e.Usage.CacheCreationInputTokens),
		}
	}
	if s.statusFn != nil {
		meta.Gap = s.statusFn()
	}
	if s.compactionLimitFn != nil {
		meta.CompactionLimitTokens = s.compactionLimitFn()
	}
	if meta.Model == "" && meta.PrevCostUsd == nil && meta.Tokens == nil && meta.Gap == "" && meta.CompactionLimitTokens == 0 {
		return
	}
	s.b.send(meta)
}

// skillLoadNotification returns the "Loaded skill" chip for a tool call that
// reads a skill file (toolformat.LoadedSkill), or false for any other call
// (including unparseable args). One chip per read: the same file read twice
// is two chips.
func (s *appSink) skillLoadNotification(e turnevent.ToolCall) (fap.Notification, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(e.Args, &m); err != nil {
		return fap.Notification{}, false
	}
	var scope skills.ReadScope
	if s.skillScopeFn != nil {
		scope = s.skillScopeFn()
	}
	line, ref, openable := toolformat.LoadedSkill(e.Name, m, scope)
	if line == "" {
		return fap.Notification{}, false
	}
	n := fap.Notification{ConversationID: s.b.convID, MessageID: fap.NewULID(), Text: line, Level: "info", Ambient: true}
	if openable {
		n.SkillURL = skillFileURL(s.b.agent(), ref)
	}
	return n, true
}
