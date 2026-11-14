package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestCommand_ManaCachedPrintsWholeReply is #3103 requirement 4 (H3, H5) on
// the CLI side: `foci command /mana cached json` sends the command to
// /command and prints the gateway's whole multi-line reply on stdout, with
// no error (exit 0). The gateway side is cmd/foci-gw TestCommand_ManaCached.
func TestCommand_ManaCachedPrintsWholeReply(t *testing.T) {
	reply := "{\n  \"account\": \"claude-code:A\",\n  \"has_figures\": false,\n  \"windows\": []\n}"
	var gotCmd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotCmd, _ = body["command"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"response": reply, "session": "agent:clutch:main", "resolved_via": "default"})
	}))
	defer srv.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := dispatchCommand(srv.URL, commandFlags{noWait: true}, "clutch", []string{"/mana", "cached", "json"})
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)

	if runErr != nil {
		t.Fatalf("dispatchCommand: %v (want exit 0)", runErr)
	}
	if gotCmd != "/mana cached json" {
		t.Errorf("command sent = %q", gotCmd)
	}
	if string(out) != reply+"\n" {
		t.Errorf("stdout = %q, want the whole reply", out)
	}
}
