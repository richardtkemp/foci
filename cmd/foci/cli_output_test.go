package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestPrintResponse202 verifies that HTTP 202 (Accepted) responses are printed without error.
func TestPrintResponse202(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status": "queued"}`))
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

// TestPrintResponseError verifies that error status codes produce formatted error messages.
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

// TestResolveMessage tests message resolution from flags, files, and trailing arguments.
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
