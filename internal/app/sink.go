package app

import (
	"context"
	"errors"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/platform"
	"foci/internal/ratelimit"
	"foci/internal/turn"
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
//     the typed replacement for the [meta] text blob the Telegram bridge injects).
//
// One appSink per turn on one conversation.
type appSink struct {
	b     *convBinding
	inner *turn.StreamingSink

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

	// tts is the agent's resolved voice.TTS provider (#1439), reused from the
	// same voice.TTS/ResolveTTS/VoiceConfig machinery telegram's outbound
	// voice-note path uses. nil = no TTS configured, so voice-mode bundling is
	// skipped and the turn degrades to a text-only reply — no error.
	tts voice.TTS

	// attachVoice uploads synthesized reply audio and emits it as a
	// VoiceMode-tagged fap.Media frame on this turn's binding. Set by
	// appConn.NewTurnSink (which alone holds the hub's blob store); nil-safe
	// (checked before use) so a sink built directly in a test without it still
	// behaves like "no TTS".
	attachVoice func(audio []byte) error

	// voiceBlockDelivered is set once a voice clip has been synthesized for a
	// delivered intermediate TextBlock this turn (#1444). It gates the
	// TurnComplete synthesis: a backend (e.g. ccstream) may accumulate
	// FinalText as the concatenation of EVERY assistant message across
	// tool-call boundaries, even though each one was already delivered — and
	// spoken — as its own app bubble via an intermediate TextBlock. Without
	// this gate, TurnComplete would re-synthesize that whole accumulation as
	// one clip, reading past the bubble it belongs to and into the next.
	voiceBlockDelivered bool
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
	s := &appSink{b: b, inner: inner}
	// A turn abandoned without TurnComplete would strand the indicator; cleanup is
	// deferred by the agent on every turn, so clearing the turn-scoped activity
	// here is the backstop. Session-scoped states (subagents/waiting) are NOT
	// cleared here — they outlive the turn by design.
	s.cleanup = func() {
		b.setTurnActivity(fap.ActivityKindIdle, "")
		renderer.Cleanup()
	}
	return s
}

// DeliversToPlatform implements turnevent.Sink — output is always user-facing.
func (s *appSink) DeliversToPlatform() bool { return true }

// Emit implements turnevent.Sink. It forwards to the shared StreamingSink for all
// text coordination and layers on the app-specific typing + meta frames.
func (s *appSink) Emit(ctx context.Context, ev turnevent.Event) {
	switch e := ev.(type) {
	case turnevent.TurnStart:
		// A fresh turn means this conversation's caller is active again — clear any
		// session-scoped "waiting on another agent" state before the turn opens.
		s.b.setWaitingDetail("")
		s.b.setTurnActivity(fap.ActivityKindWarming, "")
		s.inner.Emit(ctx, ev)

	case turnevent.SubagentText:
		// Route to the renderer: OnSubagentReply sends it as a distinct
		// fap.SubagentText frame (the app is a raw SubagentDeliverer, #1067),
		// NOT through OnReply, so it delivers the subagent's content to its chip
		// without fragmenting the in-flight main reply stream.
		s.inner.Emit(ctx, ev)

	case turnevent.ThinkingDelta, turnevent.ThinkingBlock:
		s.b.setTurnActivity(fap.ActivityKindThinking, "")
		s.inner.Emit(ctx, ev)

	case turnevent.ToolCall:
		s.b.setTurnActivity(fap.ActivityKindTool, e.Name)
		s.inner.Emit(ctx, ev)

	case turnevent.ToolResult:
		// Tool finished; the model is processing its result with no output token
		// yet — back to the "warming" (working) state until the next event.
		s.b.setTurnActivity(fap.ActivityKindWarming, "")
		s.inner.Emit(ctx, ev)

	case turnevent.TextDelta, turnevent.TextBlock:
		s.b.setTurnActivity(fap.ActivityKindTyping, "")
		if tb, ok := ev.(turnevent.TextBlock); ok && tb.Phase == turnevent.PhaseIntermediate {
			// Voice-mode per-block synthesis (#1444): speak THIS block before
			// forwarding it — same "synthesize before delivery" ordering as
			// TurnComplete below — so the resulting clip covers exactly the
			// bubble this block is about to render as (renderer.OnReply), one
			// clip per delivered app bubble rather than one clip per turn.
			s.synthesizeVoiceModeBlock(ctx, tb.Text)
		}
		s.inner.Emit(ctx, ev)

	case turnevent.TurnComplete:
		// Voice-mode bundling (#1439): synthesize BEFORE forwarding so the reply
		// waits on synthesis — text and its voice-mode attachment land together,
		// not text now / audio as a separate follow-up once TTS finishes.
		audio := s.synthesizeVoiceMode(ctx, e)
		// Forward first so the final text is delivered (TextEnd / ServerMessage),
		// then close out the turn-scoped activity (→ idle) and emit the meta frame.
		s.inner.Emit(ctx, ev)
		if len(audio) > 0 && s.attachVoice != nil {
			if err := s.attachVoice(audio); err != nil {
				appLog.Warnf("voice-mode: attach synthesized audio (conv=%s): %v", s.b.convID, err)
			}
		}
		s.b.setTurnActivity(fap.ActivityKindIdle, "")
		s.emitMeta(e)
		if s.cacheExpiryFn != nil {
			s.b.setCacheExpiry(s.cacheExpiryFn())
		}

	default:
		s.inner.Emit(ctx, ev)
	}
}

// logTTSError logs a voice-mode TTS synthesis failure. A rate limit (e.g. Groq's
// per-day token cap) is an expected transient: the turn already delivered its text
// and only the spoken clip is missing, so log it at info — keeping it OFF the
// WARN-triggered warning-injection queue (no noisy client-facing notice, no raw
// body / billing URL). Every other failure stays a WARN.
func (s *appSink) logTTSError(err error) {
	var rl *ratelimit.Error
	if errors.As(err, &rl) {
		appLog.Infof("voice-mode: TTS %v — delivered text-only (conv=%s)", rl, s.b.convID)
		return
	}
	appLog.Warnf("voice-mode: TTS synthesis (conv=%s): %v", s.b.convID, err)
}

// synthesizeVoiceMode returns synthesized reply audio for a voice-triggered
// app turn (#1439), or nil when bundling doesn't apply: no TTS configured
// (graceful text-only degradation), the turn errored, the turn's trigger isn't
// "voice" (gated so a normal typed app turn never gets an unsolicited voice
// note), or the reply has no real content to speak (cleaned the same way the
// renderer cleans delivered text — an empty/fully-silent, e.g. [[NO_RESPONSE]],
// reply is never synthesized). Text is run through voice.NormalizeForSpeech
// (#1444) before synthesis so markdown/symbol-heavy tokens (a "#1443" ticket
// ref, an em-dash, a "path/like/slash") don't mangle the TTS model's output.
// A synthesis error is logged and swallowed — same graceful degrade as
// no-TTS-configured, never fails the turn.
func (s *appSink) synthesizeVoiceMode(ctx context.Context, e turnevent.TurnComplete) []byte {
	if s.tts == nil || e.Err != nil {
		return nil
	}
	if agent.TriggerFromContext(ctx) != "voice" {
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
	audio, err := s.tts.Synthesize(ctx, voice.NormalizeForSpeech(text))
	if err != nil {
		s.logTTSError(err)
		return nil
	}
	return audio
}

// synthesizeVoiceModeBlock synthesizes and attaches a voice clip for ONE
// delivered intermediate text block (#1444) — the per-bubble counterpart to
// synthesizeVoiceMode. Same gating as synthesizeVoiceMode (TTS configured,
// trigger=="voice", non-silent after stripping), minus the e.Err check (a
// mid-turn TextBlock never carries a turn error). Marks voiceBlockDelivered
// so the terminal TurnComplete does not also speak the whole-turn
// accumulation this block is part of. Text is run through
// voice.NormalizeForSpeech (#1444) before synthesis, same as
// synthesizeVoiceMode. A synthesis error is logged and swallowed, matching
// synthesizeVoiceMode's graceful degrade.
func (s *appSink) synthesizeVoiceModeBlock(ctx context.Context, rawText string) {
	if s.tts == nil || agent.TriggerFromContext(ctx) != "voice" {
		return
	}
	text := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(rawText))
	if text == "" {
		return
	}
	s.voiceBlockDelivered = true
	audio, err := s.tts.Synthesize(ctx, voice.NormalizeForSpeech(text))
	if err != nil {
		s.logTTSError(err)
		return
	}
	if len(audio) > 0 && s.attachVoice != nil {
		if err := s.attachVoice(audio); err != nil {
			appLog.Warnf("voice-mode: attach synthesized audio (conv=%s): %v", s.b.convID, err)
		}
	}
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
