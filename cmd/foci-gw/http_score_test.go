package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"foci/internal/evals"
	"foci/internal/telemetry"
)

// Three rubrics shared by every test in this file: a human numeric axis, a
// human categorical axis, and a judge (non-human) boolean axis — enough to
// exercise rubric validation, the free-text fallback (an unregistered
// name), and the "human grade on a judge axis" override-recording path.
const (
	scoreTestQualityRubricMD = "---\n" +
		"kind: human\n" +
		"type: numeric\n" +
		"min: 1\n" +
		"max: 5\n" +
		"---\n" +
		"Rate overall quality.\n"
	scoreTestToneRubricMD = "---\n" +
		"kind: human\n" +
		"type: categorical\n" +
		"categories:\n" +
		"  - label: good\n" +
		"    value: 1\n" +
		"  - label: bad\n" +
		"    value: 0\n" +
		"---\n"
	scoreTestHonestyRubricMD = "---\n" +
		"kind: judge\n" +
		"type: boolean\n" +
		"judge:\n" +
		"  model: gpt-4o\n" +
		"---\n" +
		"Is the reply honest?\n"
)

// fakeGWLangfuse is a minimal httptest-backed stand-in for Langfuse: it
// records every POST /api/public/scores body and answers configurably
// (200 by default), and always answers 200 for the OTLP traces path so
// telemetry.Init's real exporter never logs an export warning.
type fakeGWLangfuse struct {
	mu          sync.Mutex
	scoreReqs   []map[string]any
	scoreStatus int
}

func newFakeGWLangfuse() *fakeGWLangfuse { return &fakeGWLangfuse{scoreStatus: http.StatusOK} }

func (f *fakeGWLangfuse) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/scores":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.scoreReqs = append(f.scoreReqs, body)
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
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/traces"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGWLangfuse) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scoreReqs) == 0 {
		return nil
	}
	return f.scoreReqs[len(f.scoreReqs)-1]
}

func (f *fakeGWLangfuse) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scoreReqs)
}

func (f *fakeGWLangfuse) setStatus(status int) {
	f.mu.Lock()
	f.scoreStatus = status
	f.mu.Unlock()
}

func writeScoreTestRubric(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write rubric %s: %v", name, err)
	}
}

// setupScoreHTTPTest builds the standard harness (httpTestSetup) plus a
// three-rubric registry and a live telemetry install pointed at a fake
// Langfuse server, so /score and /evals/rubrics behave as they would with
// tracing enabled for real. Telemetry is process-global (package
// foci/internal/telemetry keeps package-level state), so callers must not
// run these tests with t.Parallel() against each other — none here do.
func setupScoreHTTPTest(t *testing.T, opts httpTestOpts) (*http.ServeMux, *fakeGWLangfuse) {
	t.Helper()
	d, _ := httpTestSetup(t, opts)

	dir := t.TempDir()
	writeScoreTestRubric(t, dir, "quality", scoreTestQualityRubricMD)
	writeScoreTestRubric(t, dir, "tone", scoreTestToneRubricMD)
	writeScoreTestRubric(t, dir, "honesty", scoreTestHonestyRubricMD)
	reg, err := evals.Load(dir)
	if err != nil {
		t.Fatalf("evals.Load: %v", err)
	}
	d.rubrics = reg

	fl := newFakeGWLangfuse()
	srv := fl.start(t)
	if err := telemetry.Init(context.Background(), telemetry.Options{
		Endpoint: srv.URL + "/api/public/otel", PublicKey: "pk", SecretKey: "sk", Environment: "test",
	}); err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	t.Cleanup(func() { telemetry.Shutdown(context.Background()) })

	return newTestMux(d), fl
}

// TestHandleScore covers the validation and posting behaviour of POST
// /score: rubric-backed values (numeric range check, categorical label
// matching, a human grade overriding a judge axis), the free-text fallback
// for a name with no registered rubric, and the two "bad request" input
// errors.
func TestHandleScore(t *testing.T) {
	const turn = "agent/c1@1700000000000000000"
	wantID := telemetry.Score{TraceID: telemetry.TraceIDForTurnHex(turn), Name: "quality", Source: "human"}.ID()
	wantTrace := telemetry.TraceIDForTurnHex(turn)

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantErrSub string
		check      func(t *testing.T, resp map[string]any, fl *fakeGWLangfuse)
	}{
		{
			name:       "rubric numeric happy path",
			body:       fmt.Sprintf(`{"name":"quality","value":"4","turn":%q}`, turn),
			wantStatus: http.StatusOK,
			check: func(t *testing.T, resp map[string]any, fl *fakeGWLangfuse) {
				if resp["score_id"] != wantID {
					t.Errorf("score_id = %v, want %v", resp["score_id"], wantID)
				}
				if resp["trace_id"] != wantTrace {
					t.Errorf("trace_id = %v, want %v", resp["trace_id"], wantTrace)
				}
				if resp["rubric"] != true {
					t.Errorf("rubric = %v, want true", resp["rubric"])
				}
				if v, _ := resp["value"].(float64); v != 4 {
					t.Errorf("value = %v, want 4", resp["value"])
				}
				if fl.count() != 1 {
					t.Fatalf("langfuse saw %d POST /scores, want 1", fl.count())
				}
				last := fl.last()
				if last["dataType"] != "NUMERIC" {
					t.Errorf("langfuse dataType = %v, want NUMERIC", last["dataType"])
				}
				meta, _ := last["metadata"].(map[string]any)
				if fmt.Sprint(meta["rubric_version"]) != "1" {
					t.Errorf("langfuse metadata.rubric_version = %v, want 1", meta["rubric_version"])
				}
			},
		},
		{
			name:       "rubric numeric value outside range",
			body:       fmt.Sprintf(`{"name":"quality","value":"9","turn":%q}`, turn),
			wantStatus: http.StatusBadRequest,
			wantErrSub: "outside",
		},
		{
			name:       "rubric categorical happy path, case-insensitive",
			body:       fmt.Sprintf(`{"name":"tone","value":"GOOD","turn":%q}`, turn),
			wantStatus: http.StatusOK,
			check: func(t *testing.T, resp map[string]any, fl *fakeGWLangfuse) {
				if resp["label"] != "good" {
					t.Errorf("label = %v, want good", resp["label"])
				}
				last := fl.last()
				if last["dataType"] != "CATEGORICAL" {
					t.Errorf("langfuse dataType = %v, want CATEGORICAL", last["dataType"])
				}
			},
		},
		{
			name:       "human grade on a judge rubric records overrides_kind",
			body:       fmt.Sprintf(`{"name":"honesty","value":"yes","turn":%q}`, turn),
			wantStatus: http.StatusOK,
			check: func(t *testing.T, resp map[string]any, fl *fakeGWLangfuse) {
				last := fl.last()
				meta, _ := last["metadata"].(map[string]any)
				if meta["overrides_kind"] != "judge" {
					t.Errorf("metadata.overrides_kind = %v, want judge", meta["overrides_kind"])
				}
			},
		},
		{
			name:       "unregistered name infers numeric",
			body:       fmt.Sprintf(`{"name":"vibes","value":"7","turn":%q}`, turn),
			wantStatus: http.StatusOK,
			check: func(t *testing.T, resp map[string]any, fl *fakeGWLangfuse) {
				if resp["rubric"] != false {
					t.Errorf("rubric = %v, want false", resp["rubric"])
				}
				if v, _ := resp["value"].(float64); v != 7 {
					t.Errorf("value = %v, want 7", resp["value"])
				}
				last := fl.last()
				if last["dataType"] != "NUMERIC" {
					t.Errorf("langfuse dataType = %v, want NUMERIC", last["dataType"])
				}
			},
		},
		{
			name:       "unregistered name infers categorical",
			body:       fmt.Sprintf(`{"name":"vibes","value":"meh","turn":%q}`, turn),
			wantStatus: http.StatusOK,
			check: func(t *testing.T, resp map[string]any, fl *fakeGWLangfuse) {
				if resp["label"] != "meh" {
					t.Errorf("label = %v, want meh", resp["label"])
				}
				last := fl.last()
				if last["dataType"] != "CATEGORICAL" {
					t.Errorf("langfuse dataType = %v, want CATEGORICAL", last["dataType"])
				}
			},
		},
		{
			name:       "missing name",
			body:       fmt.Sprintf(`{"value":"7","turn":%q}`, turn),
			wantStatus: http.StatusBadRequest,
			wantErrSub: "name is required",
		},
		{
			name:       "missing value",
			body:       fmt.Sprintf(`{"name":"vibes","turn":%q}`, turn),
			wantStatus: http.StatusBadRequest,
			wantErrSub: "value is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux, fl := setupScoreHTTPTest(t, httpTestOpts{})
			w := postJSON(mux, "/score", tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				if !strings.Contains(w.Body.String(), tc.wantErrSub) {
					t.Errorf("body = %q, want it to contain %q", w.Body.String(), tc.wantErrSub)
				}
				return
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v; body: %s", err, w.Body.String())
			}
			if tc.check != nil {
				tc.check(t, resp, fl)
			}
		})
	}
}

// TestHandleScore_MethodNotAllowed proves GET /score is rejected.
func TestHandleScore_MethodNotAllowed(t *testing.T) {
	mux, _ := setupScoreHTTPTest(t, httpTestOpts{})
	req := httptest.NewRequest(http.MethodGet, "/score", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleScore_NoSessionNoTurn proves an agent with no resolvable
// default session and no explicit turn in the request is a 400, not a
// panic or a silent no-op.
func TestHandleScore_NoSessionNoTurn(t *testing.T) {
	mux, _ := setupScoreHTTPTest(t, httpTestOpts{noSession: true})
	w := postJSON(mux, "/score", `{"name":"quality","value":"4"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleScore_LangfuseError502 proves a failing Langfuse call surfaces
// as a 502 (bad gateway), not a 400 — the request was valid, the upstream
// wasn't.
func TestHandleScore_LangfuseError502(t *testing.T) {
	mux, fl := setupScoreHTTPTest(t, httpTestOpts{})
	fl.setStatus(http.StatusInternalServerError)
	w := postJSON(mux, "/score", `{"name":"quality","value":"4","turn":"agent/c1@1700000000000000000"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
}

// evalsRubricsResponse mirrors the wire shape of GET /evals/rubrics enough
// to assert on it without depending on the handler's private rubricJSON type
// name (it's already package-local, but a plain struct keeps the intent of
// what's being asserted obvious).
type evalsRubricsResponse struct {
	Rubrics []struct {
		Name  string `json:"name"`
		Kind  string `json:"kind"`
		Shape string `json:"shape"`
	} `json:"rubrics"`
	Scoring bool `json:"scoring_available"`
}

// TestHandleEvalsRubrics_List proves the full listing is sorted by name,
// renders each type's shape correctly, and reports scoring as available
// once telemetry is pointed at a Langfuse-shaped endpoint.
func TestHandleEvalsRubrics_List(t *testing.T) {
	mux, _ := setupScoreHTTPTest(t, httpTestOpts{})
	req := httptest.NewRequest(http.MethodGet, "/evals/rubrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp evalsRubricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	if !resp.Scoring {
		t.Error("scoring_available = false, want true")
	}
	if len(resp.Rubrics) != 3 {
		t.Fatalf("rubrics = %+v, want 3", resp.Rubrics)
	}
	wantOrder := []string{"honesty", "quality", "tone"}
	shapes := map[string]string{}
	for i, rb := range resp.Rubrics {
		if rb.Name != wantOrder[i] {
			t.Errorf("rubrics[%d].name = %q, want %q (sorted order)", i, rb.Name, wantOrder[i])
		}
		shapes[rb.Name] = rb.Shape
	}
	if shapes["quality"] != "1..5" {
		t.Errorf("quality shape = %q, want 1..5", shapes["quality"])
	}
	if shapes["tone"] != "good|bad" {
		t.Errorf("tone shape = %q, want good|bad", shapes["tone"])
	}
	if shapes["honesty"] != "yes/no" {
		t.Errorf("honesty shape = %q, want yes/no", shapes["honesty"])
	}
}

// TestHandleEvalsRubrics_AgentFilter proves ?agent=X narrows to the
// human-graded rubrics only — the judge rubric never shows up in a score
// control.
func TestHandleEvalsRubrics_AgentFilter(t *testing.T) {
	mux, _ := setupScoreHTTPTest(t, httpTestOpts{})
	req := httptest.NewRequest(http.MethodGet, "/evals/rubrics?agent=someagent", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp evalsRubricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	if len(resp.Rubrics) != 2 {
		t.Fatalf("rubrics = %+v, want 2 (quality, tone — honesty is a judge rubric)", resp.Rubrics)
	}
	for _, rb := range resp.Rubrics {
		if rb.Kind != "human" {
			t.Errorf("rubric %s kind = %q, want human", rb.Name, rb.Kind)
		}
	}
}

// TestHandleEvalsRubrics_NilRegistry proves a nil rubrics registry (the
// gateway's "rubrics dir failed to load" state) renders an empty list
// without panicking.
func TestHandleEvalsRubrics_NilRegistry(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{}) // d.rubrics left nil
	mux := newTestMux(d)
	req := httptest.NewRequest(http.MethodGet, "/evals/rubrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp evalsRubricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	if len(resp.Rubrics) != 0 {
		t.Errorf("rubrics = %+v, want empty for a nil registry", resp.Rubrics)
	}
}
