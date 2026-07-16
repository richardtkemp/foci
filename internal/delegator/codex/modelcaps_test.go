package codex

import (
	"encoding/json"
	"reflect"
	"testing"

	"foci/internal/modelcaps"
)

// TestListModelCapsPaginatesAndEnriches proves model/list follows cursors,
// preserves advertised effort order, prefers the wire model id, and fills
// omitted structural metadata from an exact static registry entry.
func TestListModelCapsPaginatesAndEnriches(t *testing.T) {
	const registered = "claude-opus-4-6"

	var cursors []string
	b := setupMockBackend(t, func(method string, params json.RawMessage, _ int64) (json.RawMessage, error) {
		if method != "model/list" {
			t.Fatalf("method = %q, want model/list", method)
		}
		var p modelListParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Fatalf("params: %v", err)
		}
		if p.IncludeHidden {
			t.Error("includeHidden = true, want false")
		}
		cursors = append(cursors, p.Cursor)
		if p.Cursor == "" {
			return json.RawMessage(`{"data":[{"id":"picker-alias","model":"` + registered + `","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"xhigh"}]}],"nextCursor":"page-2"}`), nil
		}
		return json.RawMessage(`{"data":[{"id":"fallback-id","model":"","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}],"nextCursor":null}`), nil
	})

	got, err := b.listModelCatalogue()
	if err != nil {
		t.Fatalf("listModelCatalogue: %v", err)
	}
	if !reflect.DeepEqual(cursors, []string{"", "page-2"}) {
		t.Errorf("cursors = %v, want [\"\" page-2]", cursors)
	}
	if caps := got.Caps[registered]; caps.ContextWindow != 1000000 || !reflect.DeepEqual(caps.Effort, []string{"low", "xhigh"}) {
		t.Errorf("registered caps = %+v", caps)
	}
	if caps := got.Caps["fallback-id"]; !reflect.DeepEqual(caps.Effort, []string{"medium"}) {
		t.Errorf("fallback-id caps = %+v", caps)
	}
	if !reflect.DeepEqual(got.Models, []string{registered, "fallback-id"}) {
		t.Errorf("models = %v", got.Models)
	}
}

// TestListModelCapsRejectsRepeatedCursor proves a broken app-server cannot
// trap backend startup in an infinite pagination loop.
func TestListModelCapsRejectsRepeatedCursor(t *testing.T) {
	b := setupMockBackend(t, func(string, json.RawMessage, int64) (json.RawMessage, error) {
		return json.RawMessage(`{"data":[],"nextCursor":"same"}`), nil
	})
	if _, err := b.listModelCatalogue(); err == nil {
		t.Fatal("listModelCatalogue succeeded with a repeated cursor")
	}
}

// TestRefreshModelCapsDeliversSnapshot proves the post-initialize refresh calls
// the configured publisher with the completed catalogue.
func TestRefreshModelCapsDeliversSnapshot(t *testing.T) {
	b := setupMockBackend(t, func(string, json.RawMessage, int64) (json.RawMessage, error) {
		return json.RawMessage(`{"data":[{"id":"gpt-live","model":"gpt-live","supportedReasoningEfforts":[]}],"nextCursor":null}`), nil
	})
	called := false
	b.SetOnModelCaps(func(entries map[string]modelcaps.Caps) {
		called = true
		if _, ok := entries["gpt-live"]; !ok {
			t.Errorf("callback entries = %+v", entries)
		}
	})
	if err := b.refreshModelCaps(); err != nil {
		t.Fatalf("refreshModelCaps: %v", err)
	}
	if !called {
		t.Error("model catalogue callback was not called")
	}
	b.mu.Lock()
	models := append([]string(nil), b.catalogueModels...)
	b.mu.Unlock()
	if !reflect.DeepEqual(models, []string{"gpt-live"}) {
		t.Errorf("stored catalogue models = %v", models)
	}
}
