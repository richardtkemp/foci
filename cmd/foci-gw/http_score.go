package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"foci/internal/evals"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/telemetry"
)

// scoreRequest is POST /score's body. Exactly what the /score chat command
// and the app's score control will send too, so the three surfaces share
// one validation path.
type scoreRequest struct {
	Agent   string `json:"agent"`             // resolved like every other endpoint; "" = the first agent
	Session string `json:"session,omitempty"` // default: the agent's default session
	Turn    string `json:"turn,omitempty"`    // api.db turn_id; default: the session's last completed turn
	// Observation scopes the score to one span of the turn (a tool call or
	// subagent, by its hex span id from Langfuse); empty = the whole trace.
	Observation string `json:"observation,omitempty"`
	Name        string `json:"name"`
	Value       string `json:"value"` // validated against the rubric when one exists
	Comment     string `json:"comment,omitempty"`
	User        string `json:"user,omitempty"` // who graded; part of the score id
}

type scoreResponse struct {
	ScoreID string  `json:"score_id"`
	TraceID string  `json:"trace_id"`
	TurnID  string  `json:"turn_id"`
	Session string  `json:"session"`
	Name    string  `json:"name"`
	Value   float64 `json:"value"`
	Label   string  `json:"label,omitempty"`
	Rubric  bool    `json:"rubric"` // false = free-text axis, no rubric validated it
}

// handleScore returns the handler for POST /score: a human score on a turn.
func handleScore(d httpHandlerDeps, resolveAgent agentResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req scoreRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := scoreTurn(d, resolveAgent, req)
		if err != nil {
			status := http.StatusBadRequest
			if strings.HasPrefix(err.Error(), "langfuse ") || strings.HasPrefix(err.Error(), "tracing ") {
				status = http.StatusBadGateway
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// scoreTurn resolves the target turn, validates the value against the
// rubric (when one is registered under that name), and posts the score.
// The value is written exactly as validated — this path is a pen, not an
// interpreter: no agent, no model, sits between the grader and the record.
func scoreTurn(d httpHandlerDeps, resolveAgent agentResolver, req scoreRequest) (*scoreResponse, error) {
	if !telemetry.ScoringAvailable() {
		return nil, fmt.Errorf("tracing is off or not pointed at Langfuse; scores need [tracing] enabled with a <host>/api/public/otel endpoint")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	inst, ok := resolveAgent(req.Agent)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", req.Agent)
	}
	sk := req.Session
	if sk == "" {
		sk = defaultSessionKey(d, inst.id)
	}
	if sk == "" && req.Turn == "" {
		return nil, fmt.Errorf("no session for agent %s; pass session or turn", inst.id)
	}
	turn := req.Turn
	if turn == "" {
		turn = telemetry.LastTurnID(sk)
	}
	if turn == "" {
		turn = log.LastTurnIDForSession(sk)
	}
	if turn == "" {
		return nil, fmt.Errorf("session %s has no completed turn to score", sk)
	}
	if sk == "" {
		sk, _, _ = strings.Cut(turn, "@")
	}

	s := telemetry.Score{
		TraceID:       telemetry.TraceIDForTurnHex(turn),
		ObservationID: req.Observation,
		Name:          req.Name,
		Comment:       strings.TrimSpace(req.Comment),
		Source:        "human",
		UserID:        strings.TrimSpace(req.User),
		Metadata:      map[string]any{"turn_id": turn, "session": sk},
	}
	resp := &scoreResponse{TraceID: s.TraceID, TurnID: turn, Session: sk, Name: req.Name}

	if d.rubrics != nil {
		if rb, ok := d.rubrics.Get(req.Name); ok {
			v, label, err := rb.Validate(req.Value)
			if err != nil {
				return nil, err
			}
			s.Value, s.StringValue, s.ConfigID = v, label, rb.ConfigID
			s.DataType = map[evals.Type]string{
				evals.TypeNumeric: telemetry.ScoreNumeric, evals.TypeBoolean: telemetry.ScoreBoolean, evals.TypeCategorical: telemetry.ScoreCategorical,
			}[rb.Type]
			s.Metadata["rubric_version"] = rb.Version
			resp.Rubric, resp.Value, resp.Label = true, v, label
			if rb.Kind != evals.KindHuman {
				// Allowed — a human grade on a judge axis is calibration data —
				// but recorded as such so the two never get averaged blind.
				s.Metadata["overrides_kind"] = string(rb.Kind)
			}
		}
	}
	if !resp.Rubric {
		// No rubric: free-text axis. Numbers are numeric, yes/no boolean,
		// anything else a category — the shape is inferred, never enforced.
		raw := strings.TrimSpace(req.Value)
		var v float64
		switch {
		case raw == "":
			return nil, fmt.Errorf("value is required")
		case strings.EqualFold(raw, "yes") || strings.EqualFold(raw, "true"):
			s.DataType, s.Value = telemetry.ScoreBoolean, 1
		case strings.EqualFold(raw, "no") || strings.EqualFold(raw, "false"):
			s.DataType, s.Value = telemetry.ScoreBoolean, 0
		default:
			if _, err := fmt.Sscanf(raw, "%g", &v); err == nil {
				s.DataType, s.Value = telemetry.ScoreNumeric, v
			} else {
				s.DataType, s.StringValue = telemetry.ScoreCategorical, raw
			}
		}
		resp.Value, resp.Label = s.Value, s.StringValue
	}
	if err := telemetry.PostScore(d.ctx, s); err != nil {
		return nil, err
	}
	resp.ScoreID = s.ID()
	httpLog.Infof("score %s=%s on %s by %q (rubric=%v)", req.Name, req.Value, turn, req.User, resp.Rubric)
	return resp, nil
}

// rubricJSON is one row of GET /evals/rubrics.
type rubricJSON struct {
	Name        string   `json:"name"`
	Version     int      `json:"version"`
	Kind        string   `json:"kind"`
	Type        string   `json:"type"`
	Shape       string   `json:"shape"`
	Description string   `json:"description,omitempty"`
	Agents      []string `json:"agents,omitempty"`
	ConfigID    string   `json:"config_id,omitempty"`
	Path        string   `json:"path"`
}

// handleEvalsRubrics returns the handler for GET /evals/rubrics: the loaded
// registry plus any files that failed to load. ?agent=X&session_type=chat
// narrows to the human rubrics that apply there (what a score control shows).
func handleEvalsRubrics(d httpHandlerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		out := struct {
			Dir     string            `json:"dir"`
			Rubrics []rubricJSON      `json:"rubrics"`
			Errors  map[string]string `json:"errors,omitempty"`
			Scoring bool              `json:"scoring_available"`
		}{Scoring: telemetry.ScoringAvailable(), Errors: map[string]string{}}
		if d.rubrics != nil {
			out.Dir = d.rubrics.Dir()
			var rs []*evals.Rubric
			if agent := r.URL.Query().Get("agent"); agent != "" {
				st := r.URL.Query().Get("session_type")
				if st == "" {
					st = string(session.SessionTypeChat)
				}
				rs = d.rubrics.Human(agent, st)
			} else {
				rs = d.rubrics.List()
			}
			for _, rb := range rs {
				out.Rubrics = append(out.Rubrics, rubricJSON{
					Name: rb.Name, Version: rb.Version, Kind: string(rb.Kind), Type: string(rb.Type),
					Shape: rb.Shape(), Description: rb.Description, Agents: rb.Select.Agents,
					ConfigID: rb.ConfigID, Path: rb.Path,
				})
			}
			for p, err := range d.rubrics.Errors() {
				out.Errors[p] = err.Error()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}
