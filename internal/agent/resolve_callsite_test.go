package agent

import (
	"testing"

	"foci/internal/config"
	"foci/internal/session"
)

// TestResolveCallSiteSessionPathsReturnSessionFormat proves the session paths
// of ResolveCallSite (nil GroupResolver, and an ungrouped call site under a
// non-nil resolver) return the triple of ONE session tuple — client, model
// AND format — so a session owning an openai tuple never gets its openai
// client paired with the agent-default format.
func TestResolveCallSiteSessionPathsReturnSessionFormat(t *testing.T) {
	defaultClient := tupleClient{name: "default"}
	clientX := tupleClient{name: "openai"}
	own := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := own.Branch()

	newAgent := func(resolver *config.GroupResolver) *Agent {
		return &Agent{
			Model:         "claude-opus-4-8",
			Endpoint:      "anthropic",
			Format:        "anthropic",
			Client:        defaultClient,
			GroupResolver: resolver,
		}
	}
	// A non-nil resolver with no groups: ResolveCall on an ungrouped site
	// (keepalive is ungrouped by defaultCallGroups) returns nil, exercising
	// the fall-through session path.
	ungroupedResolver := config.NewGroupResolver(config.GroupsConfig{}, map[string]config.ModelConfig{}, true)

	cases := []struct {
		name       string
		resolver   *config.GroupResolver
		sk         string
		wantClient tupleClient
		wantModel  string
		wantFormat string
	}{
		{"own tuple, nil resolver", nil, own.String(), clientX, "openai/gpt-5.6", "openai"},
		{"own tuple, ungrouped call site", ungroupedResolver, own.String(), clientX, "openai/gpt-5.6", "openai"},
		{"model-less branch of that root", ungroupedResolver, branch.String(), clientX, "openai/gpt-5.6", "openai"},
		{"no override", ungroupedResolver, "bot/c300", defaultClient, "claude-opus-4-8", "anthropic"},
	}
	for _, tc := range cases {
		ag := newAgent(tc.resolver)
		ag.SetSessionModel(own.String(), "openai/gpt-5.6", "openai", "openai", clientX)

		client, model, format := ag.ResolveCallSite(config.CallKeepalive, tc.sk)
		if client != tc.wantClient {
			t.Errorf("%s: client = %v, want %v", tc.name, client, tc.wantClient)
		}
		if model != tc.wantModel {
			t.Errorf("%s: model = %q, want %q", tc.name, model, tc.wantModel)
		}
		if format != tc.wantFormat {
			t.Errorf("%s: format = %q, want %q", tc.name, format, tc.wantFormat)
		}
	}
}
