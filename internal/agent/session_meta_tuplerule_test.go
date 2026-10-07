package agent

import (
	"path/filepath"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
)

// tupleClient is a stubClient with a name, so two values compare unequal —
// stubClient alone is zero-size and cannot tell the root's client from the
// agent default.
type tupleClient struct {
	stubClient
	name string
}

// tupleClientProvider hands out one fixed client per endpoint:format pair, so
// restore-path tests can prove which session's client was really rebuilt.
type tupleClientProvider struct {
	clients map[string]provider.Client
}

func (p tupleClientProvider) GetClient(endpoint, format string) provider.Client {
	return p.clients[endpoint+"/"+format]
}

func (p tupleClientProvider) PeekClient(endpoint, format string) provider.Client {
	return p.clients[endpoint+"/"+format]
}

func (p tupleClientProvider) ResolveEndpointClient(endpoint, format string) provider.Client {
	return p.clients[endpoint+"/"+format]
}

// TestBranchOwnModelDoesNotInheritRootTupleLegs proves the one-owner rule: a
// branch with its OWN model but no endpoint/format/client of its own (the
// exact shape /model's unresolved-name path writes) must NOT take the root's
// endpoint, format or client — those legs fall to the agent defaults, so the
// child's model name is never sent through the root's provider.
func TestBranchOwnModelDoesNotInheritRootTupleLegs(t *testing.T) {
	defaultClient := tupleClient{name: "default"}
	ag := &Agent{
		Model:  "claude-opus-4-8",
		Format: "anthropic",
		Client: defaultClient,
	}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()
	rootKey := root.String()
	branchKey := branch.String()

	ag.SetSessionModel(rootKey, "google/gemini-2.5-pro", "gemini", "gemini", tupleClient{name: "root"})
	ag.SetSessionModel(branchKey, "openai/gpt-5.6", "", "", nil)

	if got := ag.SessionModel(branchKey); got != "openai/gpt-5.6" {
		t.Errorf("branch model = %q, want own override %q", got, "openai/gpt-5.6")
	}
	if got := ag.SessionClient(branchKey); got != defaultClient {
		t.Errorf("branch client = %v, want agent default client — an own model must not inherit the root's client", got)
	}
	if got := ag.SessionFormat(branchKey); got != "anthropic" {
		t.Errorf("branch format = %q, want agent default %q", got, "anthropic")
	}
	if got := ag.resolveEndpoint(branchKey); got != ag.Endpoint {
		t.Errorf("branch endpoint = %q, want agent default %q", got, ag.Endpoint)
	}
}

// TestBranchOwnModelOwnFormatNilClientKeepsOwnFormatGetsDefaultClient
// proves the partial-own-tuple edge of the one-owner rule: a branch with
// its own model AND its own format but a nil client (the /model path when
// the agent has no ClientProvider to build one) keeps its OWN format and
// gets the agent default client — the root's client never fills a leg of
// a tuple the branch already owns.
func TestBranchOwnModelOwnFormatNilClientKeepsOwnFormatGetsDefaultClient(t *testing.T) {
	defaultClient := tupleClient{name: "default"}
	ag := &Agent{
		Model:   "claude-opus-4-8",
		Format:  "anthropic",
		Endpoint: "anthropic",
		Client:  defaultClient,
	}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()
	rootKey := root.String()
	branchKey := branch.String()

	ag.SetSessionModel(rootKey, "google/gemini-2.5-pro", "gemini", "gemini", tupleClient{name: "root"})
	ag.SetSessionModel(branchKey, "openai/gpt-5.6", "", "openai", nil)

	if got := ag.SessionModel(branchKey); got != "openai/gpt-5.6" {
		t.Errorf("branch model = %q, want own override %q", got, "openai/gpt-5.6")
	}
	if got := ag.SessionFormat(branchKey); got != "openai" {
		t.Errorf("branch format = %q, want own format %q", got, "openai")
	}
	if got := ag.SessionClient(branchKey); got != defaultClient {
		t.Errorf("branch client = %v, want agent default client, not the root's", got)
	}
	if got := ag.resolveEndpoint(branchKey); got != "anthropic" {
		t.Errorf("branch endpoint = %q, want agent default %q", got, "anthropic")
	}
}

// TestIndependentChildOwnModelDoesNotInheritRootTupleLegs proves the
// one-owner rule covers independent (`i`) children too — rootKeyIfChild
// treats branch `b` and independent `i` children identically.
func TestIndependentChildOwnModelDoesNotInheritRootTupleLegs(t *testing.T) {
	defaultClient := tupleClient{name: "default"}
	ag := &Agent{
		Model:  "claude-opus-4-8",
		Format: "anthropic",
		Client: defaultClient,
	}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	indep := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100", ChildType: 'i', ChildTS: 1709596800}
	rootKey := root.String()
	indepKey := indep.String()

	ag.SetSessionModel(rootKey, "google/gemini-2.5-pro", "gemini", "gemini", tupleClient{name: "root"})
	ag.SetSessionModel(indepKey, "openai/gpt-5.6", "", "", nil)

	if got := ag.SessionModel(indepKey); got != "openai/gpt-5.6" {
		t.Errorf("independent child model = %q, want own override %q", got, "openai/gpt-5.6")
	}
	if got := ag.SessionClient(indepKey); got != defaultClient {
		t.Errorf("independent child client = %v, want agent default client", got)
	}
	if got := ag.SessionFormat(indepKey); got != "anthropic" {
		t.Errorf("independent child format = %q, want agent default %q", got, "anthropic")
	}
}

// TestTupleOwnerEmptyLegsFallToAgentDefaults pins the empty-leg half of the
// one-owner rule: an owner (here the root, inherited by a model-less branch)
// whose own model is set but whose endpoint/format/client are empty resolves
// those legs to the agent defaults, never to another session.
func TestTupleOwnerEmptyLegsFallToAgentDefaults(t *testing.T) {
	defaultClient := tupleClient{name: "default"}
	ag := &Agent{
		Model:    "claude-opus-4-8",
		Format:   "anthropic",
		Endpoint: "anthropic",
		Client:   defaultClient,
	}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()

	ag.SetSessionModel(root.String(), "claude-fable-1", "", "", nil)

	if got := ag.SessionModel(branch.String()); got != "claude-fable-1" {
		t.Errorf("branch model = %q, want inherited root model %q", got, "claude-fable-1")
	}
	if got := ag.SessionFormat(branch.String()); got != "anthropic" {
		t.Errorf("branch format = %q, want agent default %q", got, "anthropic")
	}
	if got := ag.SessionClient(branch.String()); got != defaultClient {
		t.Errorf("branch client = %v, want agent default client", got)
	}
	if got := ag.resolveEndpoint(branch.String()); got != "anthropic" {
		t.Errorf("branch endpoint = %q, want agent default %q", got, "anthropic")
	}
}

// TestBranchInheritsRootWholeTupleIncludingEndpoint proves the endpoint leg of
// tuple inheritance: a branch with no own model gates on the ROOT's endpoint —
// the endpoint its inherited client actually sends traffic to — not the agent
// default.
func TestBranchInheritsRootWholeTupleIncludingEndpoint(t *testing.T) {
	ag := &Agent{
		Model:    "claude-opus-4-8",
		Endpoint: "anthropic",
	}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()

	ag.SetSessionModel(root.String(), "google/gemini-2.5-pro", "gemini", "gemini", tupleClient{name: "root"})

	if got := ag.resolveEndpoint(branch.String()); got != "gemini" {
		t.Errorf("branch endpoint = %q, want inherited root endpoint %q", got, "gemini")
	}
	if got := ag.resolveEndpoint(root.String()); got != "gemini" {
		t.Errorf("root endpoint = %q, want own override %q", got, "gemini")
	}
}

// TestSyntheticOwnerModelYieldsAgentDefaultTuple proves the sentinel never
// leaks a mixed tuple: an owner whose model is SyntheticModel resolves every
// leg — model, format AND client — to the agent defaults, whether the owner
// is the session itself or an inherited root.
func TestSyntheticOwnerModelYieldsAgentDefaultTuple(t *testing.T) {
	defaultClient := tupleClient{name: "default"}
	ag := &Agent{
		Model:  "claude-opus-4-8",
		Format: "anthropic",
		Client: defaultClient,
	}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()
	own := session.SessionKey{AgentID: "bot", Type: 'c', ID: "200"}

	// Pre-guard pollution shapes: the sentinel sat next to real legs.
	ag.setMetaLocked(root.String(), func(sm *sessionMeta) {
		sm.model = SyntheticModel
		sm.modelEndpoint = "gemini"
		sm.modelFormat = "gemini"
		sm.client = tupleClient{name: "root"}
	})
	ag.setMetaLocked(own.String(), func(sm *sessionMeta) {
		sm.model = SyntheticModel
		sm.modelEndpoint = "gemini"
		sm.modelFormat = "gemini"
		sm.client = tupleClient{name: "own"}
	})

	for _, sk := range []string{root.String(), branch.String(), own.String()} {
		if got := ag.SessionModel(sk); got != "claude-opus-4-8" {
			t.Errorf("SessionModel(%s) = %q, want agent default %q", sk, got, "claude-opus-4-8")
		}
		if got := ag.SessionFormat(sk); got != "anthropic" {
			t.Errorf("SessionFormat(%s) = %q, want agent default %q", sk, got, "anthropic")
		}
		if got := ag.SessionClient(sk); got != defaultClient {
			t.Errorf("SessionClient(%s) = %v, want agent default client", sk, got)
		}
	}
}

// TestSetSessionModelClearDeletesEndpointAndFormatRows proves that clearing
// the endpoint/format legs of a model override (what /overrides delete
// model_endpoint / model_format does) also deletes their persisted rows, so
// the clear survives a restart instead of being resurrected by
// RestoreSessionOverrides from a stale row.
func TestSetSessionModelClearDeletesEndpointAndFormatRows(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ag := &Agent{Model: "claude-opus-4-8", Format: "anthropic", SessionIndex: idx}
	ag.SetSessionModel("bot/c100", "google/gemini-2.5-pro", "gemini", "gemini", nil)
	// The clear shape overrideKeyMap uses: keep the model, empty the legs.
	ag.SetSessionModel("bot/c100", ag.SessionModel("bot/c100"), "", "", nil)

	overrides := ag.SessionOverrides("bot/c100")
	if _, ok := overrides[session.MetaKeyModelEndpoint]; ok {
		t.Errorf("overrides after clear still list %s = %q", session.MetaKeyModelEndpoint, overrides[session.MetaKeyModelEndpoint])
	}
	if _, ok := overrides[session.MetaKeyModelFormat]; ok {
		t.Errorf("overrides after clear still list %s = %q", session.MetaKeyModelFormat, overrides[session.MetaKeyModelFormat])
	}

	// A fresh agent restoring from the same index must not resurrect them.
	fresh := &Agent{Model: "claude-opus-4-8", Format: "anthropic", SessionIndex: idx}
	fresh.RestoreSessionOverrides("bot/c100")
	restored := fresh.SessionOverrides("bot/c100")
	if _, ok := restored[session.MetaKeyModelEndpoint]; ok {
		t.Errorf("restore resurrected %s = %q", session.MetaKeyModelEndpoint, restored[session.MetaKeyModelEndpoint])
	}
	if _, ok := restored[session.MetaKeyModelFormat]; ok {
		t.Errorf("restore resurrected %s = %q", session.MetaKeyModelFormat, restored[session.MetaKeyModelFormat])
	}
	if got := fresh.SessionModel("bot/c100"); got != "google/gemini-2.5-pro" {
		t.Errorf("model after restore = %q, want %q", got, "google/gemini-2.5-pro")
	}
	if got := fresh.SessionFormat("bot/c100"); got != "anthropic" {
		t.Errorf("format after restore = %q, want agent default %q", got, "anthropic")
	}
}

// TestRestoreChildModelOnlyAfterRestartUsesAgentDefaults proves the restart
// leg of the one-owner rule: a child whose persisted overrides hold a model
// but no endpoint/format resolves, after RestoreSessionOverrides, to the
// agent-default format and client — even when its root's full tuple (client
// included) was restored and rebuilt just before.
func TestRestoreChildModelOnlyAfterRestartUsesAgentDefaults(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()

	// The "old" process: root on a full foreign-provider tuple, child
	// model-only — the shape /model's unresolved path leaves behind.
	old := &Agent{Model: "claude-opus-4-8", Format: "anthropic", SessionIndex: idx}
	old.SetSessionModel(root.String(), "google/gemini-2.5-pro", "gemini", "gemini", nil)
	old.SetSessionModel(branch.String(), "openai/gpt-5.6", "", "", nil)

	// The "new" process: restore both. The root's client is genuinely
	// rebuilt through the provider; the child must not pick it up.
	rootClient := tupleClient{name: "root"}
	defaultClient := tupleClient{name: "default"}
	fresh := &Agent{
		Model:          "claude-opus-4-8",
		Format:         "anthropic",
		Client:         defaultClient,
		ClientProvider: tupleClientProvider{clients: map[string]provider.Client{"gemini/gemini": rootClient}},
		SessionIndex:   idx,
	}
	fresh.RestoreSessionOverrides(root.String())
	fresh.RestoreSessionOverrides(branch.String())

	if got := fresh.SessionClient(root.String()); got != rootClient {
		t.Fatalf("sanity: root client = %v, want the restored root client", got)
	}
	if got := fresh.SessionModel(branch.String()); got != "openai/gpt-5.6" {
		t.Errorf("branch model after restore = %q, want own override %q", got, "openai/gpt-5.6")
	}
	if got := fresh.SessionFormat(branch.String()); got != "anthropic" {
		t.Errorf("branch format after restore = %q, want agent default %q", got, "anthropic")
	}
	if got := fresh.SessionClient(branch.String()); got != defaultClient {
		t.Errorf("branch client after restore = %v, want agent default client, not the root's restored client", got)
	}
}
