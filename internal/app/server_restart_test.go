package app

import (
	"testing"

	"foci/internal/config"
	"foci/internal/platform"
)

// TestHandleServerRestart_CallsRestartWhenConfigEditAvailable proves a
// server.restart frame from a config-editing client invokes the injected
// restart function exactly once.
func TestHandleServerRestart_CallsRestartWhenConfigEditAvailable(t *testing.T) {
	called := 0
	h := newTestHub()
	h.deps = platform.ProviderDeps{
		Config:  &config.Config{SourcePath: "/tmp/foci.toml"},
		Restart: func() (string, error) { called++; return "restarting", nil },
	}

	h.handleServerRestart(fakeClient())

	if called != 1 {
		t.Fatalf("Restart called %d times, want 1", called)
	}
}

// TestHandleServerRestart_GatedOnConfigEdit proves the restart is refused when
// config editing is unavailable (no config source path) — the same gate as the
// config put/unset handlers.
func TestHandleServerRestart_GatedOnConfigEdit(t *testing.T) {
	called := 0
	h := newTestHub()
	h.deps = platform.ProviderDeps{
		// No Config → configEditAvailable() is false.
		Restart: func() (string, error) { called++; return "", nil },
	}

	h.handleServerRestart(fakeClient())

	if called != 0 {
		t.Fatalf("Restart called %d times, want 0 (config edit unavailable)", called)
	}
}

// TestHandleServerRestart_NilRestartDoesNotPanic proves a missing restart dep is
// handled gracefully (logs and returns) rather than panicking.
func TestHandleServerRestart_NilRestartDoesNotPanic(t *testing.T) {
	h := newTestHub()
	h.deps = platform.ProviderDeps{Config: &config.Config{SourcePath: "/tmp/foci.toml"}} // Restart nil

	h.handleServerRestart(fakeClient()) // must not panic
}
