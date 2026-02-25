package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// mockGateway creates a test server that mimics the clod HTTP gateway.
// It echoes the agent field back in responses so tests can verify it.
func mockGateway() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Agent      string `json:"agent"`
			Session    string `json:"session"`
			Text       string `json:"text"`
			IfActive   string `json:"if_active"`
			IfInactive string `json:"if_inactive"`
			Async      bool   `json:"async"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Agent == "nonexistent" {
			http.Error(w, "unknown agent: \"nonexistent\"", http.StatusBadRequest)
			return
		}
		if req.Async {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
			return
		}
		resp := "echo: " + req.Text
		if req.Agent != "" {
			resp = "[" + req.Agent + "] " + resp
		}
		if req.Session != "" {
			resp = "(session:" + req.Session + ") " + resp
		}
		if req.IfActive != "" {
			resp = "(if_active:" + req.IfActive + ") " + resp
		}
		if req.IfInactive != "" {
			resp = "(if_inactive:" + req.IfInactive + ") " + resp
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": resp})
	})

	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Agent      string `json:"agent"`
			Text       string `json:"text"`
			NoCompact  bool   `json:"no_compact"`
			IfActive   string `json:"if_active"`
			IfInactive string `json:"if_inactive"`
			Async      bool   `json:"async"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Agent == "nonexistent" {
			http.Error(w, "unknown agent: \"nonexistent\"", http.StatusBadRequest)
			return
		}
		if req.Async {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
			return
		}
		resp := "wake ok"
		if req.NoCompact {
			resp = "wake ok (no_compact)"
		}
		if req.Agent != "" {
			resp = "[" + req.Agent + "] " + resp
		}
		if req.IfActive != "" {
			resp = "(if_active:" + req.IfActive + ") " + resp
		}
		if req.IfInactive != "" {
			resp = "(if_inactive:" + req.IfInactive + ") " + resp
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": resp})
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		agent := r.URL.Query().Get("agent")
		resp := "status: idle"
		if agent != "" {
			resp = "[" + agent + "] " + resp
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": resp})
	})

	mux.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Agent   string `json:"agent"`
			Command string `json:"command"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Command == "/ping" {
			resp := "pong"
			if req.Agent != "" {
				resp = "[" + req.Agent + "] " + resp
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"response": resp})
			return
		}
		http.Error(w, "unknown command", http.StatusNotFound)
	})

	return httptest.NewServer(mux)
}

func TestCLIIntegration(t *testing.T) {
	server := mockGateway()
	defer server.Close()

	// Set the address to point at our mock
	addr := strings.TrimPrefix(server.URL, "http://")

	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		// Default mode is async — returns "queued"
		{"send", []string{"send", "hello"}, "queued", false},
		{"branch", []string{"branch"}, "queued", false},
		{"wake alias", []string{"wake"}, "queued", false},

		// --sync returns full response
		{"send --sync", []string{"send", "--sync", "hello"}, "echo: hello", false},
		{"send --wait", []string{"send", "--wait", "hello"}, "echo: hello", false},
		{"branch --sync", []string{"branch", "--sync"}, "wake ok", false},
		{"branch --wait", []string{"branch", "--wait"}, "wake ok", false},

		// Explicit --async
		{"send --async", []string{"send", "--async", "hello"}, "queued", false},
		{"send --no-wait", []string{"send", "--no-wait", "hello"}, "queued", false},
		{"branch --async", []string{"branch", "--async"}, "queued", false},
		{"branch --no-wait", []string{"branch", "--no-wait"}, "queued", false},

		{"status", []string{"status"}, "status: idle", false},
		{"ping", []string{"ping"}, "pong", false},
		{"command", []string{"command", "/ping"}, "pong", false},
		// eval is always sync
		{"eval", []string{"eval", "ls -la"}, "echo: Run this command", false},

		// -a flag (space-separated) — use --sync to get content-based responses
		{"send with -a", []string{"send", "--sync", "-a", "research", "hello"}, "[research] echo: hello", false},
		{"branch with -a", []string{"branch", "--sync", "-a", "research"}, "[research] wake ok", false},
		{"wake alias with -a", []string{"wake", "--sync", "-a", "research"}, "[research] wake ok", false},
		{"branch with -a and text", []string{"branch", "--sync", "-a", "research", "check news"}, "[research] wake ok", false},
		{"wake alias with -a and text", []string{"wake", "--sync", "-a", "research", "check news"}, "[research] wake ok", false},
		{"status with -a", []string{"status", "-a", "research"}, "[research] status: idle", false},
		{"eval with -a", []string{"eval", "-a", "research", "ls"}, "[research] echo: Run this command", false},
		{"command with -a", []string{"command", "-a", "research", "/ping"}, "[research] pong", false},
		{"ping with -a", []string{"ping", "-a", "research"}, "[research] pong", false},

		// --agent flag (space-separated)
		{"send with --agent", []string{"send", "--sync", "--agent", "main", "hello"}, "[main] echo: hello", false},

		// --agent=value form
		{"send with --agent=val", []string{"send", "--sync", "--agent=scout", "hello"}, "[scout] echo: hello", false},

		// -a=value form
		{"send with -a=val", []string{"send", "--sync", "-a=scout", "hello"}, "[scout] echo: hello", false},

		// -s/--session flag
		{"send with -s", []string{"send", "--sync", "-s", "research", "hello"}, "(session:research) echo: hello", false},
		{"send with --session", []string{"send", "--sync", "--session", "feature1", "hello"}, "(session:feature1) echo: hello", false},
		{"send with -s=value", []string{"send", "--sync", "-s=branch1", "hello"}, "(session:branch1) echo: hello", false},
		{"send with --session=value", []string{"send", "--sync", "--session=testing", "hello"}, "(session:testing) echo: hello", false},

		// -a and -s flags together
		{"send with -a and -s", []string{"send", "--sync", "-a", "clutch", "-s", "research", "hello"}, "(session:research) [clutch] echo: hello", false},
		{"send with -s and -a", []string{"send", "--sync", "-s", "feature", "-a", "clutch", "hello"}, "(session:feature) [clutch] echo: hello", false},
		{"send with -a= and -s=", []string{"send", "--sync", "-a=clutch", "-s=main", "hello"}, "(session:main) [clutch] echo: hello", false},

		// Flag after positional args
		{"send flag after text", []string{"send", "--sync", "hello", "-a", "research"}, "[research] echo: hello", false},

		// --no-compact flag for branch
		{"branch with --no-compact", []string{"branch", "--sync", "--no-compact"}, "wake ok (no_compact)", false},
		{"branch with --no-compact and text", []string{"branch", "--sync", "--no-compact", "morning check"}, "wake ok (no_compact)", false},
		{"branch with -a and --no-compact", []string{"branch", "--sync", "-a", "research", "--no-compact"}, "[research] wake ok (no_compact)", false},

		// --if-active flag for send
		{"send with --if-active", []string{"send", "--sync", "--if-active", "8h", "hello"}, "(if_active:8h) echo: hello", false},
		{"send with --if-active=", []string{"send", "--sync", "--if-active=30m", "hello"}, "(if_active:30m) echo: hello", false},
		{"send with -a and --if-active", []string{"send", "--sync", "-a", "clutch", "--if-active", "4h", "hello"}, "(if_active:4h) [clutch] echo: hello", false},

		// --if-active flag for branch
		{"branch with --if-active", []string{"branch", "--sync", "--if-active", "12h", "do work"}, "(if_active:12h) wake ok", false},
		{"branch with --if-active=", []string{"branch", "--sync", "--if-active=6h"}, "(if_active:6h) wake ok", false},
		{"branch with -a and --if-active", []string{"branch", "--sync", "-a", "research", "--if-active", "8h"}, "(if_active:8h) [research] wake ok", false},
		{"branch with --if-active and --no-compact", []string{"branch", "--sync", "--if-active", "8h", "--no-compact"}, "(if_active:8h) wake ok (no_compact)", false},

		// --if-inactive flag for send
		{"send with --if-inactive", []string{"send", "--sync", "--if-inactive", "30m", "hello"}, "(if_inactive:30m) echo: hello", false},
		{"send with --if-inactive=", []string{"send", "--sync", "--if-inactive=1h", "hello"}, "(if_inactive:1h) echo: hello", false},

		// --if-inactive flag for branch
		{"branch with --if-inactive", []string{"branch", "--sync", "--if-inactive", "30m", "heartbeat"}, "(if_inactive:30m) wake ok", false},
		{"branch with --if-inactive=", []string{"branch", "--sync", "--if-inactive=45m"}, "(if_inactive:45m) wake ok", false},
		{"branch with --if-inactive and --oneshot", []string{"branch", "--sync", "--if-inactive", "30m", "--oneshot", "check emails"}, "(if_inactive:30m) wake ok (no_compact)", false},

		// --message-text / -mt flag
		{"send with -mt", []string{"send", "--sync", "-mt", "hello from mt"}, "echo: hello from mt", false},
		{"send with --message-text", []string{"send", "--sync", "--message-text", "explicit text"}, "echo: explicit text", false},
		{"send with -mt=value", []string{"send", "--sync", "-mt=inline"}, "echo: inline", false},
		{"send with -mt and -a", []string{"send", "--sync", "-a", "research", "-mt", "flagged"}, "[research] echo: flagged", false},
		{"branch with -mt", []string{"branch", "--sync", "-mt", "branch text"}, "wake ok", false},

		// Error cases: unknown agent returns HTTP 400, exit non-zero
		{"send unknown agent", []string{"send", "-a", "nonexistent", "hello"}, "unknown agent", true},
		{"branch unknown agent", []string{"branch", "-a", "nonexistent"}, "unknown agent", true},
	}

	// Build the CLI binary once
	binPath := t.TempDir() + "/clod"
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(binPath, tt.args...)
			cmd.Env = append(os.Environ(), "CLOD_ADDR="+addr)

			out, err := cmd.CombinedOutput()
			output := strings.TrimSpace(string(out))

			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v\noutput: %s", err, output)
			}
			if !strings.Contains(output, tt.want) {
				t.Errorf("output %q does not contain %q", output, tt.want)
			}
		})
	}
}

func TestCLIMessageFile(t *testing.T) {
	server := mockGateway()
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	// Create temp file with message contents
	msgFile := t.TempDir() + "/msg.md"
	os.WriteFile(msgFile, []byte("hello from file"), 0644)

	// Build the CLI binary
	binPath := t.TempDir() + "/clod"
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"send -mf", []string{"send", "--sync", "-mf", msgFile}, "echo: hello from file", false},
		{"send --message-file", []string{"send", "--sync", "--message-file", msgFile}, "echo: hello from file", false},
		{"send -mf=value", []string{"send", "--sync", "-mf=" + msgFile}, "echo: hello from file", false},
		{"send -mf with -a", []string{"send", "--sync", "-a", "research", "-mf", msgFile}, "[research] echo: hello from file", false},
		{"branch -mf", []string{"branch", "--sync", "-mf", msgFile}, "wake ok", false},
		{"branch --oneshot -mf", []string{"branch", "--sync", "--oneshot", "-mf", msgFile}, "wake ok (no_compact)", false},

		// Error: both -mt and -mf
		{"send -mt and -mf", []string{"send", "-mt", "text", "-mf", msgFile}, "cannot specify both", true},
		// Error: missing file
		{"send -mf missing", []string{"send", "-mf", "/nonexistent/file.md"}, "reading message file", true},
		// Error: branch -mt and -mf
		{"branch -mt and -mf", []string{"branch", "-mt", "text", "-mf", msgFile}, "cannot specify both", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(binPath, tt.args...)
			cmd.Env = append(os.Environ(), "CLOD_ADDR="+addr)

			out, err := cmd.CombinedOutput()
			output := strings.TrimSpace(string(out))

			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v\noutput: %s", err, output)
			}
			if !strings.Contains(output, tt.want) {
				t.Errorf("output %q does not contain %q", output, tt.want)
			}
		})
	}
}

func TestCLIEnvVars(t *testing.T) {
	server := mockGateway()
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	// Create temp file for -mf tests
	msgFile := t.TempDir() + "/msg.md"
	os.WriteFile(msgFile, []byte("env file msg"), 0644)

	// Build the CLI binary
	binPath := t.TempDir() + "/clod"
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	tests := []struct {
		name    string
		args    []string
		env     []string // extra env vars beyond CLOD_ADDR
		want    string
		wantErr bool
	}{
		{
			name: "CLOD_AGENT env var",
			args: []string{"send", "--sync", "hello"},
			env:  []string{"CLOD_AGENT=research"},
			want: "[research] echo: hello",
		},
		{
			name: "flag overrides CLOD_AGENT",
			args: []string{"send", "--sync", "-a", "main", "hello"},
			env:  []string{"CLOD_AGENT=research"},
			want: "[main] echo: hello",
		},
		{
			name: "CLOD_SESSION env var",
			args: []string{"send", "--sync", "hello"},
			env:  []string{"CLOD_SESSION=research"},
			want: "(session:research) echo: hello",
		},
		{
			name: "CLOD_IF_ACTIVE env var for send",
			args: []string{"send", "--sync", "hello"},
			env:  []string{"CLOD_IF_ACTIVE=8h"},
			want: "(if_active:8h) echo: hello",
		},
		{
			name: "CLOD_MESSAGE_TEXT env var",
			args: []string{"send", "--sync"},
			env:  []string{"CLOD_MESSAGE_TEXT=from env"},
			want: "echo: from env",
		},
		{
			name: "CLOD_MESSAGE_FILE env var",
			args: []string{"send", "--sync"},
			env:  []string{"CLOD_MESSAGE_FILE=" + msgFile},
			want: "echo: env file msg",
		},
		{
			name: "CLOD_NO_COMPACT env var",
			args: []string{"branch", "--sync"},
			env:  []string{"CLOD_NO_COMPACT=1"},
			want: "wake ok (no_compact)",
		},
		{
			name: "CLOD_ONESHOT env var",
			args: []string{"branch", "--sync"},
			env:  []string{"CLOD_ONESHOT=1"},
			want: "wake ok (no_compact)",
		},
		{
			name: "CLOD_IF_ACTIVE env var for branch",
			args: []string{"branch", "--sync"},
			env:  []string{"CLOD_IF_ACTIVE=12h"},
			want: "(if_active:12h) wake ok",
		},
		{
			name: "CLOD_IF_INACTIVE env var for send",
			args: []string{"send", "--sync", "hello"},
			env:  []string{"CLOD_IF_INACTIVE=30m"},
			want: "(if_inactive:30m) echo: hello",
		},
		{
			name: "CLOD_IF_INACTIVE env var for branch",
			args: []string{"branch", "--sync"},
			env:  []string{"CLOD_IF_INACTIVE=45m"},
			want: "(if_inactive:45m) wake ok",
		},
		{
			name: "--addr flag",
			args: []string{"--addr", addr, "send", "--sync", "hello"},
			env:  nil, // no CLOD_ADDR
			want: "echo: hello",
		},
		{
			name: "--addr=value flag",
			args: []string{"--addr=" + addr, "send", "--sync", "hello"},
			env:  nil,
			want: "echo: hello",
		},
		{
			name: "CLOD_AGENT env var for branch",
			args: []string{"branch", "--sync", "do work"},
			env:  []string{"CLOD_AGENT=research"},
			want: "[research] wake ok",
		},
		{
			name: "CLOD_SYNC env var for send",
			args: []string{"send", "hello"},
			env:  []string{"CLOD_SYNC=1"},
			want: "echo: hello",
		},
		{
			name: "CLOD_SYNC env var for branch",
			args: []string{"branch"},
			env:  []string{"CLOD_SYNC=1"},
			want: "wake ok",
		},
		{
			name: "CLOD_ASYNC env var for send",
			args: []string{"send", "hello"},
			env:  []string{"CLOD_ASYNC=1"},
			want: "queued",
		},
		{
			name: "CLOD_ASYNC env var for branch",
			args: []string{"branch"},
			env:  []string{"CLOD_ASYNC=1"},
			want: "queued",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(binPath, tt.args...)
			// Start with minimal env to avoid inheriting CLOD_ vars
			env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
			if tt.env != nil {
				env = append(env, tt.env...)
			}
			// Add CLOD_ADDR unless --addr is being tested
			hasAddr := false
			for _, e := range tt.env {
				if strings.HasPrefix(e, "CLOD_ADDR=") {
					hasAddr = true
				}
			}
			for _, a := range tt.args {
				if a == "--addr" || strings.HasPrefix(a, "--addr=") {
					hasAddr = true
				}
			}
			if !hasAddr {
				env = append(env, "CLOD_ADDR="+addr)
			}
			cmd.Env = env

			out, err := cmd.CombinedOutput()
			output := strings.TrimSpace(string(out))

			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v\noutput: %s", err, output)
			}
			if !strings.Contains(output, tt.want) {
				t.Errorf("output %q does not contain %q", output, tt.want)
			}
		})
	}
}

func TestResolveMessage(t *testing.T) {
	// Create temp file
	tmpFile := t.TempDir() + "/msg.txt"
	os.WriteFile(tmpFile, []byte("file contents"), 0644)

	tests := []struct {
		name    string
		flags   sendFlags
		trail   []string
		want    string
		wantErr string
	}{
		{"trailing args", sendFlags{}, []string{"hello", "world"}, "hello world", ""},
		{"explicit -mt", sendFlags{messageText: "explicit"}, nil, "explicit", ""},
		{"explicit -mf", sendFlags{messageFile: tmpFile}, nil, "file contents", ""},
		{"-mt overrides trailing", sendFlags{messageText: "explicit"}, []string{"trailing"}, "explicit", ""},
		{"both -mt and -mf", sendFlags{messageText: "t", messageFile: tmpFile}, nil, "", "cannot specify both"},
		{"missing file", sendFlags{messageFile: "/no/such/file"}, nil, "", "reading message file"},
		{"empty", sendFlags{}, nil, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveMessage(tt.flags, tt.trail)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseSendFlagsMessageFlags(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantMT      string
		wantMF      string
		wantRest    []string
	}{
		{"-mt with value", []string{"-mt", "hello"}, "hello", "", nil},
		{"--mt with value", []string{"--mt", "hello"}, "hello", "", nil},
		{"--message-text with value", []string{"--message-text", "hello"}, "hello", "", nil},
		{"-mt=value", []string{"-mt=hello"}, "hello", "", nil},
		{"--mt=value", []string{"--mt=hello"}, "hello", "", nil},
		{"--message-text=value", []string{"--message-text=hello"}, "hello", "", nil},
		{"-mf with value", []string{"-mf", "/tmp/f"}, "", "/tmp/f", nil},
		{"--mf with value", []string{"--mf", "/tmp/f"}, "", "/tmp/f", nil},
		{"--message-file with value", []string{"--message-file", "/tmp/f"}, "", "/tmp/f", nil},
		{"-mf=value", []string{"-mf=/tmp/f"}, "", "/tmp/f", nil},
		{"--mf=value", []string{"--mf=/tmp/f"}, "", "/tmp/f", nil},
		{"--message-file=value", []string{"--message-file=/tmp/f"}, "", "/tmp/f", nil},
		{"-mt with other flags", []string{"-a", "clutch", "-mt", "hi", "extra"}, "hi", "", []string{"extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest := parseSendFlags(tt.args)
			if flags.messageText != tt.wantMT {
				t.Errorf("messageText = %q, want %q", flags.messageText, tt.wantMT)
			}
			if flags.messageFile != tt.wantMF {
				t.Errorf("messageFile = %q, want %q", flags.messageFile, tt.wantMF)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

func TestParseSendFlagsAsyncSync(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantAsync bool
		wantSync  bool
		wantRest  []string
	}{
		{"--async", []string{"--async", "hello"}, true, false, []string{"hello"}},
		{"--no-wait", []string{"--no-wait", "hello"}, true, false, []string{"hello"}},
		{"--sync", []string{"--sync", "hello"}, false, true, []string{"hello"}},
		{"--wait", []string{"--wait", "hello"}, false, true, []string{"hello"}},
		{"--sync with other flags", []string{"-a", "clutch", "--sync", "hello"}, false, true, []string{"hello"}},
		{"no async/sync flags", []string{"hello"}, false, false, []string{"hello"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest := parseSendFlags(tt.args)
			if flags.async != tt.wantAsync {
				t.Errorf("async = %v, want %v", flags.async, tt.wantAsync)
			}
			if flags.sync != tt.wantSync {
				t.Errorf("sync = %v, want %v", flags.sync, tt.wantSync)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

func TestPrintResponseError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    string
	}{
		{
			name:       "400 unknown agent",
			statusCode: http.StatusBadRequest,
			body:       "unknown agent: \"nonexistent\"\n",
			wantErr:    "HTTP 400: unknown agent: \"nonexistent\"",
		},
		{
			name:       "404 not found",
			statusCode: http.StatusNotFound,
			body:       "unknown command\n",
			wantErr:    "HTTP 404: unknown command",
		},
		{
			name:       "500 internal error",
			statusCode: http.StatusInternalServerError,
			body:       "internal error\n",
			wantErr:    "HTTP 500: internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, strings.TrimSuffix(tt.body, "\n"), tt.statusCode)
			}))
			defer server.Close()

			resp, err := http.Get(server.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()

			err = printResponse(resp)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestPrintResponse202(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	err = printResponse(resp)
	if err != nil {
		t.Errorf("printResponse returned error for 202: %v", err)
	}
}

func TestParseSendFlags(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantAgent      string
		wantSession    string
		wantIfActive   string
		wantIfInactive string
		wantRest       []string
	}{
		{
			name:     "no flags",
			args:     []string{"hello", "world"},
			wantRest: []string{"hello", "world"},
		},
		{
			name:         "--if-active with value",
			args:         []string{"--if-active", "8h", "hello"},
			wantIfActive: "8h",
			wantRest:     []string{"hello"},
		},
		{
			name:         "--if-active=value",
			args:         []string{"--if-active=30m", "hello"},
			wantIfActive: "30m",
			wantRest:     []string{"hello"},
		},
		{
			name:         "all flags together",
			args:         []string{"-a", "clutch", "-s", "main", "--if-active", "4h", "hello"},
			wantAgent:    "clutch",
			wantSession:  "main",
			wantIfActive: "4h",
			wantRest:     []string{"hello"},
		},
		{
			name:         "--if-active after text",
			args:         []string{"hello", "--if-active", "12h"},
			wantIfActive: "12h",
			wantRest:     []string{"hello"},
		},
		{
			name:     "--if-active without value at end",
			args:     []string{"hello", "--if-active"},
			wantRest: []string{"hello", "--if-active"},
		},
		{
			name:           "--if-inactive with value",
			args:           []string{"--if-inactive", "30m", "hello"},
			wantIfInactive: "30m",
			wantRest:       []string{"hello"},
		},
		{
			name:           "--if-inactive=value",
			args:           []string{"--if-inactive=1h", "hello"},
			wantIfInactive: "1h",
			wantRest:       []string{"hello"},
		},
		{
			name:           "both --if-active and --if-inactive",
			args:           []string{"--if-active", "8h", "--if-inactive", "30m", "hello"},
			wantIfActive:   "8h",
			wantIfInactive: "30m",
			wantRest:       []string{"hello"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, rest := parseSendFlags(tt.args)
			if flags.agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", flags.agent, tt.wantAgent)
			}
			if flags.session != tt.wantSession {
				t.Errorf("session = %q, want %q", flags.session, tt.wantSession)
			}
			if flags.ifActive != tt.wantIfActive {
				t.Errorf("ifActive = %q, want %q", flags.ifActive, tt.wantIfActive)
			}
			if flags.ifInactive != tt.wantIfInactive {
				t.Errorf("ifInactive = %q, want %q", flags.ifInactive, tt.wantIfInactive)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v (len %d), want %v (len %d)", rest, len(rest), tt.wantRest, len(tt.wantRest))
				return
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}

func TestParseAgentFlag(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantAgent string
		wantRest  []string
	}{
		{
			name:      "no flag",
			args:      []string{"hello", "world"},
			wantAgent: "",
			wantRest:  []string{"hello", "world"},
		},
		{
			name:      "-a with value",
			args:      []string{"-a", "research", "hello"},
			wantAgent: "research",
			wantRest:  []string{"hello"},
		},
		{
			name:      "--agent with value",
			args:      []string{"--agent", "main", "hello"},
			wantAgent: "main",
			wantRest:  []string{"hello"},
		},
		{
			name:      "-a=value",
			args:      []string{"-a=scout", "hello"},
			wantAgent: "scout",
			wantRest:  []string{"hello"},
		},
		{
			name:      "--agent=value",
			args:      []string{"--agent=scout", "hello"},
			wantAgent: "scout",
			wantRest:  []string{"hello"},
		},
		{
			name:      "flag after positional args",
			args:      []string{"hello", "world", "-a", "research"},
			wantAgent: "research",
			wantRest:  []string{"hello", "world"},
		},
		{
			name:      "flag in middle",
			args:      []string{"hello", "--agent", "research", "world"},
			wantAgent: "research",
			wantRest:  []string{"hello", "world"},
		},
		{
			name:      "empty args",
			args:      []string{},
			wantAgent: "",
			wantRest:  []string{},
		},
		{
			name:      "-a without value at end",
			args:      []string{"hello", "-a"},
			wantAgent: "",
			wantRest:  []string{"hello", "-a"},
		},
		{
			name:      "only -a and value",
			args:      []string{"-a", "research"},
			wantAgent: "research",
			wantRest:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, rest := parseAgentFlag(tt.args)
			if agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", agent, tt.wantAgent)
			}
			if len(rest) == 0 && len(tt.wantRest) == 0 {
				return // both empty, ok
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v (len %d), want %v (len %d)", rest, len(rest), tt.wantRest, len(tt.wantRest))
				return
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}

func TestSubcommandHelp(t *testing.T) {
	// Each subcommand should return nil (no error) when given -h or --help,
	// without making any HTTP requests.
	base := "http://127.0.0.1:0" // unreachable — proves no HTTP call is made

	cmds := []struct {
		name string
		fn   func(string, []string) error
	}{
		{"send", cmdSend},
		{"branch", cmdBranch},
		{"status", cmdStatus},
		{"eval", cmdEval},
		{"command", cmdCommand},
	}

	for _, cmd := range cmds {
		for _, flag := range []string{"-h", "--help"} {
			t.Run(cmd.name+"/"+flag, func(t *testing.T) {
				err := cmd.fn(base, []string{flag})
				if err != nil {
					t.Errorf("%s %s returned error: %v", cmd.name, flag, err)
				}
			})
		}
	}

	// Test wantsHelp directly for ping (handled in main switch)
	for _, flag := range []string{"-h", "--help"} {
		t.Run("ping/"+flag, func(t *testing.T) {
			if !wantsHelp([]string{flag}) {
				t.Errorf("wantsHelp(%q) = false, want true", flag)
			}
		})
	}
}

func TestWantsHelp(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"-h"}, true},
		{[]string{"--help"}, true},
		{[]string{"-a", "clutch", "-h"}, true},
		{[]string{"--help", "extra"}, true},
		{[]string{"hello"}, false},
		{[]string{"-a", "clutch"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		got := wantsHelp(tt.args)
		if got != tt.want {
			t.Errorf("wantsHelp(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}
