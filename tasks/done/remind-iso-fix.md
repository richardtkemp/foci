# Fix: memory_remind ISO timestamp parsing

## Bug
`resolveWhen()` in `memory/remind.go` doesn't parse ISO 8601 / RFC3339 timestamps. When an agent passes `when: "2026-02-26T12:05:00Z"`, it falls through to the default case and returns `now`, causing the reminder to fire immediately.

## Fix
In `resolveWhen()`, add RFC3339 parsing before the date and duration attempts:

```go
// Try parsing as an ISO 8601 / RFC3339 timestamp
if t, err := time.Parse(time.RFC3339, when); err == nil {
    return t
}
```

Add this before the existing date parse (`time.Parse("2006-01-02", when)`).

## Also update the tool description
In `tools/remind.go`, the `when` parameter description says:
> "When to surface: 'next_heartbeat', 'next_session', 'tomorrow', a date (YYYY-MM-DD), or a duration (e.g. '2h', '30m')"

Add ISO timestamp to the list:
> "When to surface: 'next_heartbeat', 'next_session', 'tomorrow', a date (YYYY-MM-DD), an ISO timestamp (e.g. '2026-02-26T12:00:00Z'), or a duration (e.g. '2h', '30m')"

## Tests
Add a test case for RFC3339 timestamps in `memory/remind_test.go`.

## Verification
```
go build -o /dev/null .
go test ./memory/ -v -count=1
go vet ./...
```

Commit and push when done.
