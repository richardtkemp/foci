package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsSocket(t *testing.T) {
	// isSocket should return true for socket files, false for everything else.
	t.Parallel()
	dir := t.TempDir()

	// Regular file
	regPath := filepath.Join(dir, "regular")
	os.WriteFile(regPath, []byte("x"), 0600)
	if isSocket(regPath) {
		t.Error("regular file reported as socket")
	}

	// Nonexistent
	if isSocket(filepath.Join(dir, "nope")) {
		t.Error("nonexistent path reported as socket")
	}

	// Actual socket
	sockPath := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if !isSocket(sockPath) {
		t.Error("socket file not detected")
	}
}

func TestResolveGWSocket_EnvVar(t *testing.T) {
	// FOCI_GW_SOCK env var should be honored when it points to an actual socket.
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "env.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	t.Setenv("FOCI_GW_SOCK", sockPath)
	got := resolveGWSocket("")
	if got != sockPath {
		t.Errorf("resolveGWSocket() = %q, want %q", got, sockPath)
	}
}

func TestResolveGWSocket_FlagOverridesEnv(t *testing.T) {
	// Explicit flag value takes priority over FOCI_GW_SOCK env var.
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "flag.sock")
	envPath := filepath.Join(dir, "env.sock")

	// Create only the flag socket
	ln, err := net.Listen("unix", flagPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	t.Setenv("FOCI_GW_SOCK", envPath) // doesn't exist
	got := resolveGWSocket(flagPath)
	if got != flagPath {
		t.Errorf("resolveGWSocket(%q) = %q, want %q", flagPath, got, flagPath)
	}
}

func TestResolveGWSocket_DefaultPath(t *testing.T) {
	// When no flag or env var, should check $HOME/data/foci-gw.sock.
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	os.MkdirAll(dataDir, 0755)
	sockPath := filepath.Join(dataDir, "foci-gw.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	t.Setenv("HOME", dir)
	t.Setenv("FOCI_GW_SOCK", "")
	got := resolveGWSocket("")
	if got != sockPath {
		t.Errorf("resolveGWSocket() = %q, want %q", got, sockPath)
	}
}

func TestResolveGWSocket_NoSocket(t *testing.T) {
	// When no socket exists anywhere, should return empty string.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("FOCI_GW_SOCK", "")
	got := resolveGWSocket("")
	if got != "" {
		t.Errorf("resolveGWSocket() = %q, want empty", got)
	}
}

func TestCLIOverUnixSocket(t *testing.T) {
	// End-to-end: CLI connects to a mock gateway via Unix socket, no API key needed.
	t.Parallel()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "gw.sock")

	mux := http.NewServeMux()
	mux.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Command string `json:"command"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": "sock:" + req.Command})
	})

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	// Use the unix transport directly (simulating what the CLI does)
	c := &http.Client{
		Transport: unixSocketTransport(sockPath),
	}
	resp, err := c.Post("http://foci-gw/command", "application/json",
		strings.NewReader(`{"command":"/ping"}`))
	if err != nil {
		t.Fatalf("POST /command: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct{ Response string }
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Response != "sock:/ping" {
		t.Errorf("response = %q, want %q", result.Response, "sock:/ping")
	}
}

func TestUnixSocketTransport(t *testing.T) {
	// The transport should dial the Unix socket regardless of the URL host.
	t.Parallel()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "transport.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	transport := unixSocketTransport(sockPath)
	c := &http.Client{Transport: transport}

	// The host in the URL is ignored — the transport dials the socket
	for _, host := range []string{"foci-gw", "localhost", "anything"} {
		resp, err := c.Get("http://" + host + "/test")
		if err != nil {
			t.Fatalf("GET via %s: %v", host, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET via %s: status = %d, want 200", host, resp.StatusCode)
		}
	}
}

func TestParseSocketFlag(t *testing.T) {
	// Verify --socket flag extraction from args.
	tests := []struct {
		args     []string
		wantSock string
		wantRest []string
	}{
		{
			args:     []string{"--socket", "/tmp/gw.sock", "send", "hello"},
			wantSock: "/tmp/gw.sock",
			wantRest: []string{"send", "hello"},
		},
		{
			args:     []string{"--socket=/tmp/gw.sock", "send"},
			wantSock: "/tmp/gw.sock",
			wantRest: []string{"send"},
		},
		{
			args:     []string{"send", "hello"},
			wantSock: "",
			wantRest: []string{"send", "hello"},
		},
	}

	for _, tt := range tests {
		sock, rest := parseSocketFlag(tt.args)
		if sock != tt.wantSock {
			t.Errorf("parseSocketFlag(%v): sock = %q, want %q", tt.args, sock, tt.wantSock)
		}
		if len(rest) != len(tt.wantRest) {
			t.Errorf("parseSocketFlag(%v): rest = %v, want %v", tt.args, rest, tt.wantRest)
		}
	}
}

