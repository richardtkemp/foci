package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foci/internal/command"
)

// TestCommandDispatchedFromHTTP_CarriesHTTPSource proves the two HTTP command
// dispatch sites stamp the transport on the command.Request (#1318 review F1):
// /send's slash short-circuit and POST /command both dispatch with
// Source = command.SourceHTTP — the identity surface-aware rendering keys on
// (/help teaches the /steer|/queue chat prefixes only on telegram/discord, so
// an HTTP-originated command must never masquerade as a chat bot or as the
// app). The probe command's reply echoes the Source it was dispatched with.
func TestCommandDispatchedFromHTTP_CarriesHTTPSource(t *testing.T) {
	t.Parallel()
	echo := &command.Command{
		Name:        "sourceecho",
		Description: "echo the dispatch source (test probe)",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "source=" + req.Source}, nil
		},
	}
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{echo}})
	mux := newTestMux(d)

	for _, tc := range []struct {
		name string
		post func() *httptest.ResponseRecorder
	}{
		{"send slash short-circuit", func() *httptest.ResponseRecorder {
			return postJSON(mux, "/send", `{"text":"/sourceecho","async":true}`)
		}},
		{"command endpoint", func() *httptest.ResponseRecorder {
			return postJSON(mux, "/command", `{"command":"/sourceecho"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.post()
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "source=http") {
				t.Errorf("body = %q, want the command dispatched with Source=http", w.Body.String())
			}
		})
	}
}
