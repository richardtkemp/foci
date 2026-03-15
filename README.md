# Foci

A minimal agent platform in Go. One binary, ~32MB RAM, no framework.

## What It Is

Foci runs AI agents on Telegram. Each agent has its own identity (character files), memory (daily logs + curated long-term), and tools. Character files are fully configurable — use the defaults (SOUL, CRAFT, COHERENCE, USER, MEMORY), follow OpenClaw's convention, or define whatever combination suits your agent. They're just markdown files in a directory. See [docs/DEFAULTS.md](docs/DEFAULTS.md) for all embedded prompts.

Built for Anthropic and battle-tested there, but any OpenAI-compatible endpoint works — the only Anthropic-specific feature is subscription allowance tracking (mana).

## Quick Start

```bash
git clone https://github.com/richardtkemp/foci.git && cd foci && ./setup.sh
```

See [docs/INSTALL.md](docs/INSTALL.md) for prerequisites, options, and next steps.

## Why Rewrite

Ground-up rewrite of [OpenClaw](https://github.com/claw-project/openclaw). Same concept, different philosophy. See [docs/COMPARISON.md](docs/COMPARISON.md) for a detailed feature comparison. OpenClaw worked but became hard to maintain and customise:

| | OpenClaw | Foci |
|---|---|---|
| Runtime | Node.js + TypeScript | Go, single binary |
| Memory | ~500MB+ idle | ~32MB |
| Dependencies | ~1,200 packages (5.4GB) | 15 direct modules |
| Startup | Seconds (transpile + boot) | Instant |
| Config | YAML + env + scattered files | One TOML file + secrets.toml |
| Cache strategy | Bolted on | Architectural from day zero |

The rewrite wasn't about performance. It was about **owning every line** — understanding what the system does, why, and being able to change it without fighting abstractions. And about **bulletproof secret management** — OS-level isolation, domain-locked credentials, redaction at every layer — designed in from the start rather than patched on.

## Design Decisions

**Cache-first architecture.**
Built around Anthropic's prompt cache. Character files form a stable prefix, session branching shares cached context, and the system actively avoids invalidation. More work per dollar. See [docs/CACHING.md](docs/CACHING.md).

**OS-level secret isolation.**
Secrets are readable only by a dedicated group. Child processes have that group dropped — they can use secret templates but never read values directly. Domain-locked, output-redacted, with optional Bitwarden vault integration. See [docs/SECRETS.md](docs/SECRETS.md).

**Tool result guard.**
Large tool outputs are truncated *before* entering context, with full results saved to disk. Cache stays intact, context window stays clean.

**Multiball.**
`/multiball` forks your session to a second Telegram bot — same agent, same context, parallel thread. Both share the cached prefix, so the fork is cheap. See [docs/MULTIBALL.md](docs/MULTIBALL.md).

**Multi-agent, single process.**
Multiple agents share one binary with separate workspaces, identities, and Telegram bots. No containers, no orchestration. One TOML file.

**First-class coding agent support.**
Tmux management and coding agent orchestration are structured tool calls, not CLI skills the agent has to parse. Start sessions, send instructions, watch for inactivity, read output.

**Compaction that doesn't lobotomise your agent.**
Context compression preserves goals, reasoning, corrections, emotional tone, and technical state — not just a generic summary. Configurable per-agent as a markdown file on disk.

**Memory that works out of the box.**
Daily markdown files + curated MEMORY.md. No vector database, no embeddings. Ships with defaults for memory formation, daily review, and weekly character evolution. The agent reads, writes, and prunes its own memory. See [docs/MEMORY.md](docs/MEMORY.md).

**Message metadata injection.**
Every inbound message gets a `[meta]` header — time, gap since last message, model, cost breakdown, mana remaining. The agent always knows what's going on without touching the system prompt.

## Requirements

| What | Why | Notes |
|------|-----|-------|
| **Go 1.24+** | Build from source | |
| **Telegram bot token** | Message transport | Create via [@BotFather](https://t.me/BotFather) |

### Optional

| What | Enables | Notes |
|------|---------|-------|
| **Claude Code** | Subscription usage tracking and coding agent orchestration | Everything else works without it. See [docs/AUTH.md](docs/AUTH.md) |
| **bash** | `set -o pipefail` in exec, tool-piping shell functions | Falls back to `sh` if absent (pipefail and shell functions unavailable) |
| **Groq API key** | Voice input (speech-to-text) | Free tier, fast Whisper transcription |
| **Brave Search API key** | `web_search` tool | Free tier available |
| **edge-tts + ffmpeg** | Voice output (text-to-speech) | `pip install edge-tts`, `apt install ffmpeg` |
| **Bitwarden CLI** | Dynamic secret management | Approval-gated via Telegram. See [docs/BITWARDEN.md](docs/BITWARDEN.md) |

See [docs/DEPENDENCIES.md](docs/DEPENDENCIES.md) for a full list of suggested system tools.

## Documentation

- [docs/SPEC.md](docs/SPEC.md) — Full specification (source of truth)
- [docs/COMPARISON.md](docs/COMPARISON.md) — Feature comparison with OpenClaw and Nanobot
- [docs/INSTALL.md](docs/INSTALL.md) — End-to-end installation guide
- [docs/CONFIG.md](docs/CONFIG.md) — Configuration reference
- [docs/DEFAULTS.md](docs/DEFAULTS.md) — Embedded prompt defaults
- [docs/AUTH.md](docs/AUTH.md) — Authentication and OAuth setup
- [docs/SECRETS.md](docs/SECRETS.md) — Secret management
- [docs/BITWARDEN.md](docs/BITWARDEN.md) — Bitwarden vault integration
- [docs/CACHING.md](docs/CACHING.md) — Cache architecture and preservation
- [docs/MEMORY.md](docs/MEMORY.md) — Memory system (search, formation, consolidation)
- [docs/MULTIBALL.md](docs/MULTIBALL.md) — Parallel conversations (session forking)
- [docs/SESSION_KEYS.md](docs/SESSION_KEYS.md) — Session key format and lifecycle
- [docs/NUDGE.md](docs/NUDGE.md) — Mid-turn behavioral reminders
- [docs/HEARTBEAT.md](docs/HEARTBEAT.md) — Keepalive and background work
- [docs/WEBHOOKS.md](docs/WEBHOOKS.md) — Webhook-triggered agent turns
- [docs/WIRING.md](docs/WIRING.md) — Internal architecture and wiring
- [docs/CLI.md](docs/CLI.md) — CLI reference
- [docs/TOOLS.md](docs/TOOLS.md) — Tool reference and shell function piping
- [docs/COMMANDS.md](docs/COMMANDS.md) — Slash commands reference
- [docs/DEPENDENCIES.md](docs/DEPENDENCIES.md) — System dependencies

## Stats

- ~52k lines of Go (~128k including tests)
- 1,200+ commits
- 15 direct dependencies
- 37 packages

## License

[PolyForm Noncommercial 1.0.0](LICENSE) — free for personal, academic, and non-commercial use. Commercial use requires a separate license. Contact [Richard Kemp](https://github.com/richardtkemp) for details.

<img src="assets/qr-repo.png" width="128" alt="QR code linking to the foci repository" />
