package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/secretsapprove"
)

// fakeSecrets records submissions; done is kept for the test to fire.
type fakeSecrets struct {
	agent string
	req   secretsapprove.Request
	done  func(secretsapprove.Result)
}

func (f *fakeSecrets) Submit(agentID string, req secretsapprove.Request, done func(secretsapprove.Result)) (string, error) {
	f.agent, f.req, f.done = agentID, req, done
	return "secrets-1", nil
}

func (f *fakeSecrets) List() string { return "LIST" }

// set submits as the calling session's agent and returns at once; the
// outcome reaches that session later as an async message.
func TestSecretsTool_SetSubmitsAndNotifiesOutcome(t *testing.T) {
	fake := &fakeSecrets{}
	var gotSession, gotMsg string
	notifier := NewAsyncNotifier(func(sk, msg, _, _ string, _ *AgentOrigin) { gotSession, gotMsg = sk, msg })
	tool := NewSecretsTool(func() SecretsRequester { return fake }, notifier)

	sk := "clutch/c100"
	ctx := WithSessionKey(context.Background(), sk)
	params, _ := json.Marshal(map[string]any{"action": "set", "args": []string{"hetzner.api_key"}, "allowed_hosts": "api.hetzner.cloud"})
	res, err := tool.Call(ctx, params)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if fake.agent != "clutch" || fake.req.Name != "hetzner.api_key" || len(fake.req.Hosts) != 1 {
		t.Fatalf("submitted agent=%q req=%+v", fake.agent, fake.req)
	}
	if !strings.Contains(res.Text, "secrets-1") {
		t.Errorf("result = %q, want the request id", res.Text)
	}
	if gotMsg != "" {
		t.Fatal("outcome delivered before the user answered")
	}
	fake.done(secretsapprove.Result{ID: "secrets-1", Request: fake.req, Outcome: secretsapprove.Applied, Message: "Secret hetzner.api_key set."})
	if gotSession != sk || !strings.Contains(gotMsg, "applied") {
		t.Errorf("notified %q with %q", gotSession, gotMsg)
	}
}

// list answers at once and needs no session or dialog.
func TestSecretsTool_ListNeedsNoDialog(t *testing.T) {
	fake := &fakeSecrets{}
	tool := NewSecretsTool(func() SecretsRequester { return fake }, nil)
	res, err := tool.Call(context.Background(), json.RawMessage(`{"action":"list"}`))
	if err != nil || res.Text != "LIST" {
		t.Fatalf("list = %q, %v", res.Text, err)
	}
	if fake.done != nil {
		t.Error("list submitted a request")
	}
}

// A bad request is refused before anything is asked.
func TestSecretsTool_BadRequestRefused(t *testing.T) {
	fake := &fakeSecrets{}
	tool := NewSecretsTool(func() SecretsRequester { return fake }, nil)
	ctx := WithSessionKey(context.Background(), "clutch/c100")
	if _, err := tool.Call(ctx, json.RawMessage(`{"action":"set","args":["svc.key","the-value"]}`)); err == nil {
		t.Fatal("a value given as an argument was accepted")
	}
	if fake.done != nil {
		t.Error("bad request submitted")
	}
}
