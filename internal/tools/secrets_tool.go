package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"foci/internal/secretsapprove"
	"foci/internal/session"
)

// SecretsRequester is the gateway's secret-approval service as the secrets
// tool sees it (#3090).
type SecretsRequester interface {
	// Submit asks the user; done gets the outcome once, later.
	Submit(agentID string, req secretsapprove.Request, done func(secretsapprove.Result)) (string, error)
	// List renders names and rules, never values.
	List() string
}

// NewSecretsTool creates the secrets tool (foci_secrets, #3090): the agent asks
// to change secrets.toml and the user approves each write in a foci app
// dialog, typing the value for a set there. The agent never handles a value.
// requester is resolved per call (the service is built after the agents).
// notifier delivers each outcome to the asking session as a later message.
func NewSecretsTool(requester func() SecretsRequester, notifier *AsyncNotifier) *Tool {
	return &Tool{
		Name: "secrets",
		Description: "Manage foci's secrets (secrets.toml) with the user's approval. You never see or type a secret value. " +
			"`list` shows names and their rules (allowed hosts, allowed_in_body keys, agent rule) and needs no approval. " +
			"Every other action opens a dialog in the user's foci app: `set` asks the user to approve and then type the value; " +
			"the rest ask Approve/Deny. The call returns at once; the outcome (applied, denied, cancelled, timed_out or failed) " +
			"arrives later as a message. Actions: " + secretsapprove.Usage + ". " +
			"--allowed-hosts replaces the section's host list (exact hostnames the secret may be sent to); " +
			"--allowed-agents makes the section visible only to those agents. Omitted, each stays as it is.",
		ExecExport: true,
		Positional: []string{"action"},
		RestParam:  "args",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["list", "set", "remove", "hosts", "body", "agents"], "description": "What to do"},
				"args": {"type": "array", "items": {"type": "string"}, "description": "The action's words, as in the shell form: set hetzner.api_key; hosts hetzner add api.hetzner.cloud; agents hetzner allow clutch,scout"},
				"allowed_hosts": {"type": "string", "description": "set only: comma-separated hostnames; replaces the section's allowed_hosts"},
				"allowed_agents": {"type": "string", "description": "set only: comma-separated agent ids; the section becomes visible only to them"}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
			var p struct {
				Action        string   `json:"action"`
				Args          []string `json:"args"`
				AllowedHosts  string   `json:"allowed_hosts"`
				AllowedAgents string   `json:"allowed_agents"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return ToolResult{}, fmt.Errorf("invalid arguments: %w", err)
			}
			svc := requester()
			if svc == nil {
				return ToolResult{}, errors.New("secret requests are not available on this gateway")
			}
			if p.Action == "list" {
				if len(p.Args) > 0 || p.AllowedHosts != "" || p.AllowedAgents != "" {
					return ToolResult{}, errors.New("list takes no arguments")
				}
				return TextResult(svc.List()), nil
			}
			if IsSpawnCharacter(ctx) {
				return ToolResult{}, errSpawnNeedsUser("secrets")
			}
			req, err := secretsapprove.Parse(p.Action, p.Args, p.AllowedHosts, p.AllowedAgents)
			if err != nil {
				return ToolResult{}, err
			}
			sk := SessionKeyFromContext(ctx)
			if sk == "" {
				return ToolResult{}, errors.New("no session key in context")
			}
			key, err := session.ParseSessionKey(sk)
			if err != nil {
				return ToolResult{}, fmt.Errorf("parse session key: %w", err)
			}
			id, err := svc.Submit(key.AgentID, req, func(res secretsapprove.Result) {
				notifier.InjectToAgent(sk, SecretsResultMessage(res), "", "async_notify")
			})
			if err != nil {
				return ToolResult{}, fmt.Errorf("secrets %s: %w. Nothing was asked or changed", req, err)
			}
			msg := fmt.Sprintf("Request %s (%s) is waiting for the user in the foci app. The outcome arrives as a later message.", id, req)
			if req.NeedsValue() {
				msg += " The user types the value in the dialog; you will not see it."
			}
			return TextResult(msg), nil
		},
	}
}

// SecretsResultMessage is the message the asking session receives when its
// request ends. It never holds a value.
func SecretsResultMessage(res secretsapprove.Result) string {
	return fmt.Sprintf("[secrets] Request %s (%s): %s. %s", res.ID, res.Request, res.Outcome, res.Message)
}
