package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Scores are Langfuse's evaluation primitive: a named value attached to a
// trace (or one observation of it) after the fact. Unlike observations they
// are upsertable by id, so every score here carries a DETERMINISTIC id —
// re-scoring the same axis on the same target by the same source overwrites
// rather than accumulates. Scores go over Langfuse's public REST API, not
// OTLP (the OTel protocol has no score concept); the API base is derived from
// the OTLP endpoint, which for Langfuse is <api base>/otel.

// Score data types, as Langfuse names them.
const (
	ScoreNumeric     = "NUMERIC"
	ScoreBoolean     = "BOOLEAN"
	ScoreCategorical = "CATEGORICAL"
)

// Score is one evaluation of a trace or observation.
type Score struct {
	// TraceID is the Langfuse trace (hex); use TraceIDForTurn for a turn.
	TraceID string
	// ObservationID scopes the score to one observation (hex span id); empty
	// scores the whole trace.
	ObservationID string
	// Name is the axis (rubric name). Free text, but keep it stable across
	// versions — the dashboard aggregates by it.
	Name string
	// DataType is NUMERIC, BOOLEAN or CATEGORICAL. Numeric/boolean carry Value;
	// categorical carries StringValue.
	DataType    string
	Value       float64
	StringValue string
	// Comment is the rationale (a judge's reasoning, a human's note).
	Comment string
	// Source names who produced it: "human", "judge", "derive". Part of the
	// id, so a human and a judge can both score one axis on one trace.
	Source string
	// UserID identifies the human or judge model; part of the id for humans.
	UserID string
	// ConfigID links the score to its Langfuse score config (optional).
	ConfigID string
	// Metadata is stored verbatim (rubric version, judge model, ...).
	Metadata map[string]any
}

// ID is the deterministic score id: one score per (target, axis, source,
// user). The 32-hex form matches trace ids, which Langfuse accepts as an id.
func (s Score) ID() string {
	h := digest("foci:score:", strings.Join([]string{s.TraceID, s.ObservationID, s.Name, s.Source, s.UserID}, "\x00"))
	return fmt.Sprintf("%x", h[:16])
}

// TraceIDForTurnHex is TraceIDForTurn as the hex string the REST API wants.
func TraceIDForTurnHex(turnID string) string { return TraceIDForTurn(turnID).String() }

var scoreClient = &http.Client{Timeout: 15 * time.Second}

// apiBase derives the REST base ("https://host/api/public") from the OTLP
// endpoint ("https://host/api/public/otel"). Empty when the endpoint is not
// Langfuse-shaped — scoring is then unavailable, tracing unaffected.
func apiBase(endpoint string) string {
	e := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(e, "/otel") {
		return strings.TrimSuffix(e, "/otel")
	}
	return ""
}

// ScoringAvailable reports whether scores can be posted: tracing is on and
// the endpoint is Langfuse's.
func ScoringAvailable() bool {
	_, o, _, ok := current()
	return ok && apiBase(o.Endpoint) != ""
}

// PostScore upserts s. Synchronous (one HTTP round trip) — call it from a
// command handler or a batch pass, never from the turn path.
func PostScore(ctx context.Context, s Score) error {
	_, o, _, ok := current()
	if !ok {
		return fmt.Errorf("tracing is not enabled")
	}
	base := apiBase(o.Endpoint)
	if base == "" {
		return fmt.Errorf("tracing endpoint %q is not a Langfuse OTLP endpoint; scores need <host>/api/public/otel", o.Endpoint)
	}
	if s.TraceID == "" || s.Name == "" {
		return fmt.Errorf("score needs a trace id and a name")
	}
	if s.DataType == "" {
		s.DataType = ScoreNumeric
	}
	body := map[string]any{
		"id":       s.ID(),
		"traceId":  s.TraceID,
		"name":     s.Name,
		"dataType": s.DataType,
	}
	if s.DataType == ScoreCategorical {
		body["value"] = s.StringValue
	} else {
		body["value"] = s.Value
	}
	if s.ObservationID != "" {
		body["observationId"] = s.ObservationID
	}
	if s.Comment != "" {
		body["comment"] = s.Comment
	}
	if s.ConfigID != "" {
		body["configId"] = s.ConfigID
	}
	if o.Environment != "" {
		body["environment"] = o.Environment
	}
	meta := map[string]any{"source": s.Source}
	if s.UserID != "" {
		meta["user_id"] = s.UserID
	}
	for k, v := range s.Metadata {
		meta[k] = v
	}
	body["metadata"] = meta
	return langfusePost(ctx, o, base+"/scores", body, nil)
}

// ScoreConfig mirrors a Langfuse score config: the declared shape of one
// axis, which the UI uses to render an annotation control.
type ScoreConfig struct {
	ID          string          `json:"id,omitempty"`
	Name        string          `json:"name"`
	DataType    string          `json:"dataType"`
	Description string          `json:"description,omitempty"`
	MinValue    *float64        `json:"minValue,omitempty"`
	MaxValue    *float64        `json:"maxValue,omitempty"`
	Categories  []ScoreCategory `json:"categories,omitempty"`
	IsArchived  bool            `json:"isArchived,omitempty"`
}

// ScoreCategory is one label/value pair of a categorical config.
type ScoreCategory struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

var (
	configMu    sync.Mutex
	configCache map[string]ScoreConfig // name → config, filled on first Ensure
)

// EnsureScoreConfig returns the id of the Langfuse score config named
// c.Name, creating it when absent. An existing config is never modified — the
// API cannot update one, and a silently diverging shape would be worse than
// a logged one — so a mismatch between the rubric and the stored config is
// returned as a warning string alongside the id.
func EnsureScoreConfig(ctx context.Context, c ScoreConfig) (id string, warning string, err error) {
	_, o, _, ok := current()
	if !ok {
		return "", "", fmt.Errorf("tracing is not enabled")
	}
	base := apiBase(o.Endpoint)
	if base == "" {
		return "", "", fmt.Errorf("tracing endpoint is not Langfuse's; score configs unavailable")
	}
	configMu.Lock()
	defer configMu.Unlock()
	if configCache == nil {
		configCache, err = listScoreConfigs(ctx, o, base)
		if err != nil {
			configCache = nil
			return "", "", err
		}
	}
	if have, ok := configCache[c.Name]; ok {
		return have.ID, configDrift(c, have), nil
	}
	var created ScoreConfig
	if err := langfusePost(ctx, o, base+"/score-configs", c, &created); err != nil {
		return "", "", err
	}
	configCache[c.Name] = created
	return created.ID, "", nil
}

func configDrift(want, have ScoreConfig) string {
	var diffs []string
	if want.DataType != have.DataType {
		diffs = append(diffs, fmt.Sprintf("dataType %s≠%s", want.DataType, have.DataType))
	}
	if want.DataType == ScoreNumeric {
		if !floatPtrEq(want.MinValue, have.MinValue) || !floatPtrEq(want.MaxValue, have.MaxValue) {
			diffs = append(diffs, "numeric range differs")
		}
	}
	if want.DataType == ScoreCategorical && len(want.Categories) != len(have.Categories) {
		diffs = append(diffs, fmt.Sprintf("%d categories≠%d", len(want.Categories), len(have.Categories)))
	}
	if len(diffs) == 0 {
		return ""
	}
	return "rubric differs from the existing Langfuse score config (" + strings.Join(diffs, ", ") + "); archive the config in the UI to recreate it"
}

func floatPtrEq(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func listScoreConfigs(ctx context.Context, o Options, base string) (map[string]ScoreConfig, error) {
	out := map[string]ScoreConfig{}
	for page := 1; page <= 50; page++ {
		var resp struct {
			Data []ScoreConfig `json:"data"`
			Meta struct {
				TotalPages int `json:"totalPages"`
			} `json:"meta"`
		}
		if err := langfuseGet(ctx, o, fmt.Sprintf("%s/score-configs?limit=100&page=%d", base, page), &resp); err != nil {
			return nil, err
		}
		for _, c := range resp.Data {
			if !c.IsArchived {
				out[c.Name] = c
			}
		}
		if page >= resp.Meta.TotalPages {
			break
		}
	}
	return out, nil
}

func langfuseAuth(o Options) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(o.PublicKey+":"+o.SecretKey))
}

func langfusePost(ctx context.Context, o Options, url string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", langfuseAuth(o))
	req.Header.Set("Content-Type", "application/json")
	return langfuseDo(req, out)
}

func langfuseGet(ctx context.Context, o Options, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", langfuseAuth(o))
	return langfuseDo(req, out)
}

func langfuseDo(req *http.Request, out any) error {
	resp, err := scoreClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("langfuse %s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}
