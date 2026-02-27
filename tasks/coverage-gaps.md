# Task: Improve test coverage on key packages

## Coverage Summary
```
compaction    94.1%   ✅
secrets       95.2%   ✅
skills        94.0%   ✅
workspace     93.4%   ✅
command       86.8%   ✅
anthropic     86.6%   ✅
agent         84.3%   ✅
state         82.1%   ok
config        80.2%   ok
session       80.5%   ok
tools         77.7%   ok
log           73.6%   ⚠️
memory        68.0%   ⚠️
telegram      66.4%   ⚠️
table         65.9%   ⚠️
voice         64.8%   ⚠️
cmd/foci      42.8%   ⚠️
main.go       3.7%    (expected — wiring)
```

## Priority: Focus on packages below 70% that are testable

### 1. table/ (65.9%) — should be easy to get to 90%+
New package, small surface area. Likely missing edge cases in Format():
- Empty columns, single column, mismatched row lengths
- Right-alignment with unicode
- Zero rows (headers only already tested?)

### 2. voice/ (64.8%)
Check what's untested — likely the TTS integration functions that shell out to external tools.

### 3. memory/ (68.0%)
Uncovered functions:
- `Watch()` — fsnotify watcher (hard to test, skip)
- `handleFileEvents()` — event handler (hard to test, skip)  
- `scheduleReindex()` — debounce (hard to test, skip)
- `scratchpad.List()` — should be easy to test

Focus on `scratchpad.List()` and any other pure-logic functions.

### 4. telegram/ (66.4%)
Most uncovered functions are integration points (NewBot, Run, pollUpdates, agentWorker, processAgentMessage, all Send* methods). These require a running Telegram bot — hard to unit test.

Testable gaps: markdown.go functions, message splitting, any pure logic.

### 5. log/ (73.6%)
Uncovered: FilePaths, payload functions, Fatalf, GetLevel. Some are trivial getters, some involve file I/O.

## Instructions
- Focus on packages where coverage can meaningfully improve with unit tests (table, memory/scratchpad, log getters)
- Skip integration-heavy code that would need mocks of external services (telegram bot, voice TTS, fsnotify)
- Don't test trivial setters/getters unless they have logic
- Run `go test -coverprofile=coverage.out ./...` after to verify improvement
- Commit and push when done
