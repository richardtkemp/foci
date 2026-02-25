# Task: Compaction Debug — Send Summary to User (#91)

## Feature
Optional feature: when compaction completes, send the compaction summary to the user as a Telegram file attachment. Helps verify what survived the cut.

## Config
New config option `compaction_debug` — boolean, default false. Global in `[sessions]` AND per-agent override on `[[agents]]`.

```toml
[sessions]
compaction_debug = false  # global default

[[agents]]
id = "clutch"
compaction_debug = true  # override for this agent
```

Use the established `*bool` pattern on AgentConfig (nil = use global), bool on SessionsConfig with `md.IsDefined` defaulting to false.

## Implementation
1. Add `CompactionDebug` to both `SessionsConfig` (bool) and `AgentConfig` (*bool)
2. Wire the resolution in main.go (per-agent overrides global)
3. After compaction completes (wherever the summary is generated), if compaction_debug is true:
   - Write the summary to a temp file (markdown, e.g. `/tmp/compaction-summary-{session}-{timestamp}.md`)
   - Send it to the user via Telegram as a file attachment with a brief caption like "Compaction summary for {session}"
4. The summary content already exists — it's whatever gets injected as the compaction handoff. Just capture it before/after injection.

## Notes
- The bot/Telegram sending function needs to be accessible from wherever compaction runs
- This is a debug/diagnostic feature — default OFF
- Don't block on send failure — log a warning and continue

## Verification
- Default: no file sent on compaction
- With `compaction_debug = true`: file sent after compaction
- Per-agent override works
- Send failure doesn't break compaction
- Update docs/CONFIG.md (both tables), SPEC.md
- `go build && go test ./... && go vet ./...`
