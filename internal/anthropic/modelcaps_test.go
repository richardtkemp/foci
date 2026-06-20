package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"foci/internal/modelcaps"
)

// catalogueJSON mirrors the real /v1/models shape: an opus with the full five
// effort levels + adaptive thinking, and a haiku with no effort/thinking.
const catalogueJSON = `{
	"data": [
		{
			"type": "model", "id": "claude-opus-4-8-20260528",
			"max_input_tokens": 1000000, "max_tokens": 128000,
			"capabilities": {
				"effort": {
					"supported": true,
					"low": {"supported": true}, "medium": {"supported": true},
					"high": {"supported": true}, "xhigh": {"supported": true},
					"max": {"supported": true}
				},
				"thinking": {"supported": true, "types": {"adaptive": {"supported": true}, "enabled": {"supported": false}}}
			}
		},
		{
			"type": "model", "id": "claude-haiku-4-5-20251001",
			"max_input_tokens": 200000, "max_tokens": 64000,
			"capabilities": {
				"effort": {"supported": false},
				"thinking": {"supported": false}
			}
		}
	],
	"has_more": false
}`

func TestFetchModelCaps(t *testing.T) {
	// Proves FetchModelCaps parses the live catalogue into Caps keyed by the
	// normalized (date-stripped) model id, with effort levels in canonical
	// order and unsupported capabilities collapsing to nil.
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q, want oauth beta", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, catalogueJSON)
	})

	caps, err := client.FetchModelCaps(context.Background())
	if err != nil {
		t.Fatalf("FetchModelCaps: %v", err)
	}

	opus, ok := caps["claude-opus-4-8"] // date suffix stripped by Normalize
	if !ok {
		t.Fatalf("missing claude-opus-4-8; got keys %v", keysOf(caps))
	}
	if opus.ContextWindow != 1000000 || opus.MaxOutput != 128000 {
		t.Errorf("opus ctx/out = %d/%d, want 1000000/128000", opus.ContextWindow, opus.MaxOutput)
	}
	if want := []string{"low", "medium", "high", "xhigh", "max"}; !reflect.DeepEqual(opus.Effort, want) {
		t.Errorf("opus effort = %v, want %v", opus.Effort, want)
	}
	if want := []string{"adaptive"}; !reflect.DeepEqual(opus.Thinking, want) {
		t.Errorf("opus thinking = %v, want %v (enabled is unsupported)", opus.Thinking, want)
	}

	haiku, ok := caps["claude-haiku-4-5"]
	if !ok {
		t.Fatalf("missing claude-haiku-4-5")
	}
	if haiku.Effort != nil || haiku.Thinking != nil {
		t.Errorf("haiku effort/thinking = %v/%v, want nil/nil", haiku.Effort, haiku.Thinking)
	}
	if haiku.ContextWindow != 200000 {
		t.Errorf("haiku ctx = %d, want 200000", haiku.ContextWindow)
	}
}

func TestFetchModelCapsError(t *testing.T) {
	// Proves a non-200 surfaces as an error rather than an empty map.
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"type":"error"}`)
	})
	if _, err := client.FetchModelCaps(context.Background()); err == nil {
		t.Error("want error on 500, got nil")
	}
}

func keysOf(m map[string]modelcaps.Caps) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
