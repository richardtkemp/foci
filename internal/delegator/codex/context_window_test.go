package codex

import (
	"context"
	"testing"
)

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
			want: "gpt-5.6-sol",
		},
		{
			name: "active model takes precedence over launch model",
			backend: &Backend{
				model:       "gpt-5.6-sol",
				launchModel: "gpt-5.6-luna",
			},
			want: "gpt-5.6-sol",
		},
		{
			name: "launch model is the fallback",
			backend: &Backend{
				launchModel: "gpt-5.6-luna",
			},
			want: "gpt-5.6-luna",
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
