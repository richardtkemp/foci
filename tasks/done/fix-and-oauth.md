# Task: Fix thinking support + integrate OAuth auto-refresh

Two separate commits needed. Do NOT rename the project — the Go module stays as `foci`.

## Commit 1: Fix thinking/effort support in API types

File: `anthropic/types.go`

1. In `ContentBlock` struct, add two fields after the `Thinking` field:
   - `Signature string \`json:"signature,omitempty"\`` — thinking: encrypted verification signature (must be preserved)
   - `Data string \`json:"data,omitempty"\`` — redacted_thinking: encrypted thinking data

2. In `MessageRequest` struct, change the `Output` field's JSON tag from `json:"output,omitempty"` to `json:"output_config,omitempty"` — the Anthropic API rejects `"output"` as an unknown field.

3. Update the ContentBlock comment to mention `redacted_thinking` block type.

Commit message: `fix: correct output_config JSON tag and add thinking block signature/data fields`

## Commit 2: Integrate OAuth auto-refresh

The OAuth manager files already exist:
- `anthropic/oauth.go` — complete, ready to use
- `anthropic/oauth_test.go` — complete, ready to use

Read the task spec at `tasks/oauth-refresh.md` for full details.

What's needed: integrate the existing OAuth manager into the rest of the codebase:

### anthropic/client.go
- Add `tokenFunc func() string` and `refreshFunc func(staleToken string) error` fields to `Client`
- Add `getToken() string` helper (returns `tokenFunc()` if set, else `apiKey`)
- Add `NewClientWithTokenFunc(tokenFunc, timeout) *Client`
- Add `SetRefreshFunc(fn)` setter
- Add `IsAuthError() bool` on `APIError` (checks for 401)
- Change `sendOnce()` and `CountTokens()` to use `c.getToken()` instead of `c.apiKey`
- In `SendMessage()`: record token before retry loop; on 401 + refreshFunc → call refreshFunc(staleToken), retry once

### config/config.go
- Add `AutoRefresh *bool` to `AnthropicConfig` (toml:"auto_refresh")

### config/display.go
- Add `auto_refresh` row to anthropic display section

### main.go
- When `credentials_file` exists and `auto_refresh` is nil or true: create `OAuthManager`, call `Start()`, defer `Stop()`
- Create `Client` via `NewClientWithTokenFunc(mgr.Token, timeout)` + `SetRefreshFunc(mgr.RefreshIfNeeded)`
- Create `UsageClient` via `NewUsageClientWithFunc(mgr.Token)`
- Fall back to static token if OAuthManager init fails

### docs/CONFIG.md
- Add `credentials_file` and `auto_refresh` rows to `[anthropic]` table

### docs/WIRING.md
- Update startup flow with OAuthManager
- Update shutdown flow to mention `OAuthManager.Stop`

Commit message: `feat(anthropic): OAuth token auto-refresh with proactive and reactive renewal`

## Verification
```
go build -o /dev/null .
go vet ./...
go test ./... 
```

Push both commits when done.
