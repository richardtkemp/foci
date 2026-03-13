package command

import (
	"context"
	"strings"
	"testing"
	"time"
)

type mockBWStore struct {
	items     int
	refreshed time.Time
	cached    []string
}

func (m *mockBWStore) ItemCount() int         { return m.items }
func (m *mockBWStore) RefreshedAt() time.Time { return m.refreshed }
func (m *mockBWStore) CachedIDs() []string    { return m.cached }

// bwCC builds a CommandContext with the given Bitwarden fields set.
func bwCC(store BitwardenStoreInfo, enabled bool) CommandContext {
	return CommandContext{
		BitwardenStore:   store,
		BitwardenEnabled: enabled,
	}
}

func TestBitwardenCommandUsage(t *testing.T) {
	// Verifies that calling /bitwarden with no args returns usage with known subcommands.
	cmd := BitwardenCommand()
	result, err := cmd.Execute(context.Background(), Request{}, bwCC(nil, false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "setup") || !strings.Contains(result.Text, "status") {
		t.Errorf("usage should mention subcommands: %s", result.Text)
	}
}

func TestBitwardenStatusDisabled(t *testing.T) {
	// Verifies that /bitwarden status reports DISABLED when BitwardenEnabled is false.
	cmd := BitwardenCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "status"}, bwCC(nil, false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "DISABLED") {
		t.Errorf("should show DISABLED: %s", result.Text)
	}
}

func TestBitwardenStatusEnabled(t *testing.T) {
	// Verifies that /bitwarden status shows item count, unlocked count, and cached IDs when enabled.
	store := &mockBWStore{
		items:     42,
		refreshed: time.Now().Add(-5 * time.Minute),
		cached:    []string{"aaaa-1111", "bbbb-2222"},
	}
	cmd := BitwardenCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "status"}, bwCC(store, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "ENABLED") {
		t.Errorf("should show ENABLED: %s", result.Text)
	}
	if !strings.Contains(result.Text, "42") {
		t.Errorf("should show item count: %s", result.Text)
	}
	if !strings.Contains(result.Text, "2") {
		t.Errorf("should show unlocked count: %s", result.Text)
	}
	if !strings.Contains(result.Text, "aaaa-1111") {
		t.Errorf("should list cached IDs: %s", result.Text)
	}
}

func TestBitwardenStatusEnabledNoStore(t *testing.T) {
	// Verifies that /bitwarden status reports not initialized when enabled but store is nil.
	cmd := BitwardenCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "status"}, bwCC(nil, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "not initialized") {
		t.Errorf("should show not initialized: %s", result.Text)
	}
}

func TestBitwardenUnknownSubcommand(t *testing.T) {
	// Verifies that an unknown subcommand falls back to usage output.
	cmd := BitwardenCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "bogus"}, bwCC(nil, false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "setup") {
		t.Errorf("unknown subcommand should show usage: %s", result.Text)
	}
}
