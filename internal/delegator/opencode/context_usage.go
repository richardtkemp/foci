// context_usage.go — ContextWindowQuerier implementation for opencode.
//
// opencode has no CC-style get_context_usage endpoint, but it exposes the
// model catalogue (sourced from models.dev) via GET /config/providers, which
// carries each model's real context window (limit.context). foci otherwise
// falls back to a generic 200k for unregistered models (modelinfo) — wrong by
// up to 5× for models like zai-coding-plan/glm-5.2 (real window 1,000,000),
// which made compaction fire at ~8% of true capacity. We query the real
// window here so foci's compaction trigger uses the correct limit.
//
// Only the window SIZE is returned here. The current token count comes from
// api.db (QuerySessionStats) — not from in-memory backend state, which is
// cleared at turn boundaries and unreliable between sessions/restarts.

package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"foci/internal/delegator"
)

// providerInfo is the subset of opencode's /config/providers provider shape
// we need: the provider id and each model's context-window limit.
type providerInfo struct {
	ID     string `json:"id"`
	Models map[string]struct {
		Limit struct {
			Context int `json:"context"`
		} `json:"limit"`
	} `json:"models"`
}

// Capabilities advertises opencode's limitations: no mid-turn message
// injection (HTTP/SSE-based, no stdin pipe). Post-tool and pre-answer
// nudges are silently unsupported.
func (b *Backend) Capabilities() delegator.Capabilities {
	return delegator.CapabilitiesForBackend("opencode")
}

// StatusDetail returns empty — opencode has no permission-mode concept.
func (b *Backend) StatusDetail() string { return "" }

// GetContextWindow implements delegator.ContextWindowQuerier. Returns the
// model's real context window from /config/providers (cached per model).
func (b *Backend) GetContextWindow(ctx context.Context) (*delegator.ContextWindow, error) {
	b.mu.Lock()
	model := b.lastModel
	provider := b.lastProvider
	b.mu.Unlock()

	if model == "" {
		return nil, fmt.Errorf("opencode: no model captured yet (no completed turn)")
	}

	limit, err := b.modelContextLimit(ctx, provider, model)
	if err != nil {
		return nil, err
	}

	return &delegator.ContextWindow{
		MaxTokens: limit,
		Model:     model,
	}, nil
}

// modelContextLimit returns the context window for providerID/modelID from
// opencode's GET /config/providers (models.dev-sourced, refreshed on opencode
// startup). The result is cached on the Backend keyed by model — the window
// is static for a given model, so we query at most once per model.
func (b *Backend) modelContextLimit(ctx context.Context, providerID, modelID string) (int, error) {
	b.mu.Lock()
	if b.ctxLimitCache > 0 && b.ctxLimitModel == modelID {
		lim := b.ctxLimitCache
		b.mu.Unlock()
		return lim, nil
	}
	b.mu.Unlock()

	url := b.server.baseURL + "/config/providers"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET /config/providers: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET /config/providers: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var payload struct {
		Providers []providerInfo `json:"providers"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("decode /config/providers: %w", err)
	}

	limit := lookupModelLimit(payload.Providers, providerID, modelID)
	if limit == 0 {
		return 0, fmt.Errorf("opencode: no context limit for %s/%s in /config/providers", providerID, modelID)
	}

	b.mu.Lock()
	b.ctxLimitCache = limit
	b.ctxLimitModel = modelID
	b.mu.Unlock()
	return limit, nil
}

// lookupModelLimit finds modelID's context window, preferring an exact
// provider match and falling back to any provider that lists the model
// (handles an empty providerID).
func lookupModelLimit(providers []providerInfo, providerID, modelID string) int {
	if providerID != "" {
		for _, p := range providers {
			if p.ID != providerID {
				continue
			}
			if m, ok := p.Models[modelID]; ok && m.Limit.Context > 0 {
				return m.Limit.Context
			}
		}
	}
	for _, p := range providers {
		if m, ok := p.Models[modelID]; ok && m.Limit.Context > 0 {
			return m.Limit.Context
		}
	}
	return 0
}
