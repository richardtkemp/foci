# Task: Improve /config Command

## Two parts — do both in one commit

### Part 1: Show full running config (#50)

The `/config` slash command currently shows an incomplete view. Fix it:

1. **Default output:** Formatted, readable view of the FULL running config for the calling agent. Show:
   - The agent's own `[[agents]]` block (all fields)
   - Global config that applies: `[telegram]`, `[sessions]`, `[memory]`, `[logging]`, `[http]`, `[environment]`, `[skills]`, `[usage_warnings]`, `prompt_rules`
   - For fields with per-agent override: show the resolved value (per-agent wins over global)
   - Redact secrets (token values → `***`)

2. **`/config toml` subcommand:** Raw TOML output of the same resolved config. Still redact secrets.

3. Each agent should see **its own** config, not every agent's. The command runs in the context of a session which belongs to an agent — use that agent's config.

### Part 2: /config available (#53)

Add `/config available` subcommand:

1. Lists config options that are NOT currently set for this agent, with their default values
2. Helps discoverability — "what can I configure that I haven't yet?"
3. Should cover both `[[agents]]` fields and `[telegram]` global fields
4. Format: table with option name, default value, brief description

## Implementation Notes

- Slash commands are handled in the agent or session layer — find where `/config` is currently implemented
- The config struct in `config/config.go` has all the fields with their toml tags — use reflection or a manual list
- For "available" options, you need to know what fields exist vs what's set. The `toml` metadata (`md.IsDefined`) pattern used elsewhere could help, or just check zero values
- Update docs: SPEC.md, any command reference

## Verification

- `/config` shows full agent config + relevant globals
- `/config toml` shows raw TOML
- `/config available` shows unset options with defaults
- Secrets are redacted in all views
- Different agents see different configs
- `go build && go test ./... && go vet ./...`
