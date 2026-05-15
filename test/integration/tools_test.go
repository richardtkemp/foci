//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestL2_Tools_HTTPRequest_ExecBridge proves the exec bridge dispatches
// foci-exposed tools (foci_http_request in this case) correctly when
// cc-stub invokes them via a scripted Bash tool_use. This is the
// generalisation of the cross-agent test for non-routing tools —
// confirming the bridge layer itself works end-to-end without a real
// tool_use coming from a real model.
//
// Mechanism: spin up a side httptest.Server that records inbound
// requests, then script alpha's cc-stub to emit a Bash tool_use that
// runs `foci_http_request <url>`. The shell command reaches foci's
// exec bridge which dispatches to internal/tools/http_request. The
// side server records receipt; the test asserts on its log.
func TestL2_Tools_HTTPRequest_ExecBridge(t *testing.T) {
	// Side HTTP server records inbound request paths for the assertion.
	var (
		mu   sync.Mutex
		hits []string
	)
	side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer side.Close()

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 6666},
		},
		ReadyTimeout: 30 * time.Second,
	})

	// Script alpha's cc-stub so its next user message runs
	// `foci_http_request <side-server-url>/probe`.
	bashCmd := fmt.Sprintf(`foci_http_request %s/probe`, side.URL)
	scriptBody, err := json.Marshal(map[string]any{
		"text": "running http_request",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": bashCmd}},
		},
	})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 6666, Type: "private"},
			From: &gotgbot.User{Id: 6666, FirstName: "Tester"},
			Text: "fetch the probe",
		},
	})

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]string(nil), hits...)
		mu.Unlock()
		for _, p := range got {
			if strings.HasSuffix(p, "/probe") {
				return // pass: exec bridge dispatched the tool, real HTTP request landed
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("foci_http_request never reached the side server; hits=%v\n--- stderr tail ---\n%s",
		hits, stderrTail(h.Stderr()))
}
