package command

import (
	"context"
	"strings"
	"testing"
)

func TestSecretsBodyView(t *testing.T) {
	// Proves that viewing allowed_in_body for a section shows the current keys,
	// or "(none)" when empty.
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x", "myapi.other": "x"},
		allowedInBody: map[string][]string{
			"myapi": {"token"},
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	result, err := cmd.Execute(context.Background(), Request{Args: "body myapi"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "token") {
		t.Errorf("expected token in output: %s", result.Text)
	}

	// View for section without allowed_in_body
	result, _ = cmd.Execute(context.Background(), Request{Args: "body legacy"}, cc)
	if !strings.Contains(result.Text, "(none)") {
		t.Errorf("expected (none) for section without allowed_in_body: %s", result.Text)
	}
}

func TestSecretsBodyAdd(t *testing.T) {
	// Proves that adding a key to allowed_in_body persists correctly.
	store := &mockSecretsStore{
		data:          map[string]string{"myapi.token": "x"},
		allowedInBody: map[string][]string{},
	}
	cmd := SecretsCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: "body myapi add token"}, secretsCC(store))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Added") {
		t.Errorf("expected Added message: %s", result.Text)
	}
	if !store.saved {
		t.Error("expected Save() to be called")
	}
	keys := store.SectionAllowedInBody("myapi")
	if len(keys) != 1 || keys[0] != "token" {
		t.Errorf("keys = %v", keys)
	}
}

func TestSecretsBodyRemove(t *testing.T) {
	// Proves that removing a key from allowed_in_body works and returns
	// appropriate messages for present and absent keys.
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedInBody: map[string][]string{
			"myapi": {"token", "other"},
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	result, err := cmd.Execute(context.Background(), Request{Args: "body myapi remove token"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Removed") {
		t.Errorf("expected Removed message: %s", result.Text)
	}
	keys := store.SectionAllowedInBody("myapi")
	if len(keys) != 1 || keys[0] != "other" {
		t.Errorf("keys after remove = %v", keys)
	}

	// Remove nonexistent
	result, _ = cmd.Execute(context.Background(), Request{Args: "body myapi remove nonexistent"}, cc)
	if !strings.Contains(result.Text, "not found") {
		t.Errorf("expected not found message: %s", result.Text)
	}
}

func TestSecretsBodyClear(t *testing.T) {
	// Proves that clearing allowed_in_body removes all entries for the section.
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedInBody: map[string][]string{
			"myapi": {"token"},
		},
	}
	cmd := SecretsCommand()

	result, err := cmd.Execute(context.Background(), Request{Args: "body myapi clear"}, secretsCC(store))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Cleared") {
		t.Errorf("expected Cleared message: %s", result.Text)
	}
	if store.SectionAllowedInBody("myapi") != nil {
		t.Error("keys should be nil after clear")
	}
}

func TestSecretsBodyUsage(t *testing.T) {
	// Proves that bare "body" and invalid actions show usage.
	store := &mockSecretsStore{data: map[string]string{}}
	cmd := SecretsCommand()
	cc := secretsCC(store)

	result, _ := cmd.Execute(context.Background(), Request{Args: "body"}, cc)
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("expected usage: %s", result.Text)
	}

	result, _ = cmd.Execute(context.Background(), Request{Args: "body myapi invalid"}, cc)
	if !strings.Contains(result.Text, "Usage") {
		t.Errorf("expected usage for invalid action: %s", result.Text)
	}
}

func TestSecretsChainKeyboardBody(t *testing.T) {
	// Proves that the body chain keyboard drills down: sections → actions →
	// keys (add shows available keys, remove shows current keys).
	store := &mockSecretsStore{
		data: map[string]string{
			"myapi.token": "x",
			"myapi.other": "x",
		},
		allowedInBody: map[string][]string{
			"myapi": {"token"},
		},
	}
	cmd := SecretsCommand()
	cc := secretsCC(store)
	ctx := context.Background()

	// "body" → section buttons
	opts := cmd.ChainKeyboard(ctx, "body", cc)
	if len(opts) != 1 || opts[0].Label != "myapi" {
		t.Errorf("body opts = %+v", opts)
	}

	// "body myapi" → action buttons [add, remove, clear]
	opts = cmd.ChainKeyboard(ctx, "body myapi", cc)
	if len(opts) != 3 {
		t.Fatalf("body myapi: got %d options, want 3", len(opts))
	}
	labels := make([]string, len(opts))
	for i, o := range opts {
		labels[i] = o.Label
	}
	if strings.Join(labels, ",") != "add,remove,clear" {
		t.Errorf("body myapi labels = %v", labels)
	}

	// "body myapi add" → keys NOT in allowed_in_body (only "other")
	opts = cmd.ChainKeyboard(ctx, "body myapi add", cc)
	if len(opts) != 1 || opts[0].Label != "other" {
		t.Errorf("body myapi add opts = %+v", opts)
	}

	// "body myapi remove" → current keys ("token")
	opts = cmd.ChainKeyboard(ctx, "body myapi remove", cc)
	if len(opts) != 1 || opts[0].Label != "token" {
		t.Errorf("body myapi remove opts = %+v", opts)
	}

	// "body myapi clear" → no further chaining
	opts = cmd.ChainKeyboard(ctx, "body myapi clear", cc)
	if opts != nil {
		t.Errorf("body myapi clear should not chain, got %v", opts)
	}
}
