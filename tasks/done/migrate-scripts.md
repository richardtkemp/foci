# Task: Migrate 3 Scripts from OpenClaw to Foci

## Context
Three scripts in `/home/foci/scripts/` were written for the old "openclaw" platform and need migrating to "foci". The migration plan at `/home/foci/scripts/migration-plan.md` has full details, but here's what you need.

## Session File Format (Foci)

**Location:** `/home/foci/data/sessions/agent/<agent>/chat/<session_id>.jsonl`

**Format:** JSONL where each line is one of:
```json
{"type":"session_meta","created_at":"2026-02-24T17:52:19Z"}
{"role":"user","content":[{"type":"text","text":"[meta] time=2026-02-26T18:58:31Z gap=41s model=claude-opus-4-6 prev_cost=$0.2546 prev_tokens=in:1/out:2/cR:156032/cW:1088 mana=92%\nActual user message here"}]}
{"role":"assistant","content":[{"type":"text","text":"Response here"}]}
```

Key differences from openclaw:
- No `{"type":"message","message":{...},"timestamp":"..."}` wrapper
- Token usage is in the `[meta]` tag of USER messages, as `prev_tokens=in:X/out:Y/cR:Z/cW:W`
- `prev_tokens` describes the PREVIOUS assistant turn's usage (not this message)
- Timestamps come from `[meta] time=...` in user messages, or `session_meta.created_at`
- Agent name comes from the directory path: `.../agent/<agent>/chat/...`
- There's also a `spawn/` subdirectory alongside `chat/` — include those too

## API Payload Log (alternative data source)

There's also `/home/foci/logs/api-payload.jsonl` with per-API-call data:
```json
{"ts":"2026-02-26T18:59:16Z","session":"agent:clutch:chat:5970082313","model":"claude-opus-4-6","request":{...},"response":{"usage":{"input_tokens":1,"output_tokens":211,"cache_read_input_tokens":159769,"cache_creation_input_tokens":314}}}
```
This is ~100KB per line (contains full request/response), so **never load it entirely into memory**. Use streaming/line-by-line parsing or `jq` for extraction. This may be a better data source for token-budget-analyzer since it has exact usage per call.

## Script 1: token-usage-tracker.py

### Changes needed:
1. Update `AGENTS_DIR` to `Path("/home/foci/data/sessions/agent")`
2. Update `OUTPUT_DIR` to `Path("/home/foci/data/token-usage")` (create if needed)
3. Rewrite `load_data()` to:
   - Glob `*/chat/*.jsonl` and `*/spawn/*.jsonl`
   - Extract agent from path (directory name after `agent/`)
   - Parse `[meta]` tags from user messages for timestamps and token usage
   - Handle the `prev_tokens` attribution (it describes previous turn)
4. Update title strings from "OpenClaw" to "Foci"
5. Update description/docstring
6. Add `-h`/`--help` support (already has argparse, just update description)

### Helper function needed:
```python
def parse_meta(text):
    """Extract timestamp and token usage from [meta] tag in user message."""
    # Returns (datetime, {input, output, cache_read, cache_write}) or (None, None)
    # Pattern: [meta] time=... prev_tokens=in:X/out:Y/cR:Z/cW:W
```

## Script 2: token-budget-analyzer.py

### Changes needed:
1. Update `load_sessions()` paths to foci format
2. Rewrite `parse_session_file()` to parse foci JSONL format
3. Extract token usage from `[meta]` tags (same helper)
4. Update all strings/descriptions from "openclaw" to "foci"
5. Consider using api-payload.jsonl instead of session files (it has exact per-call usage). But session files work too since [meta] has the data.

## Script 3: run-morning-routine.sh

### Changes needed:
1. Remove `OPENCLAW_WORKSPACE` dependency entirely
2. Remove `send-to-telegram.sh` call — replace with `foci send`
3. Update the token-usage-tracker call path
4. Update output file paths
5. Replace direct Telegram send with:
   ```bash
   # Write report to file
   cat > /home/foci/data/morning-report.md << EOF
   $MESSAGE
   EOF
   
   # Copy chart
   cp "$OUTPUT_DIR/token-usage-clutch.png" /home/foci/data/morning-chart.png 2>/dev/null
   
   # Notify agent to forward to user
   foci -a clutch send "Morning routine complete. Report at /home/foci/data/morning-report.md and chart at /home/foci/data/morning-chart.png. Please read the report and forward the summary and chart to Dick."
   ```

## Testing

After migration, run each script and verify:
```bash
# Script 1
python3 /home/foci/scripts/token-usage-tracker.py --report --days 3
python3 /home/foci/scripts/token-usage-tracker.py --days 7

# Script 2
python3 /home/foci/scripts/token-budget-analyzer.py

# Script 3 (just check it parses, don't actually send)
bash -n /home/foci/scripts/run-morning-routine.sh
```

## Important
- Keep the scripts in `/home/foci/scripts/` (same location)
- Create `/home/foci/data/token-usage/` output directory
- Don't change the analysis logic — only the data loading/format parsing and output delivery
- Make sure scripts handle the case where `[meta]` tag or `prev_tokens` is missing (some messages won't have it)
- Push changes when done
