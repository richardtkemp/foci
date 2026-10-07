package command

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/provider"
	"foci/internal/session"
)

// overridesStubClient is a provider.Client with a name, so two values compare
// unequal — the tests must tell the root's client from the agent default.
type overridesStubClient struct {
	name string
}

func (overridesStubClient) SendMessage(context.Context, *provider.MessageRequest) (*provider.MessageResponse, error) {
	return nil, fmt.Errorf("stub: not implemented")
}

func (overridesStubClient) CountTokens(context.Context, *provider.MessageRequest) (int, error) {
	return 0, nil
}

func (overridesStubClient) IsCachingAvailable() bool { return false }

// TestOverridesDeleteModelEndpointOnChildKeepsModelDropsInheritedTuple proves
// /overrides delete model_endpoint on a child behaves exactly as on a root:
// the child keeps the model it had, and its format and client become the
// agent defaults — no leg silently re-inherits the root's tuple.
func TestOverridesDeleteModelEndpointOnChildKeepsModelDropsInheritedTuple(t *testing.T) {
	defaultClient := overridesStubClient{name: "default"}
	ag := &agent.Agent{
		Model:  "claude-opus-4-8",
		Format: "anthropic",
		Client: defaultClient,
	}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()
	ag.SetSessionModel(root.String(), "google/gemini-2.5-pro", "gemini", "gemini", overridesStubClient{name: "root"})
	ag.SetSessionModel(branch.String(), "openai/gpt-5.6", "", "", nil)

	resp, err := deleteOverride(branch.String(), modelCC(ag), "model_endpoint")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "cleared") {
		t.Errorf("deleteOverride response = %q, want a cleared confirmation", resp.Text)
	}

	if got := ag.SessionModel(branch.String()); got != "openai/gpt-5.6" {
		t.Errorf("branch model after delete model_endpoint = %q, want kept model %q", got, "openai/gpt-5.6")
	}
	if got := ag.SessionFormat(branch.String()); got != "anthropic" {
		t.Errorf("branch format after delete model_endpoint = %q, want agent default %q", got, "anthropic")
	}
	if got := ag.SessionClient(branch.String()); got != defaultClient {
		t.Errorf("branch client after delete model_endpoint = %v, want agent default client, not the root's", got)
	}
}

// TestOverridesDeleteModelFormatOnChildKeepsModelDropsInheritedTuple proves
// /overrides delete model_format on a child behaves exactly as on a root: the
// child keeps the model it had, and its format and client become the agent
// defaults — no leg silently re-inherits the root's tuple.
func TestOverridesDeleteModelFormatOnChildKeepsModelDropsInheritedTuple(t *testing.T) {
	defaultClient := overridesStubClient{name: "default"}
	ag := &agent.Agent{
		Model:  "claude-opus-4-8",
		Format: "anthropic",
		Client: defaultClient,
	}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()
	ag.SetSessionModel(root.String(), "google/gemini-2.5-pro", "gemini", "gemini", overridesStubClient{name: "root"})
	ag.SetSessionModel(branch.String(), "openai/gpt-5.6", "", "", nil)

	resp, err := deleteOverride(branch.String(), modelCC(ag), "model_format")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "cleared") {
		t.Errorf("deleteOverride response = %q, want a cleared confirmation", resp.Text)
	}

	if got := ag.SessionModel(branch.String()); got != "openai/gpt-5.6" {
		t.Errorf("branch model after delete model_format = %q, want kept model %q", got, "openai/gpt-5.6")
	}
	if got := ag.SessionFormat(branch.String()); got != "anthropic" {
		t.Errorf("branch format after delete model_format = %q, want agent default %q", got, "anthropic")
	}
	if got := ag.SessionClient(branch.String()); got != defaultClient {
		t.Errorf("branch client after delete model_format = %v, want agent default client, not the root's", got)
	}
}
