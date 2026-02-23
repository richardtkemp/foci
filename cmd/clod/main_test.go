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
			Agent   string `json:"agent"`
			Session string `json:"session"`
			Text    string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Agent == "nonexistent" {
			http.Error(w, "unknown agent: \"nonexistent\"", http.StatusBadRequest)
			return
		}
		resp := "echo: " + req.Text
		if req.Agent != "" {
			resp = "[" + req.Agent + "] " + resp
		}
		if req.Session != "" {
			resp = "(session:" + req.Session + ") " + resp
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": resp})
	})

	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Agent string `json:"agent"`
			Text  string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Agent == "nonexistent" {
			http.Error(w, "unknown agent: \"nonexistent\"", http.StatusBadRequest)
			return
		}
		resp := "wake ok"
		if req.Agent != "" {
			resp = "[" + req.Agent + "] " + resp
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
		{"send", []string{"send", "hello"}, "echo: hello", false},
		{"branch", []string{"branch"}, "wake ok", false},
		{"wake alias", []string{"wake"}, "wake ok", false},
		{"status", []string{"status"}, "status: idle", false},
		{"ping", []string{"ping"}, "pong", false},
		{"command", []string{"command", "/ping"}, "pong", false},
		{"eval", []string{"eval", "ls -la"}, "echo: Run this command", false},

		// -a flag (space-separated)
		{"send with -a", []string{"send", "-a", "research", "hello"}, "[research] echo: hello", false},
		{"branch with -a", []string{"branch", "-a", "research"}, "[research] wake ok", false},
		{"wake alias with -a", []string{"wake", "-a", "research"}, "[research] wake ok", false},
		{"branch with -a and text", []string{"branch", "-a", "research", "check news"}, "[research] wake ok", false},
		{"wake alias with -a and text", []string{"wake", "-a", "research", "check news"}, "[research] wake ok", false},
		{"status with -a", []string{"status", "-a", "research"}, "[research] status: idle", false},
		{"eval with -a", []string{"eval", "-a", "research", "ls"}, "[research] echo: Run this command", false},
		{"command with -a", []string{"command", "-a", "research", "/ping"}, "[research] pong", false},
		{"ping with -a", []string{"ping", "-a", "research"}, "[research] pong", false},

		// --agent flag (space-separated)
		{"send with --agent", []string{"send", "--agent", "main", "hello"}, "[main] echo: hello", false},

		// --agent=value form
		{"send with --agent=val", []string{"send", "--agent=scout", "hello"}, "[scout] echo: hello", false},

		// -a=value form
		{"send with -a=val", []string{"send", "-a=scout", "hello"}, "[scout] echo: hello", false},

		// -s/--session flag
		{"send with -s", []string{"send", "-s", "research", "hello"}, "(session:research) echo: hello", false},
		{"send with --session", []string{"send", "--session", "feature1", "hello"}, "(session:feature1) echo: hello", false},
		{"send with -s=value", []string{"send", "-s=branch1", "hello"}, "(session:branch1) echo: hello", false},
		{"send with --session=value", []string{"send", "--session=testing", "hello"}, "(session:testing) echo: hello", false},

		// -a and -s flags together
		{"send with -a and -s", []string{"send", "-a", "clutch", "-s", "research", "hello"}, "(session:research) [clutch] echo: hello", false},
		{"send with -s and -a", []string{"send", "-s", "feature", "-a", "clutch", "hello"}, "(session:feature) [clutch] echo: hello", false},
		{"send with -a= and -s=", []string{"send", "-a=clutch", "-s=main", "hello"}, "(session:main) [clutch] echo: hello", false},

		// Flag after positional args
		{"send flag after text", []string{"send", "hello", "-a", "research"}, "[research] echo: hello", false},

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
