# Task: Per-Agent Secrets (#84)

## Problem
Secrets in `secrets.toml` are global — all agents see the same values. We need agents to have their own values for the same secret key (e.g. different `google.token` per agent for different Google accounts).

## Design: Nested TOML sections

```toml
# Global secrets (available to all agents)
telegram.clutch = "token1"
telegram.scout = "token2"
brave.api_key = "shared-key"

# Per-agent overrides
[agents.fotini]
google.token = "fotini-google-token"
google.calendar_id = "eleni-calendar"

[agents.clutch]
google.token = "clutch-google-token"
```

## Resolution order
1. Agent-scoped secret (`agents.<agent_id>.<key>`) — highest priority
2. Global secret (`<key>`) — fallback

## Implementation

1. **Config loading** (`config/config.go` or secrets loading):
   - Parse `secrets.toml` with awareness of `[agents.*]` sections
   - Store two maps: global secrets `map[string]string` and per-agent secrets `map[string]map[string]string`

2. **Secret resolution**:
   - Add a method like `ResolveSecret(agentID, key string) (string, bool)` that checks agent-scoped first, then global
   - Current `GetSecret(key)` should still work for backward compat (returns global)

3. **Wiring**:
   - When tools use `{{secret:NAME}}` templates, the resolution should be agent-aware
   - The http_request tool (and any other secret-consuming code) needs to know which agent is calling
   - Find where secrets are resolved in tool execution and pass the agent ID through

4. **Secret listing**:
   - The secrets list shown in system prompt should reflect what that agent can see (its own overrides + globals)
   - Don't show other agents' secrets

## Edge cases
- Agent section exists but key not in it → fall back to global
- Agent section doesn't exist → all global
- Key exists in both → agent wins
- `{{secret:NAME}}` in http_request should resolve per the calling agent

## Security
- Per-agent secrets should NOT be readable by other agents via any tool
- The secrets list in the system prompt must be scoped

## Verification
- Agent with override gets its own value
- Agent without override gets global value
- `{{secret:NAME}}` in http_request resolves per-agent
- Secrets list in system prompt is scoped
- Backward compatible — existing secrets.toml with no `[agents.*]` sections works unchanged
- `go build && go test ./... && go vet ./...`

## Docs
- Update SPEC.md
- Update docs/CONFIG.md (or create docs/SECRETS.md if it doesn't exist)
- Document the resolution order and TOML format
