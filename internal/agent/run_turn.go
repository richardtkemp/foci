package agent

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/agent/turnevent"
	"foci/internal/platform"
	"foci/internal/turn"
)

// RunTurn executes a single batched turn for sk, using driver for the
// platform-specific per-turn sink (renderer + tool tracker + StreamingSink)
// and the session router (TODO #745) for late-delivery routing.
//
// The agent owns turn execution: ctx metadata composition, sink wiring,
// batch text/attachment collection, RunTurn invocation. The platform's
// only contributions are (a) the per-turn sink via Driver.NewTurnSink and
// (b) the upstream context (cancellation, /stop hook — Stage A leaves
// these in Bot.Drive's wrapper; Stage C migrates them to the per-session
// inbox).
//
// RunTurn is the eventual single source of truth for turn execution; this
// stage extracts it from Bot.Drive without changing observable behaviour.
// driveAndDrainOrphans currently still calls driver.Drive (which calls
// RunTurn internally); Stage B will switch driveAndDrainOrphans to call
// RunTurn directly and delete the Drive method.
func (a *Agent) RunTurn(
	ctx context.Context,
	sk string,
	batch []Envelope,
	steerer turnevent.Steerer,
	router *sessionRouter,
	driver Driver,
) error {
	if len(batch) == 0 || sk == "" {
		return nil
	}
	first := batch[0]

	// Per-turn sink construction is platform-specific (renderer/tracker
	// types vary). Driver.NewTurnSink returns nil sink for envelopes it
	// can't render — typically because Original isn't the platform's
	// expected message type. Skip silently in that case.
	sink, cleanup := driver.NewTurnSink(first)
	if sink == nil {
		return nil
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Wrap with conversation-DB logging using this turn's metadata. The
	// log moved here from the per-turn OnText closure in turn_delegated.go
	// when delivery was migrated to session-scoped SessionEvents (TODO
	// #747) — the closures no longer have ts.Meta/ts.ConvChatID in
	// lexical scope. The wrapper intercepts TextBlock events for logging
	// and forwards everything else unchanged. SessionMeta is built from
	// the first envelope (mirrors how WithTurnMetadata is set below).
	sink = newLoggingSink(sink, a, first.ChatID, &TurnMetadata{
		UserID:   first.UserID,
		Username: first.SenderName,
		ChatID:   first.ChatID,
	}, sk)

	// Register the per-turn streaming sink with the session router for
	// late-delivery routing (TODO #745). In-turn events route through
	// `sink`; events arriving after this turn ends — e.g. ccstream
	// post-OnResult text from a folded steer — fall through to the
	// router's late-delivery fallback.
	dispatchSink := sink
	if router != nil {
		router.Register(sink)
		defer router.Clear()
		dispatchSink = router
	}

	// Per-turn metadata. Trigger names the platform; downstream consumers
	// (logging, conversation DB) discriminate by it.
	platformName := ""
	if conn := driver.Connection(); conn != nil {
		platformName = conn.PlatformName()
	}
	if platformName != "" {
		ctx = WithTrigger(ctx, platformName)
	}
	ctx = WithTurnMetadata(ctx, &TurnMetadata{
		UserID:   first.UserID,
		Username: first.SenderName,
		ChatID:   first.ChatID,
	})
	if !first.ReceivedAt.IsZero() {
		ctx = WithReceivedAt(ctx, first.ReceivedAt)
	}
	// Thread the sender's steer/queue choice through to the turn transport —
	// RunInference dispatches a SteerNever turn like a system turn (never
	// folds, waits for backend idle) and skips the typed-answer intercepts.
	ctx = WithSteerPreference(ctx, first.Steer)

	// Collect texts and attachments across the batch. Group-chat messages
	// gain sender attribution so downstream logs and prompts know who said
	// what when several users share one channel.
	var texts []string
	var allAttachments []platform.Attachment
	for _, env := range batch {
		text := env.Text
		if env.IsGroupChat && env.SenderName != "" {
			text = fmt.Sprintf("[%s] %s", env.SenderName, text)
		} else if env.IsGroupChat && env.UserID != "" {
			text = fmt.Sprintf("[user:%s] %s", env.UserID, text)
		}
		texts = append(texts, text)
		allAttachments = append(allAttachments, env.Attachments...)
	}

	// Route a typed reply that answers a pending foci_ask to the waiting ask
	// instead of running a normal turn. `ask` is async — the asking turn already
	// ended — so the user's typed ("Other") answer arrives here as a fresh
	// inbound message. Gating on RunTurn (the platform-message path) means this
	// never eats system injects (keepalive, reflection, session_notify), which
	// reach HandleMessage directly and bypass this function.
	// A paused ask (via /pause) skips answer-capture: the user's replies fall
	// through to a normal turn while the ask stays pending (buttons still resolve
	// it; /resume restores capture).
	// Platform carve-out (mirrors inbox.go): the native app answers asks via
	// interactive-form frames, so a typed app message is always meant for the
	// agent — never capture it as an answer. platformName is resolved above from
	// driver.Connection(); "" (unknown source) falls through to capture as before.
	// SteerNever carve-out (mirrors inbox.go): an explicitly-queued message is a
	// turn, never an answer.
	if a.AskRouter != nil && a.AskRouter.PendingForSession != nil && len(texts) > 0 && platformName != platformApp &&
		first.Steer != SteerNever {
		if reqID := a.AskRouter.PendingForSession(sk); reqID != "" &&
			!(a.AskRouter.IsPaused != nil && a.AskRouter.IsPaused(sk)) {
			answer := strings.TrimSpace(texts[0])
			if answer != "" {
				a.taggedLog("ask").Debugf("session=%s routing typed text to pending ask req=%s: %q", sk, reqID, answer)
				a.AskRouter.HandleResponse(reqID, answer)
				return nil
			}
		}
	}

	return turn.RunTurn(ctx, a, dispatchSink, steerer, sk, texts, allAttachments)
}
