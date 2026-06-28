// plan.go — opencode's /plan delivery. Registered via
// delegator.RegisterPlan("opencode", planDelivery) in init().
//
// opencode has a built-in `plan` agent (see Config.agent.plan in the
// opencode docs). The SDK's session.prompt endpoint accepts an `agent`
// field in the request body, so we pass agent:"plan" per-request
// WITHOUT changing server-wide config — no swap-back needed. The next
// prompt without the agent field uses the default agent automatically.
//
// This is simpler than the plan's original sketch (PATCH /config then
// swap back via OnTurnComplete) because the per-request agent field
// eliminates the global state mutation.

package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"foci/internal/delegator"
)

// planDelivery implements delegator.PlanDelivery for the opencode backend.
// Sends the user's plan request via POST /prompt_async with agent:"plan"
// in the body so opencode uses the plan agent for this turn.
func planDelivery(ctx context.Context, deps delegator.PlanDeps, args string) (string, error) {
	be, err := deps.Backend()
	if err != nil {
		return "", fmt.Errorf("/plan: %w", err)
	}
	b, ok := be.(*Backend)
	if !ok {
		return "", fmt.Errorf("/plan: backend is %T, not *opencode.Backend", be)
	}

	// Send the plan request with agent:"plan". No beginTurn — the
	// plan delivery is fire-and-forget; the response arrives via
	// SessionEvents.OnText (the user sees it in the chat). Post-turn
	// bookkeeping doesn't fire for plan turns in v1, same limitation
	// as steerBuf follow-up turns (Step 7).
	if err := b.sendPromptWithAgent(ctx, args, "plan"); err != nil {
		return "", fmt.Errorf("/plan: %w", err)
	}
	return "📋 Planning…", nil
}

// sendPromptWithAgent is sendPrompt with an `agent` field in the body.
// Used by planDelivery to select the "plan" agent for a single request
// without changing server-wide config. Empty agent = use the default.
func (b *Backend) sendPromptWithAgent(ctx context.Context, text, agent string) error {
	if b.server == nil || b.sessionID == "" {
		return fmt.Errorf("opencode: no session")
	}
	body := buildPromptBodyWithAgent(text, nil, false, agent)
	return b.postMessage(ctx, "/prompt_async", body)
}

// buildPromptBodyWithAgent extends buildPromptBody with an optional
// `agent` field. When non-empty, opencode uses the named agent for
// this request only.
func buildPromptBodyWithAgent(text string, attachments []delegator.Attachment, noReply bool, agent string) []byte {
	type part struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
		Mime string `json:"mime,omitempty"`
		URL  string `json:"url,omitempty"`
	}
	parts := make([]part, 0, 1+len(attachments))
	parts = append(parts, part{Type: "text", Text: text})
	for _, att := range attachments {
		dataURL := "data:" + att.MimeType + ";base64," + base64.StdEncoding.EncodeToString(att.Data)
		parts = append(parts, part{Type: "file", Mime: att.MimeType, URL: dataURL})
	}
	body := map[string]any{"parts": parts}
	if noReply {
		body["noReply"] = true
	}
	if agent != "" {
		body["agent"] = agent
	}
	out, _ := json.Marshal(body)
	return out
}
