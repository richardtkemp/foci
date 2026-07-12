package agent

import (
	"testing"

	"foci/internal/session"
)

// TestBranchInheritsRootModelTuple proves a branch session with no own model
// override resolves to the ROOT session's live model tuple (model/endpoint/
// format/client), not the agent default — so the branch launches on the same
// model the root is live on and reuses the root's per-model prompt cache.
func TestBranchInheritsRootModelTuple(t *testing.T) {
	ag := &Agent{Model: "claude-opus-4-8", Format: "anthropic"}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()
	rootKey := root.String()
	branchKey := branch.String()

	client := stubClient{}
	ag.SetSessionModel(rootKey, "claude-fable-1", "https://fable.example", "openai", client)

	if got := ag.SessionModel(branchKey); got != "claude-fable-1" {
		t.Errorf("branch model = %q, want inherited root model %q", got, "claude-fable-1")
	}
	if got := ag.SessionFormat(branchKey); got != "openai" {
		t.Errorf("branch format = %q, want inherited root format %q", got, "openai")
	}
	if got := ag.SessionClient(branchKey); got != client {
		t.Errorf("branch client = %v, want inherited root client", got)
	}
}

// TestBranchOwnModelWins proves a branch with its own explicit model override
// keeps that override rather than inheriting root's.
func TestBranchOwnModelWins(t *testing.T) {
	ag := &Agent{Model: "claude-opus-4-8"}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()

	ag.SetSessionModel(root.String(), "claude-fable-1", "", "", nil)
	ag.SetSessionModel(branch.String(), "claude-sonnet-4-6", "", "", nil)

	if got := ag.SessionModel(branch.String()); got != "claude-sonnet-4-6" {
		t.Errorf("branch model = %q, want own override %q", got, "claude-sonnet-4-6")
	}
}

// TestBranchFallsToAgentDefaultWhenRootUnset proves that when neither the branch
// nor the root has a model override, resolution reaches the agent default.
func TestBranchFallsToAgentDefaultWhenRootUnset(t *testing.T) {
	ag := &Agent{Model: "claude-opus-4-8"}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()

	if got := ag.SessionModel(branch.String()); got != "claude-opus-4-8" {
		t.Errorf("branch model = %q, want agent default %q", got, "claude-opus-4-8")
	}
}

// TestRootModelUnaffectedByFallback proves the root session itself never walks
// (it is its own root) and simply resolves own override → agent default.
func TestRootModelUnaffectedByFallback(t *testing.T) {
	ag := &Agent{Model: "claude-opus-4-8"}
	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}

	if got := ag.SessionModel(root.String()); got != "claude-opus-4-8" {
		t.Errorf("root model = %q, want agent default %q", got, "claude-opus-4-8")
	}
	ag.SetSessionModel(root.String(), "claude-fable-1", "", "", nil)
	if got := ag.SessionModel(root.String()); got != "claude-fable-1" {
		t.Errorf("root model = %q, want own override %q", got, "claude-fable-1")
	}
}
