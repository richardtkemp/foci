package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveStaleSocket_NoFile(t *testing.T) {
	// Removing a nonexistent socket should succeed silently.
	t.Parallel()
	err := removeStaleSocket(filepath.Join(t.TempDir(), "no-such.sock"))
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestRemoveStaleSocket_RegularFile(t *testing.T) {
	// Refusing to remove a regular file protects against accidental deletion.
	t.Parallel()
	path := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	err := removeStaleSocket(path)
	if err == nil {
		t.Fatal("expected error for regular file")
	}
}

func TestRemoveStaleSocket_ActualSocket(t *testing.T) {
	// A leftover socket file from a previous run should be cleaned up.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close() // close listener but leave the file

	if err := removeStaleSocket(path); err != nil {
		t.Fatalf("removeStaleSocket: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket file still exists after removal")
	}
}

func TestStartUnixSocket_ServesHTTP(t *testing.T) {
	// The Unix socket server should serve HTTP requests from the same user.
	t.Parallel()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	})

	srv, err := startUnixSocket(sockPath, mux)
	if err != nil {
		t.Fatalf("startUnixSocket: %v", err)
	}
	defer cleanupSocket(srv, sockPath)

	// Verify socket file exists with restricted permissions
	fi, err := os.Lstat(sockPath)
	if err != nil {
		t.Fatalf("socket file missing: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("not a socket: %v", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("socket permissions = %o, want 0600", perm)
	}

	// Make HTTP request over the socket
	c := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := c.Get("http://foci-gw/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	if string(body) != "pong" {
		t.Fatalf("body = %q, want %q", body, "pong")
	}
}

func TestStartUnixSocket_CleansUpStale(t *testing.T) {
	// Starting a new socket should clean up a stale one from a previous run.
	t.Parallel()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "stale.sock")

	// Create and close a listener to leave a stale socket file
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv, err := startUnixSocket(sockPath, mux)
	if err != nil {
		t.Fatalf("startUnixSocket over stale socket: %v", err)
	}
	defer cleanupSocket(srv, sockPath)

	c := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := c.Get("http://foci-gw/ok")
	if err != nil {
		t.Fatalf("GET /ok: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestPeerCredMiddleware_RejectsMissingUID(t *testing.T) {
	// Requests without peer credentials (e.g. if ConnContext wasn't set)
	// must be rejected.
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := peerCredMiddleware(inner)

	// Simulate a request with no peer UID in context
	req, _ := http.NewRequest("GET", "/test", nil)
	rec := &statusRecorder{code: 0}
	handler.ServeHTTP(rec, req)
	if rec.code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.code)
	}
}

func TestPeerCredMiddleware_AcceptsSameUID(t *testing.T) {
	// Requests from the same UID should be allowed through.
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := peerCredMiddleware(inner)

	ctx := context.WithValue(context.Background(), peerUIDKey{}, uint32(os.Getuid()))
	req, _ := http.NewRequestWithContext(ctx, "GET", "/test", nil)
	rec := &statusRecorder{code: 0}
	handler.ServeHTTP(rec, req)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.code)
	}
}

func TestPeerCredMiddleware_RejectsWrongUID(t *testing.T) {
	// Requests from a different UID should be rejected.
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := peerCredMiddleware(inner)

	// Use a UID that definitely isn't ours
	fakeUID := uint32(os.Getuid()) + 99999
	ctx := context.WithValue(context.Background(), peerUIDKey{}, fakeUID)
	req, _ := http.NewRequestWithContext(ctx, "GET", "/test", nil)
	rec := &statusRecorder{code: 0}
	handler.ServeHTTP(rec, req)
	if rec.code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.code)
	}
}

func TestGetPeerUID(t *testing.T) {
	// Verify that getPeerUID extracts the correct UID from a real Unix socket connection.
	t.Parallel()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "peercred.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- fmt.Errorf("accept: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()

		uc, ok := conn.(*net.UnixConn)
		if !ok {
			done <- fmt.Errorf("not a UnixConn: %T", conn)
			return
		}
		uid, err := getPeerUID(uc)
		if err != nil {
			done <- fmt.Errorf("getPeerUID: %w", err)
			return
		}
		if uid != uint32(os.Getuid()) {
			done <- fmt.Errorf("uid = %d, want %d", uid, os.Getuid())
			return
		}
		done <- nil
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCleanupSocket(t *testing.T) {
	// cleanupSocket should remove the socket file and close the server.
	t.Parallel()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "cleanup.sock")

	mux := http.NewServeMux()
	srv, err := startUnixSocket(sockPath, mux)
	if err != nil {
		t.Fatal(err)
	}

	cleanupSocket(srv, sockPath)

	if _, err := os.Lstat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("socket file still exists after cleanup")
	}
}

// statusRecorder is a minimal ResponseWriter that records the status code.
type statusRecorder struct {
	code int
}

func (r *statusRecorder) Header() http.Header        { return http.Header{} }
func (r *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *statusRecorder) WriteHeader(code int)        { r.code = code }
