package command

import (
	"context"
	"fmt"
	"path/filepath"
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
	ag.SetSessionModel(branch.String(), "openai/gpt-5.6", "openai", "openai", overridesStubClient{name: "branch"})

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
	ag.SetSessionModel(branch.String(), "openai/gpt-5.6", "openai", "openai", overridesStubClient{name: "branch"})

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

// TestOverridesDeleteUnsetKeyRepliesNotSet proves deleting an override that is
// not set changes nothing: every key in overrideKeyMap on a clean session
// replies exactly the not-set message and runs no clear function (the session
// stays override-free), and an unknown key keeps its distinct reply.
func TestOverridesDeleteUnsetKeyRepliesNotSet(t *testing.T) {
	ag := &agent.Agent{
		Model:  "claude-opus-4-8",
		Format: "anthropic",
		Client: overridesStubClient{name: "default"},
	}
	root := "bot/c100"

	for key := range overrideKeyMap {
		resp, err := deleteOverride(root, modelCC(ag), key)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("Override %q is not set.", key); resp.Text != want {
			t.Errorf("delete %q on clean session = %q, want %q", key, resp.Text, want)
		}
		if overrides := ag.SessionOverrides(root); len(overrides) != 0 {
			t.Errorf("delete %q on clean session left overrides %v, want none", key, overrides)
		}
	}

	resp, err := deleteOverride(root, modelCC(ag), "no_such_key")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Text, `Unknown override key "no_such_key".`) {
		t.Errorf("unknown key reply = %q, want the unknown-key message", resp.Text)
	}
}

// TestOverridesDeleteLegsOnModellessBranchAreNoOpAndFollowRoot proves a
// model-less branch deleting model_endpoint/model_format is a true no-op: the
// keys are not set on the branch, so nothing is written, the branch keeps
// resolving the root's whole tuple — and keeps following it when the root
// later switches models.
func TestOverridesDeleteLegsOnModellessBranchAreNoOpAndFollowRoot(t *testing.T) {
	defaultClient := overridesStubClient{name: "default"}
	ag := &agent.Agent{
		Model:  "claude-opus-4-8",
		Format: "anthropic",
		Client: defaultClient,
	}

	root := session.SessionKey{AgentID: "bot", Type: 'c', ID: "100"}
	branch := root.Branch()
	ag.SetSessionModel(root.String(), "google/gemini-2.5-pro", "gemini", "gemini", overridesStubClient{name: "root"})

	for _, key := range []string{"model_endpoint", "model_format"} {
		resp, err := deleteOverride(branch.String(), modelCC(ag), key)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("Override %q is not set.", key); resp.Text != want {
			t.Errorf("delete %s on model-less branch = %q, want %q", key, resp.Text, want)
		}
		if overrides := ag.SessionOverrides(branch.String()); len(overrides) != 0 {
			t.Errorf("delete %s on model-less branch left overrides %v, want none", key, overrides)
		}
	}

	if got := ag.SessionModel(branch.String()); got != "google/gemini-2.5-pro" {
		t.Errorf("branch model = %q, want inherited root model", got)
	}
	if got := ag.SessionFormat(branch.String()); got != "gemini" {
		t.Errorf("branch format = %q, want inherited root format %q", got, "gemini")
	}
	if got := ag.SessionClient(branch.String()); got != (overridesStubClient{name: "root"}) {
		t.Errorf("branch client = %v, want inherited root client", got)
	}

	// The branch must still follow a later root switch — nothing was pinned.
	ag.SetSessionModel(root.String(), "openai/gpt-5.6", "openai", "openai", overridesStubClient{name: "root2"})
	if got := ag.SessionModel(branch.String()); got != "openai/gpt-5.6" {
		t.Errorf("branch model after root switch = %q, want the new root model %q", got, "openai/gpt-5.6")
	}
	if got := ag.SessionFormat(branch.String()); got != "openai" {
		t.Errorf("branch format after root switch = %q, want the new root format %q", got, "openai")
	}
	if got := ag.SessionClient(branch.String()); got != (overridesStubClient{name: "root2"}) {
		t.Errorf("branch client after root switch = %v, want the new root client", got)
	}
}

// TestOverridesDeleteOrphanModelEndpointRowWithoutOwnModelStaysInheriting
// proves the model_endpoint clear never writes a model the session does not
// own: an orphan endpoint row (no model row — the shape the state.json
// migration can leave) is deleted, the guard passes because the key IS set,
// and the session keeps no model of its own — neither in memory nor in the
// index — so it stays on the agent defaults.
func TestOverridesDeleteOrphanModelEndpointRowWithoutOwnModelStaysInheriting(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	defaultClient := overridesStubClient{name: "default"}
	ag := &agent.Agent{
		Model:        "claude-opus-4-8",
		Format:       "anthropic",
		Endpoint:     "anthropic",
		Client:       defaultClient,
		SessionIndex: idx,
	}
	root := "bot/c100"
	if err := idx.SetSessionMetadata(root, session.MetaKeyModelEndpoint, "gemini"); err != nil {
		t.Fatal(err)
	}
	ag.RestoreSessionOverrides(root)
	if _, set := ag.SessionOverrides(root)[session.MetaKeyModelEndpoint]; !set {
		t.Fatal("sanity: orphan model_endpoint row not listed in SessionOverrides")
	}

	resp, err := deleteOverride(root, modelCC(ag), "model_endpoint")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "cleared") {
		t.Errorf("deleteOverride response = %q, want a cleared confirmation", resp.Text)
	}
	if overrides := ag.SessionOverrides(root); len(overrides) != 0 {
		t.Errorf("overrides after orphan clear = %v, want none — no model may be pinned", overrides)
	}
	if v, err := idx.GetSessionMetadata(root, session.MetaKeyModel); err != nil || v != "" {
		t.Errorf("index model row after orphan clear = %q (err %v), want none", v, err)
	}
	if v, err := idx.GetSessionMetadata(root, session.MetaKeyModelEndpoint); err != nil || v != "" {
		t.Errorf("index model_endpoint row after orphan clear = %q (err %v), want deleted", v, err)
	}
	if got := ag.SessionModel(root); got != "claude-opus-4-8" {
		t.Errorf("model after orphan clear = %q, want agent default %q", got, "claude-opus-4-8")
	}
	if got := ag.SessionFormat(root); got != "anthropic" {
		t.Errorf("format after orphan clear = %q, want agent default %q", got, "anthropic")
	}
	if got := ag.SessionClient(root); got != defaultClient {
		t.Errorf("client after orphan clear = %v, want agent default client", got)
	}
}

// TestOverridesDeleteOrphanModelFormatRowWithoutOwnModelStaysInheriting is
// the model_format twin of the orphan-row test: the clear must delete the
// orphan format row without pinning the effective model onto the session.
func TestOverridesDeleteOrphanModelFormatRowWithoutOwnModelStaysInheriting(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	defaultClient := overridesStubClient{name: "default"}
	ag := &agent.Agent{
		Model:        "claude-opus-4-8",
		Format:       "anthropic",
		Endpoint:     "anthropic",
		Client:       defaultClient,
		SessionIndex: idx,
	}
	root := "bot/c100"
	if err := idx.SetSessionMetadata(root, session.MetaKeyModelFormat, "gemini"); err != nil {
		t.Fatal(err)
	}
	ag.RestoreSessionOverrides(root)
	if _, set := ag.SessionOverrides(root)[session.MetaKeyModelFormat]; !set {
		t.Fatal("sanity: orphan model_format row not listed in SessionOverrides")
	}

	resp, err := deleteOverride(root, modelCC(ag), "model_format")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "cleared") {
		t.Errorf("deleteOverride response = %q, want a cleared confirmation", resp.Text)
	}
	if overrides := ag.SessionOverrides(root); len(overrides) != 0 {
		t.Errorf("overrides after orphan clear = %v, want none — no model may be pinned", overrides)
	}
	if v, err := idx.GetSessionMetadata(root, session.MetaKeyModel); err != nil || v != "" {
		t.Errorf("index model row after orphan clear = %q (err %v), want none", v, err)
	}
	if v, err := idx.GetSessionMetadata(root, session.MetaKeyModelFormat); err != nil || v != "" {
		t.Errorf("index model_format row after orphan clear = %q (err %v), want deleted", v, err)
	}
	if got := ag.SessionModel(root); got != "claude-opus-4-8" {
		t.Errorf("model after orphan clear = %q, want agent default %q", got, "claude-opus-4-8")
	}
	if got := ag.SessionFormat(root); got != "anthropic" {
		t.Errorf("format after orphan clear = %q, want agent default %q", got, "anthropic")
	}
	if got := ag.SessionClient(root); got != defaultClient {
		t.Errorf("client after orphan clear = %v, want agent default client", got)
	}
}

// TestOverridesDeleteModelEndpointOnFullOwnTupleDeletesRowsFromIndex proves
// the own-model clear path against the persisted index: a session with its
// OWN full tuple keeps its model, drops to the agent-default format/client,
// and its endpoint/format rows leave the index (so a restart cannot
// resurrect them). Fails if the clear function is a no-op.
func TestOverridesDeleteModelEndpointOnFullOwnTupleDeletesRowsFromIndex(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	defaultClient := overridesStubClient{name: "default"}
	ag := &agent.Agent{
		Model:        "claude-opus-4-8",
		Format:       "anthropic",
		Client:       defaultClient,
		SessionIndex: idx,
	}
	sk := "bot/c100"
	ag.SetSessionModel(sk, "openai/gpt-5.6", "openai", "openai", overridesStubClient{name: "own"})

	resp, err := deleteOverride(sk, modelCC(ag), "model_endpoint")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "cleared") {
		t.Errorf("deleteOverride response = %q, want a cleared confirmation", resp.Text)
	}

	if got := ag.SessionModel(sk); got != "openai/gpt-5.6" {
		t.Errorf("model after delete model_endpoint = %q, want kept own model %q", got, "openai/gpt-5.6")
	}
	if got := ag.SessionFormat(sk); got != "anthropic" {
		t.Errorf("format after delete model_endpoint = %q, want agent default %q", got, "anthropic")
	}
	if got := ag.SessionClient(sk); got != defaultClient {
		t.Errorf("client after delete model_endpoint = %v, want agent default client", got)
	}
	if v, err := idx.GetSessionMetadata(sk, session.MetaKeyModelEndpoint); err != nil || v != "" {
		t.Errorf("index model_endpoint row after delete = %q (err %v), want deleted", v, err)
	}
	if v, err := idx.GetSessionMetadata(sk, session.MetaKeyModelFormat); err != nil || v != "" {
		t.Errorf("index model_format row after delete = %q (err %v), want deleted", v, err)
	}
	if v, err := idx.GetSessionMetadata(sk, session.MetaKeyModel); err != nil || v != "openai/gpt-5.6" {
		t.Errorf("index model row after delete = %q (err %v), want kept %q", v, err, "openai/gpt-5.6")
	}
}

// TestOverridesDeleteModelFormatOnFullOwnTupleDeletesRowsFromIndex is the
// model_format twin of the full-own-tuple index test.
func TestOverridesDeleteModelFormatOnFullOwnTupleDeletesRowsFromIndex(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	defaultClient := overridesStubClient{name: "default"}
	ag := &agent.Agent{
		Model:        "claude-opus-4-8",
		Format:       "anthropic",
		Client:       defaultClient,
		SessionIndex: idx,
	}
	sk := "bot/c100"
	ag.SetSessionModel(sk, "openai/gpt-5.6", "openai", "openai", overridesStubClient{name: "own"})

	resp, err := deleteOverride(sk, modelCC(ag), "model_format")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "cleared") {
		t.Errorf("deleteOverride response = %q, want a cleared confirmation", resp.Text)
	}

	if got := ag.SessionModel(sk); got != "openai/gpt-5.6" {
		t.Errorf("model after delete model_format = %q, want kept own model %q", got, "openai/gpt-5.6")
	}
	if got := ag.SessionFormat(sk); got != "anthropic" {
		t.Errorf("format after delete model_format = %q, want agent default %q", got, "anthropic")
	}
	if got := ag.SessionClient(sk); got != defaultClient {
		t.Errorf("client after delete model_format = %v, want agent default client", got)
	}
	if v, err := idx.GetSessionMetadata(sk, session.MetaKeyModelFormat); err != nil || v != "" {
		t.Errorf("index model_format row after delete = %q (err %v), want deleted", v, err)
	}
	if v, err := idx.GetSessionMetadata(sk, session.MetaKeyModelEndpoint); err != nil || v != "" {
		t.Errorf("index model_endpoint row after delete = %q (err %v), want deleted", v, err)
	}
	if v, err := idx.GetSessionMetadata(sk, session.MetaKeyModel); err != nil || v != "openai/gpt-5.6" {
		t.Errorf("index model row after delete = %q (err %v), want kept %q", v, err, "openai/gpt-5.6")
	}
}
