package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeStubCodex creates an executable named "codex" in a temp dir and
// returns that directory. CheckReady only runs exec.LookPath, so the stub
// just needs to exist and be executable.
func writeStubCodex(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return dir
}

func TestCheckReady_BinaryInPath(t *testing.T) {
	dir := writeStubCodex(t)
	t.Setenv("PATH", dir)
	b := &Backend{}

	ready, err := b.CheckReady(context.Background())
	if err != nil {
		t.Fatalf("CheckReady err: %v", err)
	}
	if !ready {
		t.Errorf("ready = false; want true")
	}
}

func TestCheckReady_BinaryNotFound(t *testing.T) {
	// PATH points at an empty dir → LookPath cannot find "codex".
	t.Setenv("PATH", t.TempDir())
	b := &Backend{}

	ready, err := b.CheckReady(context.Background())
	if err == nil {
		t.Fatalf("CheckReady err = nil; want not-found error")
	}
	if ready {
		t.Errorf("ready = true; want false when binary is missing")
	}
}

func TestWaitReady_AlreadyReady(t *testing.T) {
	b := &Backend{}
	b.readyCh = make(chan struct{})
	close(b.readyCh)

	if err := b.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady err: %v", err)
	}
}

func TestWaitReady_ContextCancellation(t *testing.T) {
	b := &Backend{}
	b.readyCh = make(chan struct{}) // never closed

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.WaitReady(ctx); err != context.Canceled {
		t.Errorf("WaitReady err = %v; want context.Canceled", err)
	}
}
