package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestScore_ID_Deterministic proves the id is a pure function of the five
// discriminators, so the same score overwrites while a change to any one
// field lands on a different Langfuse score.
func TestScore_ID_Deterministic(t *testing.T) {
	base := Score{TraceID: "trace1", ObservationID: "obs1", Name: "quality", Source: "human", UserID: "alice"}
	if base.ID() != base.ID() {
		t.Fatal("ID() is not stable across calls on the same value")
	}
	variants := []Score{
		{TraceID: "trace2", ObservationID: "obs1", Name: "quality", Source: "human", UserID: "alice"},
		{TraceID: "trace1", ObservationID: "obs2", Name: "quality", Source: "human", UserID: "alice"},
		{TraceID: "trace1", ObservationID: "obs1", Name: "tone", Source: "human", UserID: "alice"},
		{TraceID: "trace1", ObservationID: "obs1", Name: "quality", Source: "judge", UserID: "alice"},
		{TraceID: "trace1", ObservationID: "obs1", Name: "quality", Source: "human", UserID: "bob"},
	}
	seen := map[string]bool{base.ID(): true}
	for i, v := range variants {
		id := v.ID()
		if seen[id] {
			t.Errorf("variant %d collided with a previous id %q", i, id)
		}
		seen[id] = true
	}
}

// TestApiBase covers the OTLP → REST base derivation.
func TestApiBase(t *testing.T) {
	cases := []struct {
		endpoint string
		want     string
	}{
		{"https://cloud.langfuse.com/api/public/otel", "https://cloud.langfuse.com/api/public"},
		{"https://cloud.langfuse.com/api/public/otel/", "https://cloud.langfuse.com/api/public"},
		{"https://cloud.langfuse.com/api/public/other", ""},
		{"https://example.com", ""},
	}
	for _, tc := range cases {
		if got := apiBase(tc.endpoint); got != tc.want {
			t.Errorf("apiBase(%q) = %q, want %q", tc.endpoint, got, tc.want)
		}
	}
}

// recordedRequest is one HTTP call the fake Langfuse server observed.
type recordedRequest struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

// fakeLangfuse is a minimal httptest-backed stand-in for Langfuse's public
// API: /scores (POST) and /score-configs (GET paged, POST create).
type fakeLangfuse struct {
	mu          sync.Mutex
	requests    []recordedRequest
	scoreStatus int // HTTP status /scores responds with; 0 = 200

	// configs served by GET /score-configs (page 1, one page).
	configs []map[string]any
}

func newFakeLangfuse() *fakeLangfuse {
	return &fakeLangfuse{scoreStatus: http.StatusOK}
}

func (f *fakeLangfuse) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		f.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/scores":
			f.mu.Lock()
			status := f.scoreStatus
			f.mu.Unlock()
			if status == 0 {
				status = http.StatusOK
			}
			if status != http.StatusOK {
				http.Error(w, "boom", status)
				return
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/public/score-configs":
			f.mu.Lock()
			data := f.configs
			f.mu.Unlock()
			resp := map[string]any{
				"data": data,
				"meta": map[string]any{"totalPages": 1},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/score-configs":
			id := "cfg-" + fmt.Sprint(body["name"])
			body["id"] = id
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/traces":
			// Never expected to fire in these tests (no spans produced), but
			// answer 200 so a stray export never turns into a warning log.
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeLangfuse) last() recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *fakeLangfuse) countPath(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func setupScoreTest(t *testing.T, endpoint string) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	if err := initWith(exp, Options{Endpoint: endpoint, PublicKey: "pk", SecretKey: "sk", Environment: "test", MaxFieldBytes: 1 << 20}); err != nil {
		t.Fatalf("initWith: %v", err)
	}
	t.Cleanup(resetForTest)
	t.Cleanup(ResetScoreConfigCache)
}

// TestPostScore_Numeric proves the request shape for a numeric score: method,
// path, basic auth, and every body field the gateway relies on.
func TestPostScore_Numeric(t *testing.T) {
	f := newFakeLangfuse()
	srv := f.start(t)
	setupScoreTest(t, srv.URL+"/api/public/otel")

	s := Score{
		TraceID:       TraceIDForTurnHex("agent/c1@123"),
		ObservationID: "aabbccdd11223344",
		Name:          "quality",
		DataType:      ScoreNumeric,
		Value:         4,
		Comment:       "solid turn",
		Source:        "human",
		UserID:        "richard",
		Metadata:      map[string]any{"rubric_version": 1},
	}
	if err := PostScore(context.Background(), s); err != nil {
		t.Fatalf("PostScore: %v", err)
	}

	req := f.last()
	if req.method != http.MethodPost || req.path != "/api/public/scores" {
		t.Fatalf("request = %s %s, want POST /api/public/scores", req.method, req.path)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk:sk"))
	if req.auth != wantAuth {
		t.Errorf("Authorization = %q, want %q", req.auth, wantAuth)
	}
	if req.body["id"] != s.ID() {
		t.Errorf("body id = %v, want %v", req.body["id"], s.ID())
	}
	if req.body["traceId"] != s.TraceID {
		t.Errorf("body traceId = %v, want %v", req.body["traceId"], s.TraceID)
	}
	if req.body["name"] != "quality" {
		t.Errorf("body name = %v, want quality", req.body["name"])
	}
	if req.body["dataType"] != ScoreNumeric {
		t.Errorf("body dataType = %v, want %v", req.body["dataType"], ScoreNumeric)
	}
	if v, ok := req.body["value"].(float64); !ok || v != 4 {
		t.Errorf("body value = %v, want numeric 4", req.body["value"])
	}
	if req.body["comment"] != "solid turn" {
		t.Errorf("body comment = %v, want %q", req.body["comment"], "solid turn")
	}
	if req.body["observationId"] != "aabbccdd11223344" {
		t.Errorf("body observationId = %v, want %v", req.body["observationId"], "aabbccdd11223344")
	}
	if req.body["environment"] != "test" {
		t.Errorf("body environment = %v, want test", req.body["environment"])
	}
	meta, ok := req.body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("body metadata = %v, want a map", req.body["metadata"])
	}
	if meta["source"] != "human" {
		t.Errorf("metadata.source = %v, want human", meta["source"])
	}
	if meta["user_id"] != "richard" {
		t.Errorf("metadata.user_id = %v, want richard", meta["user_id"])
	}
	if fmt.Sprint(meta["rubric_version"]) != "1" {
		t.Errorf("metadata.rubric_version = %v, want 1", meta["rubric_version"])
	}
}

// TestPostScore_Categorical proves a categorical score sends its value as
// the string label, not a number.
func TestPostScore_Categorical(t *testing.T) {
	f := newFakeLangfuse()
	srv := f.start(t)
	setupScoreTest(t, srv.URL+"/api/public/otel")

	s := Score{
		TraceID:     TraceIDForTurnHex("agent/c1@123"),
		Name:        "tone",
		DataType:    ScoreCategorical,
		StringValue: "good",
		Source:      "human",
	}
	if err := PostScore(context.Background(), s); err != nil {
		t.Fatalf("PostScore: %v", err)
	}
	req := f.last()
	if v, ok := req.body["value"].(string); !ok || v != "good" {
		t.Errorf("body value = %v (%T), want string \"good\"", req.body["value"], req.body["value"])
	}
	if req.body["dataType"] != ScoreCategorical {
		t.Errorf("body dataType = %v, want %v", req.body["dataType"], ScoreCategorical)
	}
}

// TestPostScore_ServerError500 proves a non-2xx response surfaces as an
// error naming the HTTP status.
func TestPostScore_ServerError500(t *testing.T) {
	f := newFakeLangfuse()
	f.scoreStatus = http.StatusInternalServerError
	srv := f.start(t)
	setupScoreTest(t, srv.URL+"/api/public/otel")

	s := Score{TraceID: TraceIDForTurnHex("agent/c1@1"), Name: "quality", Source: "human"}
	err := PostScore(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("PostScore error = %v, want it to mention HTTP 500", err)
	}
}

// TestPostScore_TracingDisabled proves PostScore refuses to run once tracing
// has been torn down.
func TestPostScore_TracingDisabled(t *testing.T) {
	f := newFakeLangfuse()
	srv := f.start(t)
	setupScoreTest(t, srv.URL+"/api/public/otel")
	resetForTest() // tear down what setupScoreTest just armed

	s := Score{TraceID: TraceIDForTurnHex("agent/c1@1"), Name: "quality", Source: "human"}
	err := PostScore(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "tracing is not enabled") {
		t.Fatalf("PostScore error = %v, want it to mention tracing is not enabled", err)
	}
}

// TestPostScore_NonLangfuseEndpoint proves an endpoint that isn't shaped like
// Langfuse's OTLP path is rejected before any HTTP call.
func TestPostScore_NonLangfuseEndpoint(t *testing.T) {
	f := newFakeLangfuse()
	srv := f.start(t)
	setupScoreTest(t, srv.URL+"/some/other/path")

	s := Score{TraceID: TraceIDForTurnHex("agent/c1@1"), Name: "quality", Source: "human"}
	err := PostScore(context.Background(), s)
	if err == nil {
		t.Fatal("PostScore over a non-Langfuse endpoint: want an error, got nil")
	}
	if n := f.countPath(http.MethodPost, "/api/public/scores"); n != 0 {
		t.Errorf("PostScore made %d call(s) to /scores over a non-Langfuse endpoint, want 0", n)
	}
}

// TestEnsureScoreConfig covers: an existing config reused without a POST, a
// drift warning when the rubric's shape differs from what's stored, a new
// axis triggering exactly one POST, that POST result being cached so a
// second Ensure of the same axis makes no second POST, and an archived
// config being treated as absent.
func TestEnsureScoreConfig(t *testing.T) {
	f := newFakeLangfuse()
	minV, maxV := 1.0, 5.0
	archivedMin, archivedMax := 0.0, 1.0
	f.configs = []map[string]any{
		{"id": "cfg-quality", "name": "quality", "dataType": "NUMERIC", "minValue": minV, "maxValue": maxV},
		{"id": "cfg-archived", "name": "archived_one", "dataType": "NUMERIC", "minValue": archivedMin, "maxValue": archivedMax, "isArchived": true},
	}
	srv := f.start(t)
	setupScoreTest(t, srv.URL+"/api/public/otel")

	// Existing config, same shape: reused, no warning, no POST.
	id, warning, err := EnsureScoreConfig(context.Background(), ScoreConfig{Name: "quality", DataType: ScoreNumeric, MinValue: &minV, MaxValue: &maxV})
	if err != nil {
		t.Fatalf("EnsureScoreConfig(quality): %v", err)
	}
	if id != "cfg-quality" {
		t.Errorf("id = %q, want cfg-quality", id)
	}
	if warning != "" {
		t.Errorf("warning = %q, want none for a matching shape", warning)
	}
	if n := f.countPath(http.MethodPost, "/api/public/score-configs"); n != 0 {
		t.Fatalf("POST /score-configs called %d time(s) for an existing config, want 0", n)
	}

	// Existing config, different shape: same id, warning mentions the drift.
	otherMax := 10.0
	id, warning, err = EnsureScoreConfig(context.Background(), ScoreConfig{Name: "quality", DataType: ScoreNumeric, MinValue: &minV, MaxValue: &otherMax})
	if err != nil {
		t.Fatalf("EnsureScoreConfig(quality, drifted): %v", err)
	}
	if id != "cfg-quality" {
		t.Errorf("id = %q, want cfg-quality", id)
	}
	if !strings.Contains(warning, "differs") {
		t.Errorf("warning = %q, want it to mention 'differs'", warning)
	}

	// New axis: one POST, id returned.
	id, warning, err = EnsureScoreConfig(context.Background(), ScoreConfig{Name: "new_axis", DataType: ScoreNumeric, MinValue: &minV, MaxValue: &maxV})
	if err != nil {
		t.Fatalf("EnsureScoreConfig(new_axis): %v", err)
	}
	if id != "cfg-new_axis" {
		t.Errorf("id = %q, want cfg-new_axis", id)
	}
	if warning != "" {
		t.Errorf("warning = %q, want none for a freshly created config", warning)
	}
	if n := f.countPath(http.MethodPost, "/api/public/score-configs"); n != 1 {
		t.Fatalf("POST /score-configs called %d time(s) after creating new_axis, want 1", n)
	}

	// Second Ensure of the same new axis: cached, no second POST.
	id2, _, err := EnsureScoreConfig(context.Background(), ScoreConfig{Name: "new_axis", DataType: ScoreNumeric, MinValue: &minV, MaxValue: &maxV})
	if err != nil {
		t.Fatalf("EnsureScoreConfig(new_axis, second): %v", err)
	}
	if id2 != id {
		t.Errorf("second id = %q, want the cached %q", id2, id)
	}
	if n := f.countPath(http.MethodPost, "/api/public/score-configs"); n != 1 {
		t.Fatalf("POST /score-configs called %d time(s) after a second Ensure of the cached axis, want still 1", n)
	}

	// Archived config: not treated as existing, so Ensure creates a new one.
	id, _, err = EnsureScoreConfig(context.Background(), ScoreConfig{Name: "archived_one", DataType: ScoreNumeric, MinValue: &archivedMin, MaxValue: &archivedMax})
	if err != nil {
		t.Fatalf("EnsureScoreConfig(archived_one): %v", err)
	}
	if id != "cfg-archived_one" {
		t.Errorf("id = %q, want a freshly created cfg-archived_one, not the archived cfg-archived", id)
	}
	if n := f.countPath(http.MethodPost, "/api/public/score-configs"); n != 2 {
		t.Errorf("POST /score-configs called %d time(s) total, want 2 (new_axis + archived_one)", n)
	}
}
