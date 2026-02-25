# Task: Cost Command Variations

## Current behaviour
`/cost` shows API cost summary for the current calendar day (UTC).

## New behaviour
Add subcommands for different time windows:

1. **`/cost`** (default, unchanged) — today's costs (calendar day UTC)
2. **`/cost 24h`** — last 24 hours rolling window
3. **`/cost week`** — mean cost per day over the last 7 days, plus the 7-day total

## Implementation

1. Find where `/cost` is handled (likely `command/builtins.go`)
2. The cost data comes from the API log file (`api.jsonl`) — find how it's currently aggregated
3. Add argument parsing: empty → today, "24h" → last 24h, "week" → 7-day stats
4. For "week" view, show:
   - Total over 7 days
   - Mean per day
   - Per-day breakdown (one line per day with date and cost)

## Format examples

`/cost 24h`:
```
API cost (last 24h): $4.32 eq.
  Cache reads:  $1.20
  Cache writes: $0.85
  Input:        $0.47
  Output:       $1.80
```

`/cost week`:
```
API cost (7-day summary):
  Total:    $28.45 eq.
  Mean/day: $4.06

  2026-02-25  $4.32
  2026-02-24  $6.10
  2026-02-23  $3.85
  2026-02-22  $2.90
  2026-02-21  $4.50
  2026-02-20  $3.28
  2026-02-19  $3.50
```

## Notes
- All costs are list-price equivalent (not real spend — flat monthly subscription)
- Keep the existing default behaviour identical
- Update SPEC.md

## Verification
- `/cost` works as before
- `/cost 24h` shows rolling 24h window
- `/cost week` shows 7-day breakdown with total and mean
- `go build && go test ./... && go vet ./...`
