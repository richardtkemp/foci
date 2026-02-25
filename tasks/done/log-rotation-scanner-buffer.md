# Fix: Log rotation scanner buffer too small

## Problem
Log rotation fails on `api-payload.jsonl` with:
```
scan: bufio.Scanner: token too long
```

The default `bufio.Scanner` buffer is 64KB, but api-payload.jsonl lines contain full API request+response JSON blobs (including system prompts) — easily 100KB+ per line.

## Fix
In the log rotation code, set a custom scanner buffer. Something like:

```go
scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB max line
```

This should be applied wherever `bufio.Scanner` is used for reading log lines during rotation.

## Test
After fix, verify rotation completes without the "token too long" warning. The api-payload.jsonl is currently ~3.1GB.

## Docs
No doc changes needed — this is a bugfix.
