package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/configapprove"
	"foci/internal/configcheck"
	"foci/internal/platform"
)

// config_approve.go wires the agent config tool (#479): the approval service
// (internal/configapprove), its HTTP API — POST /config/request to ask,
// GET /config/request?id=…&wait=N to follow — and the hook the command
// registry uses for /config set over HTTP. The prompt goes to the requesting
// agent's owner's default chat, the chat its other approval prompts use.

// gwConfigApprove is the process-wide approval service, set in main before
// the HTTP server starts. The command registry reaches it through
// requestConfigApproval.
var gwConfigApprove *configapprove.Service

// configRequestPollMax caps one long-poll call below the HTTP server's 30s
// WriteTimeout, so a poll always completes as a normal response.
const configRequestPollMax = 20 * time.Second

// newConfigApproveService builds the service for the gateway's config file.
func newConfigApproveService(cfg *config.Config, configPath string, agents map[string]*agentInstance, connMgr platform.ConnectionManager) *configapprove.Service {
	mode, err := config.ParseFileMode(cfg.FileMode)
	if err != nil {
		mode = 0o600
	}
	return configapprove.New(configapprove.Deps{
		ConfigPath:     configPath,
		FileMode:       mode,
		CheckProtected: gwConfigProtect,
		CheckContent:   configcheck.CheckContent,
		Present: func(p configapprove.Prompt) error {
			inst := agents[p.AgentID]
			if inst == nil {
				return fmt.Errorf("unknown agent %q", p.AgentID)
			}
			target := defaultSessionKeyFor(inst.ag, p.AgentID)
			resolve := connResolver(connMgr, target, p.AgentID)
			if resolve() == nil {
				return fmt.Errorf("no chat connection for %s", target)
			}
			buttons := []platform.ButtonChoice{{Label: "Approve", Data: "approve"}, {Label: "Deny", Data: "deny"}}
			_, err := platform.SendInteractiveMessageWithID(resolve, p.ID, p.Summary, p.Text, buttons,
				func(choice platform.ButtonChoice) string { return p.OnAnswer(choice.Data == "approve") },
				p.OnExpire)
			return err
		},
		Close: func(id, finalText string) {
			if err := platform.CancelInteractiveMessage(id, finalText); err != nil {
				configLog.Warnf("config request %s: edit timed-out prompt: %v", id, err)
			}
		},
		Apply: func(p config.KeyPath) {
			if gwLiveApply == nil {
				return
			}
			section, key := p.LiveApplyAddress()
			if _, err := gwLiveApply.Apply(section, key); err != nil {
				configLog.Warnf("%s written but live apply failed (takes effect on restart): %v", p, err)
			}
		},
	})
}

// requestConfigApproval is the command registry's ConfigApprovalFn: the
// agent is the session key's first segment.
func requestConfigApproval(sessionKey string, op configapprove.Op, path, value string) configapprove.Status {
	if gwConfigApprove == nil {
		return configapprove.Status{Outcome: configapprove.Failed, Reason: "config approval is not available"}
	}
	agentID, _, _ := strings.Cut(sessionKey, "/")
	return gwConfigApprove.Submit(configapprove.Submission{AgentID: agentID, SessionKey: sessionKey, Op: op, Path: path, Value: value})
}

// configRequestBody is POST /config/request's body.
type configRequestBody struct {
	Session     string `json:"session"` // the requesting session key; its first segment is the agent
	Op          string `json:"op"`      // set | unset
	Path        string `json:"path"`
	Value       string `json:"value,omitempty"`
	TimeoutSecs int    `json:"timeout_secs,omitempty"`
}

// handleConfigRequest serves /config/request: POST submits a change and
// answers at once with its status (pending, or failed when it did not pass
// validation); GET ?id=…&wait=N returns the status once it is no longer
// pending or after N seconds (capped below the server's WriteTimeout). An
// unknown id answers 404 with outcome failed: the gateway restarted.
func handleConfigRequest(svc func() *configapprove.Service, agentKnown func(string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := svc()
		if s == nil {
			http.Error(w, "config approval is not available", http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodPost:
			var body configRequestBody
			if !decodeCappedJSON(w, r, &body) {
				return
			}
			agentID, _, _ := strings.Cut(body.Session, "/")
			if agentID == "" || !agentKnown(agentID) {
				http.Error(w, fmt.Sprintf("bad request: session %q names no known agent", body.Session), http.StatusBadRequest)
				return
			}
			st := s.Submit(configapprove.Submission{
				AgentID:    agentID,
				SessionKey: body.Session,
				Op:         configapprove.Op(body.Op),
				Path:       body.Path,
				Value:      body.Value,
				Timeout:    time.Duration(body.TimeoutSecs) * time.Second,
			})
			writeJSON(w, http.StatusOK, st)
		case http.MethodGet:
			id := r.URL.Query().Get("id")
			if id == "" {
				http.Error(w, "bad request: need ?id=", http.StatusBadRequest)
				return
			}
			wait := configRequestPollMax
			if v := r.URL.Query().Get("wait"); v != "" {
				secs, err := strconv.Atoi(v)
				if err != nil || secs < 0 {
					http.Error(w, "bad request: wait must be whole seconds", http.StatusBadRequest)
					return
				}
				wait = min(time.Duration(secs)*time.Second, configRequestPollMax)
			}
			st, known := s.Wait(r.Context(), id, wait)
			code := http.StatusOK
			if !known {
				code = http.StatusNotFound
			}
			writeJSON(w, code, st)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// writeJSON writes v as a JSON response.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		httpLog.Debugf("write JSON response: %v", err)
	}
}
