package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/secrets"
	"foci/internal/telemetry"
	"foci/internal/turnevent"
)

// TestInitTracingRedactsLiveAndOverrideSecrets proves end to end that the
// trace redactor reads the LIVE secrets store, not a list copied at startup:
// one turn exported through the public telemetry API must scrub the startup
// global value, the [agents.<id>.*] override value AND a value added to
// secrets.toml after initTracing ran, while the plain marker word still
// arrives (positive control: the export happened and the startup-read
// Langfuse auth pair worked). On the pre-#2249 code the override and the
// added value leak: initTracing copied Names() (globals only) into a
// one-time slice. Telemetry is process-global, so this test must not run
// parallel to anything else arming it, and the cleanup must always run.
func TestInitTracingRedactsLiveAndOverrideSecrets(t *testing.T) {
	// The exporter is built without WithCompression; clear any ambient env
	// that could turn it on (the handler below gunzips regardless).
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "")

	// Made-up values: >= 8 chars, lower-case letters and digits only, none
	// a substring of another, so only value-based redaction can scrub them.
	const (
		startupGlobal = "globalval01" // custom.global_key, in the file at Load
		agentOverride = "agentover01" // agents.helper.custom.agent_key, in the file at Load
		addedLater    = "lateadded01" // extra.added, appended AFTER initTracing
		marker        = "markerword"  // not a secret — proves the export arrived
	)

	path := filepath.Join(t.TempDir(), "secrets.toml")
	base := `[langfuse]
public_key = "publickey01"
secret_key = "secretkey01"

[custom]
global_key = "` + startupGlobal + `"

[agents.helper.custom]
agent_key = "` + agentOverride + `"
`
	if err := os.WriteFile(path, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var mu sync.Mutex
	var bodies []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/traces") {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body, err = io.ReadAll(zr)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		mu.Lock()
		bodies = append(bodies, body...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK) // empty body = a valid empty ExportResponse
	}))
	defer srv.Close()

	cfg := &config.Config{Tracing: config.TracingConfig{
		Enabled:  true,
		Endpoint: srv.URL, // Content nil → content export on (the default)
	}}
	cleanup := initTracing(context.Background(), cfg, store)
	defer cleanup()

	// A second global secret, added after initTracing — the store is live,
	// so the very next value read must see it.
	after := base + "\n[extra]\nadded = \"" + addedLater + "\"\n"
	if err := os.WriteFile(path, []byte(after), 0600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour) // the size differs too; the stamp just has to differ
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// One turn through the public API, the way the orchestrator drives it.
	sink, turn := telemetry.NewTurnSink(turnevent.NopSink{})
	if turn == nil {
		cleanup()
		t.Fatal("NewTurnSink returned nil *Turn while tracing enabled")
	}
	turn.Begin(telemetry.TurnInfo{
		TurnID:     "gw/e2e@1700000000000000000",
		SessionKey: "gw/e2e",
		AgentID:    "gw",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	turn.SetInput("prompt "+marker+" "+startupGlobal+" "+agentOverride+" "+addedLater, "claude-opus-5")
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done " + marker, Model: "claude-opus-5"})

	cleanup() // Shutdown flushes the batch; CAS-idempotent, so the deferred call is safe

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no OTLP export reached the test server — the turn never arrived")
	}
	if !bytes.Contains(bodies, []byte(marker)) {
		t.Error("the recorded OTLP payload lacks the plain marker word — the export or its content attributes did not arrive")
	}
	for key, val := range map[string]string{
		"custom.global_key":              startupGlobal,
		"agents.helper.custom.agent_key": agentOverride,
		"extra.added":                    addedLater,
	} {
		if bytes.Contains(bodies, []byte(val)) {
			t.Errorf("the recorded OTLP payload leaks secret %s (exported field langfuse.observation.input)", key)
		}
	}
}
