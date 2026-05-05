package agent

import (
	"context"
	"fmt"

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

	// Register the per-turn streaming sink with the session router for
	// late-delivery routing (TODO #745). In-turn events route through
	// `sink`; events arriving after this turn ends — e.g. ccstream
	// rearm-counter responses — fall through to the router's
	// late-delivery fallback.
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

	return turn.RunTurn(ctx, a, dispatchSink, steerer, sk, texts, allAttachments)
}
