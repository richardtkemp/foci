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

func (m *mockBWStore) ItemCount() int           { return m.items }
func (m *mockBWStore) RefreshedAt() time.Time   { return m.refreshed }
func (m *mockBWStore) CachedIDs() []string      { return m.cached }

func TestBitwardenCommandUsage(t *testing.T) {
	// Verifies that invoking the bitwarden command with no args shows the usage
	// message including both "setup" and "status" subcommands.
	cmd := NewBitwardenCommand(nil, false)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "setup") || !strings.Contains(result, "status") {
		t.Errorf("usage should mention subcommands: %s", result)
	}
}

func TestBitwardenStatusDisabled(t *testing.T) {
	// Verifies that "status" with bitwarden disabled (no store, enabled=false)
	// reports DISABLED in the output.
	cmd := NewBitwardenCommand(nil, false)
	result, err := cmd.Execute(context.Background(), "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "DISABLED") {
		t.Errorf("should show DISABLED: %s", result)
	}
}

func TestBitwardenStatusEnabled(t *testing.T) {
	// Verifies that "status" with a populated store shows ENABLED, item count,
	// unlocked secret count, and lists cached IDs.
	store := &mockBWStore{
		items:     42,
		refreshed: time.Now().Add(-5 * time.Minute),
		cached:    []string{"aaaa-1111", "bbbb-2222"},
	}
	cmd := NewBitwardenCommand(store, true)
	result, err := cmd.Execute(context.Background(), "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "ENABLED") {
		t.Errorf("should show ENABLED: %s", result)
	}
	if !strings.Contains(result, "42") {
		t.Errorf("should show item count: %s", result)
	}
	if !strings.Contains(result, "2") {
		t.Errorf("should show unlocked count: %s", result)
	}
	if !strings.Contains(result, "aaaa-1111") {
		t.Errorf("should list cached IDs: %s", result)
	}
}

func TestBitwardenStatusEnabledNoStore(t *testing.T) {
	// Verifies that "status" with enabled=true but no store reports "not initialized"
	// rather than panicking or showing incorrect data.
	cmd := NewBitwardenCommand(nil, true)
	result, err := cmd.Execute(context.Background(), "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "not initialized") {
		t.Errorf("should show not initialized: %s", result)
	}
}

func TestBitwardenUnknownSubcommand(t *testing.T) {
	// Verifies that an unrecognised subcommand falls back to showing usage
	// rather than returning an error.
	cmd := NewBitwardenCommand(nil, false)
	result, err := cmd.Execute(context.Background(), "bogus")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "setup") {
		t.Errorf("unknown subcommand should show usage: %s", result)
	}
}
