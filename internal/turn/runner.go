package turn

import (
	"context"

	"foci/internal/agent/turnevent"
	"foci/internal/platform"
)

// RunTurn attaches sink (and optionally steerer) to ctx and delegates to the
// handler. It's the single shared wiring point for interactive platforms
// (telegram, discord) and any caller that builds a sink themselves — the
// caller owns sink/steerer construction and any platform-specific context
// decoration (WithTrigger, WithTurnMetadata) that should happen before the
// turn runs.
//
// steerer may be nil when the caller does not support mid-turn steering
// (HTTP handlers, injected wakes, hook-driven turns).
func RunTurn(
	ctx context.Context,
	handler platform.MessageHandler,
	sink turnevent.Sink,
	steerer turnevent.Steerer,
	sessionKey string,
	texts []string,
	attachments []platform.Attachment,
) error {
	ctx = turnevent.WithSink(ctx, sink)
	if steerer != nil {
		ctx = turnevent.WithSteerer(ctx, steerer)
	}
	return handler.HandleMessage(ctx, sessionKey, texts, attachments)
}
