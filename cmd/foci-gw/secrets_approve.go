package main

import (
	"sync/atomic"

	"foci/internal/app"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/secrets"
	"foci/internal/secretsapprove"
	"foci/internal/tools"
)

// secrets_approve.go wires the agent secrets tool (#3090): foci_secrets asks
// the gateway's secretsapprove.Service, which shows the user an app-wide
// dialog (the askgw dialog, internal/app/askgw_prompts.go) and, for a set,
// takes the typed value from the app straight into the root secret store.
//
// Approver (Dick 2026-11-13): "Only me, in my chat", whichever agent asks. The
// foci app is his surface alone, so the prompt goes to the app and nowhere
// else ("App only"): with no app device able to show it, the request fails.

// gwSecretsApprove is the process-wide service, set in main once the agent
// registry exists. The secrets tool resolves it per call.
var gwSecretsApprove atomic.Pointer[secretsRequester]

// secretsRequester adapts the service and the root store to
// tools.SecretsRequester.
type secretsRequester struct {
	*secretsapprove.Service
	store *secrets.Store
}

// List renders names and rules from the root store, never values.
func (r *secretsRequester) List() string { return secretsapprove.List(r.store) }

// currentSecretsRequester is the secrets tool's resolver: nil until main has
// built the service.
func currentSecretsRequester() tools.SecretsRequester {
	if r := gwSecretsApprove.Load(); r != nil {
		return r
	}
	return nil
}

// newSecretsRequester builds the service over the ROOT store (agent views are
// read-only) with the app as its only surface.
func newSecretsRequester(store *secrets.Store, agents *agentRegistry) *secretsRequester {
	svc := secretsapprove.New(secretsapprove.Deps{
		Store:      store,
		KnownAgent: func(id string) bool { _, ok := agents.Get(id); return ok },
		Present:    presentSecretsPrompt,
		Close:      func(id string) { app.CloseAskgwPrompt(id) },
	})
	return &secretsRequester{Service: svc, store: store}
}

// presentSecretsPrompt shows p in the app-wide dialog. A set gets the masked
// value step after Approve; every request can be denied.
func presentSecretsPrompt(p secretsapprove.Prompt) error {
	buttons := []platform.ButtonChoice{
		{Label: "Approve", Data: secretsapprove.ChoiceApprove},
		{Label: "Deny", Data: secretsapprove.ChoiceDeny},
	}
	var value *fap.ValueInput
	if p.NeedsValue {
		value = &fap.ValueInput{Choice: secretsapprove.ChoiceApprove, Label: p.ValueLabel, CancelData: secretsapprove.ChoiceCancel}
	}
	// The answer arrives on the device's socket goroutine; the file write and
	// the agent's notice run off it, like the /secrets wizard's writes.
	return app.PresentAppPrompt(p.ID, p.Header, p.Text, p.Warning, buttons, value, func(data, v string) {
		go p.OnAnswer(secretsapprove.Answer{Decision: data, Value: v})
	})
}
