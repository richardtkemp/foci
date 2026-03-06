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

// mockGateway creates a test server that mimics the foci HTTP gateway.
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



func TestCLIEnvVars(t *testing.T) {
	server := mockGateway()
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	// Create temp file for -mf tests
	msgFile := t.TempDir() + "/msg.md"
	os.WriteFile(msgFile, []byte("env file msg"), 0644)

	// Build the CLI binary
	binPath := t.TempDir() + "/foci"
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

	// Standalone commands (no base URL needed)
	standaloneHelp := []struct {
		name string
		fn   func([]string) error
	}{
		{"auth", cmdAuth},
		{"setup", cmdSetup},
		{"secrets", cmdSecrets},
	}

	for _, cmd := range standaloneHelp {
		for _, flag := range []string{"-h", "--help"} {
			t.Run(cmd.name+"/"+flag, func(t *testing.T) {
				err := cmd.fn([]string{flag})
				if err != nil {
					t.Errorf("%s %s returned error: %v", cmd.name, flag, err)
				}
			})
		}
	}

	// Secrets subcommands
	for _, sub := range []string{"list", "get", "set", "delete"} {
		for _, flag := range []string{"-h", "--help"} {
			t.Run("secrets_"+sub+"/"+flag, func(t *testing.T) {
				err := cmdSecrets([]string{sub, flag})
				if err != nil {
					t.Errorf("secrets %s %s returned error: %v", sub, flag, err)
				}
			})
		}
	}
}

func TestVersionCommand(t *testing.T) {
	// Build the CLI binary
	binPath := t.TempDir() + "/foci"
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	for _, arg := range []string{"version", "--version", "-v"} {
		t.Run(arg, func(t *testing.T) {
			cmd := exec.Command(binPath, arg)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", arg, err, out)
			}
			output := string(out)
			if !strings.HasPrefix(output, "foci ") {
				t.Errorf("output %q does not start with 'foci '", output)
			}
		})
	}
}

func TestHelpCommand(t *testing.T) {
	// Build the CLI binary
	binPath := t.TempDir() + "/foci"
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			cmd := exec.Command(binPath, arg)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", arg, err, out)
			}
			output := string(out)
			if !strings.Contains(output, "Usage:") {
				t.Errorf("output %q does not contain 'Usage:'", output)
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

