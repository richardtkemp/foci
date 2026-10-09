package opencode

import (
	"context"
	"net/http"
	"testing"
)

// mkProvider builds a providerInfo with the given id and model→context map.
func mkProvider(id string, models map[string]int) providerInfo {
	p := providerInfo{ID: id}
	p.Models = map[string]struct {
		Limit struct {
			Context int `json:"context"`
		} `json:"limit"`
	}{}
	for model, ctx := range models {
		var m struct {
			Limit struct {
				Context int `json:"context"`
			} `json:"limit"`
		}
		m.Limit.Context = ctx
		p.Models[model] = m
	}
	return p
}

func TestLookupModelLimit(t *testing.T) {
	providers := []providerInfo{
		mkProvider("zai-coding-plan", map[string]int{"glm-5.2": 1000000, "glm-5.1": 200000}),
		mkProvider("openrouter", map[string]int{"glm-5.2": 131072}), // same model, different limit
	}

	tests := []struct {
		name       string
		providerID string
		modelID    string
		want       int
	}{
		{"exact provider+model", "zai-coding-plan", "glm-5.2", 1000000},
		{"exact provider other model", "zai-coding-plan", "glm-5.1", 200000},
		{"unknown model", "zai-coding-plan", "glm-9", 0},
		{"unknown provider falls back to model match", "nonexistent", "glm-5.2", 1000000},
		{"empty provider falls back to any model match", "", "glm-5.1", 200000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lookupModelLimit(providers, tt.providerID, tt.modelID)
			if got != tt.want {
				t.Errorf("lookupModelLimit(%q, %q) = %d, want %d", tt.providerID, tt.modelID, got, tt.want)
			}
		})
	}
}

// TestGetContextWindow_ReportsProviderQualifiedModel: GetContextWindow's model
// also becomes the session model (session_meta), so it must carry the provider
// like TurnResult.Model does (#2264).
func TestGetContextWindow_ReportsProviderQualifiedModel(t *testing.T) {
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/providers" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"providers":[{"id":"zai-coding-plan","models":{"glm-5.3":{"limit":{"context":1000000}}}}]}`))
	})
	b.mu.Lock()
	b.lastModel, b.lastProvider = "glm-5.3", "zai-coding-plan"
	b.mu.Unlock()

	wnd, err := b.GetContextWindow(context.Background())
	if err != nil {
		t.Fatalf("GetContextWindow: %v", err)
	}
	if wnd.MaxTokens != 1000000 {
		t.Errorf("MaxTokens = %d, want 1000000", wnd.MaxTokens)
	}
	if wnd.Model != "zai-coding-plan/glm-5.3" {
		t.Errorf("Model = %q, want %q", wnd.Model, "zai-coding-plan/glm-5.3")
	}
}
