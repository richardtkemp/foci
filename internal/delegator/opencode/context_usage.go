// context_usage.go — ContextUsageQuerier implementation for opencode.
//
// opencode has no CC-style get_context_usage endpoint, but it exposes the
// model catalogue (sourced from models.dev) via GET /config/providers, which
// carries each model's real context window (limit.context). foci otherwise
// falls back to a generic 200k for unregistered models (modelinfo) — wrong by
// up to 5× for models like zai-coding-plan/glm-5.2 (real window 1,000,000),
// which made compaction fire at ~8% of true capacity. We query the real
// window here so foci's compaction trigger uses the correct limit.

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

// GetContextUsage implements delegator.ContextUsageQuerier. MaxTokens is the
// model's real context window (from /config/providers); TotalTokens is the
// last completed turn's context size. Both come from already-captured state
// plus one cached HTTP GET — no LLM call.
func (b *Backend) GetContextUsage(ctx context.Context) (*delegator.ContextUsage, error) {
	b.mu.Lock()
	model := b.lastModel
	provider := b.lastProvider
	usage := b.lastUsage
	b.mu.Unlock()

	if model == "" {
		return nil, fmt.Errorf("opencode: no model captured yet (no completed turn)")
	}

	limit, err := b.modelContextLimit(ctx, provider, model)
	if err != nil {
		return nil, err
	}

	total := 0
	if usage != nil {
		total = usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	}
	pct := 0
	if limit > 0 {
		pct = int(float64(total) / float64(limit) * 100)
	}

	return &delegator.ContextUsage{
		TotalTokens: total,
		MaxTokens:   limit,
		Percentage:  pct,
		Model:       model,
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
