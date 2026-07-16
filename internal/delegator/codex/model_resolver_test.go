package codex

import (
	"context"
	"testing"

	"foci/internal/delegator"
)

// TestResolveCatalogueModel proves exact ids win and substring aliases choose
// the numerically newest matching version while preserving catalogue order for
// equal versions.
func TestResolveCatalogueModel(t *testing.T) {
	catalogue := []string{
		"gpt-5.9-sol",
		"gpt-5.12-luna",
		"gpt-5.12-terra",
		"gpt-5.6-luna",
	}
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{name: "exact older id", query: "gpt-5.6-luna", want: "gpt-5.6-luna"},
		{name: "qualified exact id", query: "codex/GPT-5.6-LUNA", want: "gpt-5.6-luna"},
		{name: "family alias", query: "luna", want: "gpt-5.12-luna"},
		{name: "broad alias highest number", query: "gpt", want: "gpt-5.12-luna"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveCatalogueModel(tt.query, catalogue)
			if err != nil {
				t.Fatalf("resolveCatalogueModel: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolved = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveCatalogueModelRejectsMissingAlias proves unknown aliases are not
// passed through to app-server as invalid model ids.
func TestResolveCatalogueModelRejectsMissingAlias(t *testing.T) {
	if _, err := resolveCatalogueModel("moon", []string{"gpt-5.6-luna"}); err == nil {
		t.Fatal("unknown alias resolved without error")
	}
}

// TestBackendResolveModelReturnsCanonicalIDs proves the optional delegator
// resolver returns separate wire and foci-tracking model identifiers.
func TestBackendResolveModelReturnsCanonicalIDs(t *testing.T) {
	b := &Backend{catalogueModels: []string{"gpt-5.6-luna"}}
	got, err := b.ResolveModel(context.Background(), "luna")
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	want := delegator.ModelResolution{BackendModel: "gpt-5.6-luna", Model: "codex/gpt-5.6-luna"}
	if got != want {
		t.Errorf("resolution = %+v, want %+v", got, want)
	}
}

// TestPrepareConfiguredModelHandlesFreshAndResumedThreads proves the config
// alias is sent in thread/start for new sessions and queued for turn/start when
// resuming an existing thread.
func TestPrepareConfiguredModelHandlesFreshAndResumedThreads(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		b := &Backend{
			startOpts:       delegator.StartOptions{Model: "luna"},
			catalogueModels: []string{"gpt-5.7-luna", "gpt-5.6-luna"},
		}
		if err := b.prepareConfiguredModel(context.Background(), resumed); err != nil {
			t.Fatalf("resumed=%v: %v", resumed, err)
		}
		if got := b.modelFromOpts(); got != "gpt-5.7-luna" {
			t.Errorf("resumed=%v launch model = %q", resumed, got)
		}
		if resumed && b.pendingModel != "gpt-5.7-luna" {
			t.Errorf("resumed pending model = %q", b.pendingModel)
		}
		if !resumed && b.pendingModel != "" {
			t.Errorf("fresh pending model = %q, want empty", b.pendingModel)
		}
	}
}
