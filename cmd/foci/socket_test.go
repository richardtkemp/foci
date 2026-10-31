package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	// Verify --socket flag extraction from args, through the shared
	// global-flag scanner (parseGlobalFlags) every command uses.
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
		globals, rest := parseGlobalFlags(tt.args)
		if globals.socket != tt.wantSock {
			t.Errorf("parseGlobalFlags(%v): socket = %q, want %q", tt.args, globals.socket, tt.wantSock)
		}
		if len(rest) != len(tt.wantRest) {
			t.Errorf("parseGlobalFlags(%v): rest = %v, want %v", tt.args, rest, tt.wantRest)
		}
	}
}

// TestCheckExplicitSocket proves an explicit --socket path must be a Unix
// socket: a missing path or a regular file is an error naming the path,
// while no flag and a real socket pass.
func TestCheckExplicitSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "gw.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "no-such.sock")

	if err := checkExplicitSocket(""); err != nil {
		t.Errorf("no flag: err = %v, want nil", err)
	}
	if err := checkExplicitSocket(sock); err != nil {
		t.Errorf("real socket: err = %v, want nil", err)
	}
	for path, cause := range map[string]string{missing: "no such file", regular: "not a Unix socket"} {
		err := checkExplicitSocket(path)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), cause) {
			t.Errorf("checkExplicitSocket(%q) = %v, want an error naming the path and %q", path, err, cause)
		}
	}
}

// TestCLIExplicitSocketNeverFallsBackToTCP runs the real binary with a
// --socket path that does not exist while a gateway listens on the TCP
// address: the CLI must exit non-zero naming the path, and the TCP
// listener must see no request — a fall-back would send todo done (a
// write) to whatever listens there.
//
// disconnected-test-ok: black-box CLI test; execs compiled binary
func TestCLIExplicitSocketNeverFallsBackToTCP(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"response":"ok"}`))
	}))
	defer srv.Close()

	missing := filepath.Join(t.TempDir(), "no-such.sock")
	for _, args := range [][]string{
		{"--socket", missing, "todo", "done", "-a", "clutch", "7", "landed"},
		{"--socket=" + missing, "ping"},
	} {
		cmd := exec.Command(testBinary, args...)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"FOCI_ADDR=" + strings.TrimPrefix(srv.URL, "http://"),
		}
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("foci %v: exit 0, want an error (output %q)", args, out)
		}
		if !strings.Contains(string(out), missing) {
			t.Errorf("foci %v: output %q, want it to name %s", args, out, missing)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d request(s) reached the TCP gateway, want none", n)
	}
}

// TestResolveGWSocket_BesideBinary is the macOS case (#2577): the daemon's
// install root is ~/foci (FOCI_HOME in its LaunchAgent), but a login shell or
// ssh session sets neither FOCI_HOME nor HOME=~/foci. The CLI, installed as
// <root>/bin/foci, must still find <root>/data/foci-gw.sock.
func TestResolveGWSocket_BesideBinary(t *testing.T) {
	root := shortSockRoot(t)
	os.MkdirAll(filepath.Join(root, "bin"), 0755)
	os.MkdirAll(filepath.Join(root, "data"), 0755)
	sockPath := filepath.Join(root, "data", "foci-gw.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	t.Setenv("HOME", t.TempDir()) // the user's real home: no data/ socket
	t.Setenv("FOCI_HOME", "")
	t.Setenv("FOCI_GW_SOCK", "")
	orig := executablePath
	executablePath = func() (string, error) { return filepath.Join(root, "bin", "foci"), nil }
	t.Cleanup(func() { executablePath = orig })

	if got := resolveGWSocket(""); got != sockPath {
		t.Errorf("resolveGWSocket() = %q, want %q (socket beside the binary)", got, sockPath)
	}
}

// TestResolveGWSocket_InstallRootBeatsBinary proves the beside-the-binary
// lookup is only a fallback: when $FOCI_HOME/$HOME has a live socket (the NUC,
// where the CLI is /usr/local/bin/foci and the root is /home/foci), that socket
// wins.
func TestResolveGWSocket_InstallRootBeatsBinary(t *testing.T) {
	home := shortSockRoot(t)
	os.MkdirAll(filepath.Join(home, "data"), 0755)
	homeSock := filepath.Join(home, "data", "foci-gw.sock")
	ln1, err := net.Listen("unix", homeSock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln1.Close() }()

	root := shortSockRoot(t)
	os.MkdirAll(filepath.Join(root, "data"), 0755)
	ln2, err := net.Listen("unix", filepath.Join(root, "data", "foci-gw.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln2.Close() }()

	t.Setenv("HOME", home)
	t.Setenv("FOCI_HOME", "")
	t.Setenv("FOCI_GW_SOCK", "")
	orig := executablePath
	executablePath = func() (string, error) { return filepath.Join(root, "bin", "foci"), nil }
	t.Cleanup(func() { executablePath = orig })

	if got := resolveGWSocket(""); got != homeSock {
		t.Errorf("resolveGWSocket() = %q, want %q (install root first)", got, homeSock)
	}
}

// shortSockRoot returns a short temp dir for a socket root. t.TempDir() embeds
// the test name, and a long name pushes <dir>/data/foci-gw.sock past macOS's
// 104-byte sun_path limit (bind: invalid argument), as in #804. The dir sits
// in $TMPDIR, which the sealed test sandbox allows, without the test name.
func shortSockRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
