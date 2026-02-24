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
	cmd := NewBitwardenCommand(nil, false)
	result, err := cmd.Execute(context.Background(), "bogus")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "setup") {
		t.Errorf("unknown subcommand should show usage: %s", result)
	}
}
