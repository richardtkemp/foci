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
func mockGateway() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Text string }
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": "echo: " + req.Text})
	})

	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": "wake ok"})
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": "status: idle"})
	})

	mux.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Command string }
		json.NewDecoder(r.Body).Decode(&req)
		if req.Command == "/ping" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"response": "pong"})
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
		{"wake", []string{"wake"}, "wake ok", false},
		{"status", []string{"status"}, "status: idle", false},
		{"ping", []string{"ping"}, "pong", false},
		{"command", []string{"command", "/ping"}, "pong", false},
		{"eval", []string{"eval", "ls -la"}, "echo: Run this command", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build the CLI binary
			binPath := t.TempDir() + "/clod"
			build := exec.Command("go", "build", "-o", binPath, ".")
			build.Dir = "."
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build failed: %s\n%s", err, out)
			}

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
