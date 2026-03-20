package turn

import "foci/internal/display"

// TurnDisplay holds resolved display settings for a single turn.
// Resolved once at turn start to avoid repeated override lookups.
type TurnDisplay struct {
	ShowToolCalls string
	ShowThinking  string
	StreamOutput  bool
	DisplayWidth  int
	MaxChars      int                // Platform message limit: 4096 (Telegram), 2000 (Discord)
	RenderOpts    display.RenderOpts
}
