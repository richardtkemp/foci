# Foci

A minimal agent platform in Go. One binary, ~32MB RAM, no framework.

## What It Is

Foci runs AI agents on Telegram. Each agent has its own identity (character files), memory (daily logs + curated long-term), and tools. Agents wake fresh each session — character documents are how they become themselves again.

Character files are fully configurable. Use the defaults (SOUL, CRAFT, COHERENCE, USER, MEMORY), follow OpenClaw's convention (AGENTS, TOOLS, SOUL, MEMORY, KEEPALIVE), or define whatever combination suits your agent. They're just markdown files in a directory — foci loads whatever you point it at.

Built as a ground-up rewrite of [OpenClaw](https://github.com/claw-project/openclaw) (TypeScript/Node.js). Same concept, different philosophy. Opinionated but still highly configurable.

## Why Rewrite

OpenClaw worked but fought its own weight:

| | OpenClaw | Foci |
|---|---|---|
| Runtime | Node.js + TypeScript | Go, single binary |
| Memory | ~500MB+ idle | ~32MB |
| Dependencies | ~1,200 packages (5.4GB) | 33 modules (~1.4GB) |
| Startup | Seconds (transpile + boot) | Instant |
| Speed | JS runtime overhead | Native Go (benchmarks TBD) |
| Config | YAML + env + scattered files | One TOML file + secrets.toml |
| Cache strategy | Bolted on | Architectural from day zero |

The rewrite wasn't about performance. It was about **owning every line** — understanding what the system does, why, and being able to change it without fighting abstractions. And about **bulletproof secret management** — OS-level isolation, domain-locked credentials, redaction at every layer — designed in from the start rather than patched on.

## Design Decisions

**Cache-first architecture.** Foci is designed around Anthropic's prompt cache — character files form a stable prefix, session branching shares cached prefixes, and the system actively avoids cache invalidation. The result is dramatically better token efficiency — more actual work per dollar. See [docs/CACHING.md](docs/CACHING.md) for the full cache architecture.

**Character documents, not system prompts.** Agents have SOUL.md (identity), CRAFT.md (practices), COHERENCE.md (how these relate), and MEMORY.md (learned experience). These aren't configuration — they're the agent's self-understanding, maintained by the agent itself. Foci ships default character files that produce and maintain a more coherent agent identity than OpenClaw's approach — resulting in better instruction-following and agent wellbeing.

**Compaction that doesn't lobotomise your agent.** When conversation context fills up, foci compresses history with a configurable prompt that preserves goals, reasoning, corrections, emotional tone, and technical state — not just a generic "summarise this." The default prompt produces agents that continue seamlessly after compaction instead of forgetting what they were doing. Prompt lives on disk as a markdown file, easy to tune per-agent.

**Memory that works out of the box.** Daily markdown files + a curated MEMORY.md — no vector database, no embeddings. What makes it work: foci ships with sensible defaults for memory formation (cron-driven capture), daily review (pruning and promotion), and weekly character review (identity evolution). New agents get these immediately. The agent reads, writes, and prunes its own memory. Compaction summaries preserve context when conversation history grows too large.

**Message metadata injection.** Every inbound message gets a `[meta]` header with current time, gap since last message, model, previous turn cost, token breakdown, and mana remaining. The agent always knows what time it is and how much budget is left — without touching the system prompt, so the cache stays intact.

**Tool result guard.** Large tool outputs are truncated *before* they enter context, with the full result saved to disk. This preserves the prompt cache (truncation happens at the tool layer, not post-hoc) and prevents a single oversized `cat` from blowing the context window. OpenClaw does this after the fact, which means the cache is already busted by the time you notice.

**Message transforms.** Configurable regex find/replace rules applied to inbound messages before command dispatch and the agent loop. Use case: prepending "Questions are just requests for information.\n-------\n" to any message ending with `?` — training the agent to answer questions without acting on them. Transforms can also produce commands (e.g. `m` → `/mana`). Rules are per-agent and invisible to the user.

**Prompt repetition.** Based on [research showing that repeating the input prompt improves LLM accuracy](https://arxiv.org/abs/2512.14982) without increasing output tokens or latency, foci can automatically duplicate user messages in API calls. Configurable per-agent (`duplicate_messages`), skipped for system triggers like cron wakes.

**Multiball.** `/multiball` forks your session to a second Telegram bot — same agent, same context, parallel thread. Both sessions share the cached prefix, so the fork is cheap. See [docs/MULTIBALL.md](docs/MULTIBALL.md) for bot pool config, session lifecycle, and routing details.

**Built-in todo list.** Persistent, priority-ranked task management the agent can use to keep its own priorities straight. Add, complete, search, remove — stored in SQLite, survives restarts. The agent tracks its own work without external tools or memory file hacks.

**Cron is cron.** Scheduled tasks use the system crontab, not a built-in scheduler. Keepalives, memory formation, daily reviews — they're all cron entries calling `foci send` or `foci branch`. Debug with `crontab -l`, edit with `crontab -e`, monitor with your existing tools. No reinvented wheels, no custom DSL, no "task engine" to learn. See [docs/HEARTBEAT.md](docs/HEARTBEAT.md) for the keepalive, background work, and manamometer details.

**Multi-agent, single process.** Multiple agents share one binary with separate workspaces, identities, and Telegram bots. No container overhead, no orchestration. Config is one TOML file.

**First-class coding agent support.** Tmux management and coding agent orchestration are built-in tools, not CLI-based skills the agent has to parse. The agent can start sessions, send instructions, watch for inactivity, and read output through structured tool calls — simpler to use, therefore more reliable.

**OS-level secret isolation.** Secrets live in a file readable only by a dedicated group. Child processes have that group dropped — they can use secret templates but never read the values directly. Secrets are domain-locked, output is redacted, and Bitwarden vault integration adds approval-gated dynamic secrets. See [docs/SECRETS.md](docs/SECRETS.md) for the full security model.

## Architecture

```
foci.toml + secrets.toml
    │
    ├── Agent (identity, tools, memory)
    │   ├── Character files (SOUL, CRAFT, COHERENCE, USER, MEMORY)
    │   ├── Session management (branching, compaction)
    │   └── Tool registry (exec, http_request, tmux, spawn, bitwarden, ...)
    │
    ├── Telegram bot (per-agent)
    │   ├── Message routing
    │   ├── Tool call previews
    │   └── Voice mode (TTS)
    │
    ├── Anthropic API client
    │   ├── Streaming responses
    │   ├── Prompt caching (prefix-matched)
    │   └── Token tracking / mana monitoring
    │
    └── Background services
        ├── Cron (memory formation, keepalive, reviews)
        ├── Tmux memory monitor
        └── Bitwarden vault refresh
```

## Requirements

| What | Why | Notes |
|------|-----|-------|
| **Go 1.22+** | Build from source | |
| **Claude Code** | Provides the OAuth token foci uses to access the Anthropic API | Also enables `/usage` (rate limit detection) and coding agent orchestration. See [docs/AUTH.md](docs/AUTH.md) for token resolution and `foci auth` setup. |
| **Telegram bot token** | Message transport | Create via [@BotFather](https://t.me/BotFather) |

### Optional

| What | Enables | Notes |
|------|---------|-------|
| **bash** | `set -o pipefail` in exec, tool-piping shell functions | Falls back to `sh` if absent (pipefail and shell functions unavailable) |
| **Groq API key** | Voice input (speech-to-text) | Free tier, fast Whisper transcription |
| **Brave Search API key** | `web_search` tool | Free tier available |
| **edge-tts + ffmpeg** | Voice output (text-to-speech) | `pip install edge-tts`, `apt install ffmpeg` |
| **Bitwarden CLI** | Dynamic secret management | Approval-gated via Telegram |

See [docs/DEPENDENCIES.md](docs/DEPENDENCIES.md) for a full list of suggested system tools.

## Quick Start

```bash
git clone https://github.com/richardtkemp/foci.git && cd foci && sudo ./setup.sh -u foci
```

<img src="assets/qr-repo.png" width="128" alt="QR code linking to the foci repository" />

See [docs/INSTALL.md](docs/INSTALL.md) for prerequisites, options, and next steps.

## Documentation

- [docs/SPEC.md](docs/SPEC.md) — Full specification (source of truth)
- [docs/INSTALL.md](docs/INSTALL.md) — End-to-end installation guide
- [docs/CONFIG.md](docs/CONFIG.md) — Configuration reference
- [docs/AUTH.md](docs/AUTH.md) — Authentication and OAuth setup
- [docs/SECRETS.md](docs/SECRETS.md) — Secret management and Bitwarden integration
- [docs/CACHING.md](docs/CACHING.md) — Cache architecture and preservation
- [docs/MULTIBALL.md](docs/MULTIBALL.md) — Parallel conversations (session forking)
- [docs/WIRING.md](docs/WIRING.md) — Internal architecture and wiring
- [docs/CLI.md](docs/CLI.md) — CLI reference
- [docs/TOOLS.md](docs/TOOLS.md) — Tool reference and shell function piping
- [docs/COMMANDS.md](docs/COMMANDS.md) — Slash commands reference
- [docs/DEPENDENCIES.md](docs/DEPENDENCIES.md) — System dependencies

## Stats

- ~35k lines of Go
- 285+ commits
- 33 dependencies
- 20 packages
- Built and deployed daily on a single NUC

## License

[PolyForm Noncommercial 1.0.0](LICENSE) — free for personal, academic, and non-commercial use. Commercial use requires a separate license. Contact [Richard Kemp](https://github.com/richardtkemp) for details.
