package codex

import (
	"context"
	"testing"

	"foci/internal/delegator"
)

// TestGetContextWindowReportsEffectiveModel also pins the spelling: the model
// carries the same "codex/" prefix as TurnResult.Model, since the agent writes
// both into the session's model field (#1645).
func TestGetContextWindowReportsEffectiveModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend *Backend
		want    string
	}{
		{
			name: "pending override takes precedence",
			backend: &Backend{
				model:        "gpt-5.6-luna",
				pendingModel: "gpt-5.6-sol",
			},
			want: "codex/gpt-5.6-sol",
		},
		{
			name: "active model takes precedence over launch model",
			backend: &Backend{
				model:       "gpt-5.6-sol",
				launchModel: "gpt-5.6-luna",
			},
			want: "codex/gpt-5.6-sol",
		},
		{
			name: "launch model is the fallback",
			backend: &Backend{
				launchModel: "gpt-5.6-luna",
			},
			want: "codex/gpt-5.6-luna",
		},
		{
			// The launch request is SessionModel, which may already carry
			// the prefix; it must not be doubled.
			name: "already-prefixed request is not prefixed again",
			backend: &Backend{
				startOpts: delegator.StartOptions{Model: "codex/gpt-5.6-sol"},
			},
			want: "codex/gpt-5.6-sol",
		},
		{
			name:    "no model known stays empty, not a bare prefix",
			backend: &Backend{},
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tt.backend.GetContextWindow(context.Background())
			if err != nil {
				t.Fatalf("GetContextWindow: %v", err)
			}
			if got.Model != tt.want {
				t.Errorf("Model = %q, want %q", got.Model, tt.want)
			}
		})
	}
}
