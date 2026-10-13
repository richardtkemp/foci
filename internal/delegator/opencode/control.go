// control.go — delegator.ControlSender + Interrupt implementation.
// Translates foci's backend-agnostic control intents into opencode's
// HTTP API.

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"foci/internal/delegator"
	"foci/internal/log"
)

// SendControl dispatches a ControlRequest to opencode's HTTP API.
// Implements delegator.ControlSender.
//
//	SetModelRequest          → store b.model (applied per-prompt via body "model" field)
//	SetPermissionModeRequest → PATCH /session/:id {permission: [rules]} — this Backend's own session only
//	ApplyFlagSettingsRequest → no-op (effort has no opencode equivalent)
//	other                    → error
func (b *Backend) SendControl(ctx context.Context, req delegator.ControlRequest) error {
	switch r := req.(type) {
	case *delegator.SetModelRequest:
		binaryPath, _ := b.cfg["binary"].(string)
		resolved, err := b.resolveModelFn(ctx, binaryPath, b.workDir, r.Model)
		if err != nil {
			return fmt.Errorf("opencode: %w", err)
		}
		b.mu.Lock()
		b.model = resolved
		b.mu.Unlock()
		return nil

	case *delegator.SetPermissionModeRequest:
		return b.applySessionPermission(ctx, permissionRuleset(mapPermissionMode(r.Mode)))

	case *delegator.ApplyFlagSettingsRequest:
		// opencode has no apply_flag_settings equivalent. Effort is
		// CC-only. Fire-and-forget — return nil.
		log.NewComponentLogger(b.logComponent()).Debugf("SendControl: ApplyFlagSettings is a no-op for opencode")
		return nil

	default:
		return fmt.Errorf("opencode: unsupported control request %T", req)
	}
}

// Interrupt aborts any in-flight turn via POST /session/:id/abort.
// Implements delegator.Delegator.Interrupt.
func (b *Backend) Interrupt(ctx context.Context) error {
	if b.server == nil || b.sessionID == "" {
		return errors.New("opencode: Interrupt before Start")
	}
	url := fmt.Sprintf("%s/session/%s/abort", b.server.baseURL, b.sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("POST /abort: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST /abort: HTTP %d", resp.StatusCode)
	}
	return nil
}

// applySessionPermission PATCHes the rules onto THIS Backend's opencode
// session (PATCH /session/:id, body {permission: rules}) — the narrowest
// scope opencode offers (prompt_async has no permission field) and the
// only one it actually enforces.
//
// NEVER send permissions through PATCH /config again (#2330; live-checked
// against opencode 1.17.15 on 2026-10-13):
//   - the response echoes the value but it is never enforced — GET /config
//     does not show it, and a session created afterwards ran bash with no
//     prompt, so /mode and default_permission changed nothing;
//   - every PATCH /config disposes and recreates the server's instance,
//     aborting the in-flight turn of EVERY session on the shared server
//     (MessageAbortedError, "User aborted the command");
//   - the body is written to config.json in the server's working
//     directory — the agent's workDir (/home/foci/arnix/config.json got
//     committed to that repo this way).
//
// opencode's ruleset API is append-only: the PATCHed rules are
// concatenated onto the session's accumulated rules and resolved with
// findLast (last matching rule wins, default "ask"), so callers re-send
// a FULL ruleset per apply and the new batch shadows the old — including
// rules a resumed session still carries from before.
func (b *Backend) applySessionPermission(ctx context.Context, rules []permissionRule) error {
	if b.server == nil || b.server.baseURL == "" {
		return errors.New("opencode: no server (Start not called)")
	}
	if b.sessionID == "" {
		return errors.New("opencode: no session (Start not called)")
	}
	payload, err := json.Marshal(sessionPermissionPatch{Permission: rules})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/session/%s", b.server.baseURL, b.sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("PATCH /session/:id: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PATCH /session/:id: HTTP %d", resp.StatusCode)
	}
	return nil
}

// permissionRuleset converts mapPermissionMode's category→action map
// into the rule-array form the session API takes: one
// {permission, pattern, action} entry per category, pattern "*"
// (the mapping is category-level, never command-level).
//
// Order is load-bearing. opencode resolves a session's accumulated
// rules with findLast (last match wins), so the "*" catch-all is
// emitted FIRST and the specific categories after it, sorted (Go map
// iteration is random) — only then do the specific rules override their
// own catch-all. The same findLast is what makes "latest mode wins"
// work across applies: every batch must carry a {*,*} rule, which then
// outranks every rule of every earlier batch. mapPermissionMode's every
// branch emits "*" — keep it that way, or modes applied earlier would
// resurface.
func permissionRuleset(actions map[string]any) []permissionRule {
	rules := make([]permissionRule, 0, len(actions))
	specific := make([]string, 0, len(actions))
	for category, action := range actions {
		if category == "*" {
			rules = append(rules, permissionRule{Permission: "*", Pattern: "*", Action: fmt.Sprint(action)})
			continue
		}
		specific = append(specific, category)
	}
	sort.Strings(specific)
	for _, category := range specific {
		rules = append(rules, permissionRule{
			Permission: category,
			Pattern:    "*",
			Action:     fmt.Sprint(actions[category]),
		})
	}
	return rules
}

// mapPermissionMode translates foci's permission modes to opencode's
// per-category ask|allow|deny rules. opencode's permission config uses
// wildcards + per-tool overrides.
//
// For v1 we apply a coarse mapping: "bypassPermissions" → allow all;
// "acceptEdits" → allow edits + ask for bash; "plan" → deny edits;
// default → ask for everything.
func mapPermissionMode(mode string) map[string]any {
	switch mode {
	case "bypassPermissions", "auto":
		return map[string]any{"*": "allow"}
	case "acceptEdits":
		return map[string]any{
			"edit": "allow",
			"bash": "ask",
			"*":    "ask",
		}
	case "plan":
		return map[string]any{
			"edit": "deny",
			"read": "allow",
			"*":    "ask",
		}
	default:
		return map[string]any{
			"edit": "ask",
			"bash": "ask",
			"*":    "ask",
		}
	}
}
