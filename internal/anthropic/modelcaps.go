package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"foci/internal/modelcaps"
	"foci/internal/modelinfo"
)

// FetchModelCaps GETs the full /v1/models catalogue and extracts each model's
// live capabilities (context window, max output, effort levels, thinking modes)
// into modelcaps.Caps, keyed by the bare (normalized) model id.
//
// This bypasses the SDK's typed ModelInfo (which keeps only id+created_at) with
// a raw request, because the capability/token fields are not modelled by the
// SDK struct. Mirrors the raw-GET pattern in usage.go. Intended to be wired as
// the modelcaps.Fetcher at startup.
func (c *Client) FetchModelCaps(ctx context.Context) (map[string]modelcaps.Caps, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/v1/models?limit=100", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (status %d): %s", httpResp.StatusCode, string(respBody))
	}

	var raw modelsResponse
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	out := make(map[string]modelcaps.Caps, len(raw.Data))
	for _, m := range raw.Data {
		out[modelinfo.Normalize(m.ID)] = modelcaps.Caps{
			ContextWindow: m.MaxInputTokens,
			MaxOutput:     m.MaxTokens,
			Effort:        m.Capabilities.Effort.levels(),
			Thinking:      m.Capabilities.Thinking.modes(),
		}
	}
	return out, nil
}

// modelsResponse mirrors the subset of /v1/models we consume.
type modelsResponse struct {
	Data []modelObject `json:"data"`
}

type modelObject struct {
	ID             string `json:"id"`
	MaxInputTokens int    `json:"max_input_tokens"`
	MaxTokens      int    `json:"max_tokens"`
	Capabilities   struct {
		Effort   effortCap   `json:"effort"`
		Thinking thinkingCap `json:"thinking"`
	} `json:"capabilities"`
}

// capFlag is the common {"supported": bool} shape used throughout capabilities.
type capFlag struct {
	Supported bool `json:"supported"`
}

// effortCap carries the per-level supported flags. Explicit fields (rather than
// a map) so we control the output order deterministically.
type effortCap struct {
	Supported bool    `json:"supported"`
	Low       capFlag `json:"low"`
	Medium    capFlag `json:"medium"`
	High      capFlag `json:"high"`
	XHigh     capFlag `json:"xhigh"`
	Max       capFlag `json:"max"`
}

// levels returns the supported effort levels in canonical ascending order, or
// nil if effort is unsupported.
func (e effortCap) levels() []string {
	if !e.Supported {
		return nil
	}
	var out []string
	for _, l := range []struct {
		name string
		flag capFlag
	}{
		{"low", e.Low}, {"medium", e.Medium}, {"high", e.High},
		{"xhigh", e.XHigh}, {"max", e.Max},
	} {
		if l.flag.Supported {
			out = append(out, l.name)
		}
	}
	return out
}

type thinkingCap struct {
	Supported bool `json:"supported"`
	Types     struct {
		Adaptive capFlag `json:"adaptive"`
		Enabled  capFlag `json:"enabled"`
	} `json:"types"`
}

// modes returns the supported thinking modes in canonical order, or nil if
// thinking is unsupported.
func (t thinkingCap) modes() []string {
	if !t.Supported {
		return nil
	}
	var out []string
	if t.Types.Adaptive.Supported {
		out = append(out, "adaptive")
	}
	if t.Types.Enabled.Supported {
		out = append(out, "enabled")
	}
	return out
}
