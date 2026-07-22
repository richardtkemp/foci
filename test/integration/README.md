# L2 Integration Tests

End-to-end tests for foci that spawn a real `foci-gw` subprocess against a stubbed Telegram Bot API and a stubbed `claude` binary (cc-stub). They prove the **wiring across packages** works: ingress paths, agent dispatch, exec bridge tool routing, cross-agent send_to_session, subprocess lifecycle — without burning mana on real model inference or risking flakiness from real Telegram.

## Layer model

| Layer | foci-gw | Telegram | CC | What it catches | When to run |
|-------|---------|----------|-----|-----------------|-------------|
| L1 unit | in-process | n/a | n/a | logic inside one package | every commit (`make test`) |
| **L2 component** | **real subprocess** | stub (httptest) | stub binary | **cross-package wiring** | every PR (`make integration`) |
| L3 e2e | real subprocess | real bot | real claude | edge-protocol quirks, CC contract drift | nightly (planned, not yet built) |

The bug that prompted this layer — fixed in `d87875c1` (cross-agent `send_to_session` routing) — was structurally invisible to L1. L2 catches that class.

## Running

```sh
make integration
```

That target runs every test under `./test/integration/...` (build-tagged `//go:build integration`) plus the testharness's own smoke test. About 30 seconds on a warm cache.

To run a single test:

```sh
go test -tags=integration -count=1 -timeout 60s -run TestL2_CrossAgent ./test/integration/...
```

`-tags=integration` is required — without it, the test files are excluded from the build and `go test ./...` reports "no test files".

## Architecture

Each test:

1. **`testharness.StartGateway`** builds `bin/foci-gw` and `bin/cc-stub` from source (Go's build cache makes this cheap), synthesises a minimal `foci.toml` + `secrets.toml` in `t.TempDir()`, registers per-agent bot tokens with an in-process `TelegramStub`, spawns `foci-gw`, and waits for the deterministic `started N agent(s):` log line.
2. **Push synthetic Telegram updates** via `h.TelegramStub().PushUpdate(token, gotgbot.Update{...})`. The bot's long-poll drains them within ~1 second.
3. **Optional: script cc-stub** for an agent to emit specific assistant content. `h.WriteCCStubScript(t, agentID, body)` drops a JSON file that cc-stub reads on its next user message. Scripts can include Bash `tool_use` blocks — cc-stub literally runs `bash -c` so the exec bridge gets hit and foci-exposed tools (e.g. `foci_send_to_session`) dispatch through the normal RPC path.
4. **Assert on side effects**:
   - **cc-stub recorder** (`h.RecorderPath()`): JSONL with `kind=invocation` (one per process spawn) and `kind=user_message` (one per turn). Tests group by `workdir` and `text_prefix`.
   - **Telegram stub call log** (`h.TelegramStub().PeekSent(token)`): every outbound API call recorded with method + body.
   - **foci-gw stderr** (`h.Stderr()`): full log output, useful for failure diagnostics.

## Adding a new test

1. Create `test/integration/<name>_test.go` with `//go:build integration` as the first line.
2. Import `"foci/internal/testharness"` and `"github.com/PaulSonOfLars/gotgbot/v2"`.
3. Start a gateway, push updates, assert on side effects. Use the shared helpers in `helpers_test.go` (`readRecorderEntries`, `waitForUserMessage`, etc.) for common patterns.
4. Pick a polling deadline that's generous (~15-20s) — foci-gw startup is ~3s, but first-run onboarding and nudge extraction add another few seconds per agent. cc-stub's RunOnce path means even those use the stub, so they don't hit real claude, but they're still work.

If your test needs a behaviour cc-stub doesn't yet have (e.g. multi-turn scripted tool_use, deliberate failure injection, partial-message streaming) extend cc-stub itself — that's the right place. Test-only behaviour goes behind env vars; the existing surface is documented in `cmd/cc-stub/main.go`.

If your test needs a custom event log path (e.g. to seed fixture lines), use `scopedLoggingTOML` (`helpers_test.go`) for the `ExtraConfigTOML` — never hand-write a partial `[logging]\nevent_file = ...\n` block. A partial override leaves `api_file`/`payload_file`/`archive_dir` at their package defaults, which resolve against the real host `$HOME` and can alias production log files (foci_todo #1492/#1479's live incident). `testharness.verifyGeneratedLogPaths` fails the test loudly if this ever happens anyway, but `scopedLoggingTOML` gets it right the first time.

## What's tested today

| Test | What it asserts |
|------|-----------------|
| `TestL2_Ingress_*` | Telegram message → agent's cc-stub invoked in agent's workspace |
| `TestL2_Egress_*` | Agent reply → Telegram stub recorded a `sendMessage` with the echo body |
| `TestL2_CrossAgent_*` | `send_to_session` from fotini to clutch lands in clutch's workdir (regression net for `d87875c1`) |
| `TestL2_Tools_HTTPRequest_*` | `foci_http_request` via exec bridge reaches a side HTTP server |
| `TestL2_Lifecycle_RestartAfterStubExit` | Two sequential messages both process |

## Limitations

- **Stub fidelity**: cc-stub speaks the minimum-viable stream-json protocol. It doesn't model partial-message streaming, MCP elicitations, compaction events, or hook output. Tests that depend on those need to either extend cc-stub or wait for L3 (real claude).
- **Telegram stub fidelity**: 14 endpoints are implemented at a minimum-viable level. Media uploads are accepted but multipart bodies aren't parsed for assertion. File downloads are a stub redirect.
- **Permission model**: cc-stub skips the CC-level `can_use_tool` round trip entirely — it runs Bash tool_uses directly out-of-band. Tests that need real permission gating semantics would need L3 with real claude.
- **No model behaviour**: cc-stub's responses are canned. Anything that relies on real model output (cache fidelity, thinking blocks, real tool use decision-making) is L3 territory.

L3 (real Telegram + real CC, planned for nightly runs) will fill these gaps. See `/home/foci/clutch/docs/integration-testing-design.md` for the full design.
