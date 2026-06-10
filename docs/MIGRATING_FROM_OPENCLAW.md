# Migrating from OpenClaw

A practical guide for OpenClaw users moving to foci. Assumes you've used OpenClaw and understand its concepts.

## What's Different

Things that will surprise you coming from OpenClaw.

### Secrets are invisible

In OpenClaw, secrets are injected as env vars and can appear in the agent's context (with redaction in logs). In foci, secrets live in a separate `secrets.toml` the agent can never read. API credentials are resolved at the OS level via `{{secret:NAME}}` templates in HTTP requests, locked to specific hostnames. The model never sees the credential value — it can't accidentally include an API key in a shell command or leak it in output.

### Caching actually works

OpenClaw has prompt caching support but its architecture causes frequent cache busts — workspace file edits, session structure changes, and tool ordering all invalidate the cache. In foci, the system does not accidentally bust the cache.

### Compaction preserves more context

Foci's compaction creates a summary and preserves recent messages, but also carries forward the scratchpad and todo list. The agent wakes up after compaction with its working notes intact, not just a summary of what happened.

### Queue behaviour is simpler

Foci implements the two queue modes that matter: steer (messages arriving mid-turn get injected between tool calls as a user redirect) and FIFO queuing behind the turn lock. Toggle with `steer_mode` in config.

OpenClaw has seven modes (steer, followup, collect, steer-backlog, steer+backlog, queue, interrupt) with per-mode debounce, cap, and drop policy. The extra modes — collect, interrupt, steer-backlog variants — are niche and add configuration complexity without proportional value. If you were using one of them, you'll find steer + FIFO covers the same ground in practice.

### Session branching is a first-class feature

OpenClaw doesn't have session branching. Foci has three branch types:
- **Facet** (`/fork`) — parallel conversation on a separate Telegram bot, sharing the parent's cache prefix.
- **Spawn** (`spawn` tool) — sub-call with four context modes for different autonomy levels.
- **Clone** (`spawn` with `clone` context) — full async branch with tools and the parent's conversation context.

### Mid-turn behavioral nudges

Foci extracts behavioral rules from character files and injects them as mid-turn nudges between tool calls — targeted reminders to keep important rules fresh. OpenClaw doesn't have this.

## Why Migrate

### What you gain

- **Secret isolation and domain-locked HTTP.** Credentials never appear in the agent's context — OS-level process boundaries, not env vars with logging redaction. API calls carry credentials via `{{secret:NAME}}` templates, locked to allowed hostnames per-secret. The model sees `{{secret:NAME}}` placeholders, not values; an OS group boundary keeps spawned subprocesses from reading the secrets file, in-process file tools enforce a path blocklist, and responses are redacted. (No design fully prevents a prompt-injected model from *using* a credential it's permitted to send — the goal is to stop it reading or exfiltrating the raw secret. See `docs/SECRETS.md`.)
- **Cache-first architecture.** Session structure, branching, tool injection, and character file ordering are all designed to maximize cache hits. OpenClaw's architecture causes frequent cache busts — you'll see the difference in your API bill immediately.
- **Operational simplicity.** One ~50 MB static binary, ~35 MB idle RAM. No Node.js runtime, no 560-package dependency tree. You `scp` it to a server and run it.
- **Session branching with cache sharing.** Facet bots, spawn modes, and clone sessions share the parent session's cache prefix. Parallel conversations without parallel cost.
- **Tmux-native coding agent orchestration.** Full lifecycle management — start sessions, send commands, watch for inactivity, autopilot with automatic completion notifications. OpenClaw can use tmux via shell, but foci's built-in tool adds autopilot mode and simplifies key-sending.

### What you lose

Be honest with yourself about whether these matter for your use case:

- **Linux only.** Foci runs on Linux (bare metal or Docker). No native macOS or Windows support. OpenClaw runs everywhere Node.js does.
- **Multi-platform support.** Telegram is the only messaging platform today. Other platforms are coming — the platform abstraction is clean and PRs are welcome. If you need WhatsApp, Discord, or Slack *right now*, stay on OpenClaw until those land.
- **Skill marketplace (by default).** Foci can use ClawHub skills — the format is compatible — but doesn't ship with marketplace integration enabled. This is a deliberate security choice: marketplace skills are untrusted code that gets injected into your agent's context. If you want ClawHub access, your agent can set it up. But the default is locked down.
- **Native apps.** No macOS or iOS clients. Android app is in development.
- **WebChat UI / TUI.** No `openclaw tui` equivalent. No Control UI.
- **Model failover chains.** Foci supports multiple endpoints but not ordered fallback chains.
- **Canvas / A2UI.** No visual workspace.
- **Binding-based routing.** Foci routes by bot, not by specificity hierarchy.

See [COMPARISON.md](COMPARISON.md) for the full feature matrix.

## Concepts Mapping

| Concept | OpenClaw | Foci | Notes |
|---------|----------|------|-------|
| API credentials | `.env` / inline env vars | `secrets.toml` | Separate file, inaccessible to agent context |
| Agent identity | Workspace root files (fixed names) | `system_files` (any names, any order) | Foci's character system is format-agnostic — use any file names, any number of files |
| Long-term memory | `MEMORY.md` + `memory/YYYY-MM-DD.md` | Same | Same daily log pattern, same curated memory file |
| Memory capture | `compaction.memoryFlush` (pre-compaction) | `[reflection]` | Foci adds interval-based and session-end reflection (memory + autogenerated skill formation). Session-end runs on a branch so it doesn't block the caller. |
| Periodic tasks | `heartbeat` | `[keepalive]` + `[background]` | Split into cache-warming (keepalive) and idle work (background) |
| Skills | `~/.openclaw/skills/` (global) | Per-agent `skills/` dirs | Per-agent isolation; ClawHub compatible but not enabled by default |
| MCP servers | `mcporter` | `mcp.toml` | Same protocol, different config |
| Queue behaviour | 7 modes with per-mode debounce/cap/drop | Steer + FIFO | Foci covers steer and queue; OpenClaw adds collect, followup, interrupt, and variants |

See [CONFIG.md](CONFIG.md) for the complete foci configuration reference.

## Character File Migration

**Foci's character system is format-agnostic.** The `system_files` config key is an ordered list of markdown file paths — foci reads them in order and injects them into the system prompt. It doesn't care what they're called or how they're structured internally.

This means **you can bring your OpenClaw character files as-is.** Copy them into your workspace, list them in `system_files`, and they'll work. No reformatting required.

Foci ships with two sets of default templates for new agents: foci-native defaults (`shared/character/`) and OpenClaw-format defaults (`shared/openclaw/`). Use whichever you prefer, or neither — write your own from scratch.

### Bringing your files directly

The simplest migration: copy your OpenClaw workspace files and point foci at them.

```bash
# Copy your character files
cp /path/to/openclaw/workspace/{IDENTITY,SOUL,AGENTS,TOOLS,USER,MEMORY}.md \
   /home/foci/myagent/character/

# Tell foci which files to load (in foci.toml, under [[agents]])
system_files = [
  "character/IDENTITY.md",
  "character/SOUL.md",
  "character/AGENTS.md",
  "character/TOOLS.md",
  "character/USER.md",
  "character/MEMORY.md",
]
```

That's it. Your agent will load these files exactly as they are.

### If you want foci-native naming

Foci's default file structure uses different names that reflect different framing. If you want to adopt foci conventions:

| OpenClaw file | Foci convention | What changes |
|--------------|----------------|--------------|
| `IDENTITY.md` | `SOUL.md` | Merge identity info into SOUL.md's header |
| `SOUL.md` | `SOUL.md` | Same purpose, same file |
| `AGENTS.md` | `COHERENCE.md` | Reframed as "how I understand myself" — same content, different lens |
| `TOOLS.md` | `CRAFT.md` | Renamed; review tool-specific instructions (foci's tools differ) |
| `USER.md` | `USER.md` | Identical |
| `MEMORY.md` | `MEMORY.md` | Identical |
| `HEARTBEAT.md` | `KEEPALIVE.md` | Optional; foci has built-in keepalive prompts |

### Notes

- **Blank files are skipped.** If a file is empty, foci won't inject it into the system prompt. No need to delete unused files.
- **COHERENCE.md is foci-specific.** This file explains how the other character files relate to each other — a meta-integration layer. OpenClaw doesn't have this concept. It's optional but valuable for agent self-consistency.

## Memory Migration

### What transfers directly

- **MEMORY.md** — Copy as-is to `character/MEMORY.md`. Same format, same purpose.
- **Daily memory files** — Copy `memory/YYYY-MM-DD.md` files to `{workspace}/memory/`. Foci indexes these for search.

### What starts fresh

- **Session history** — OpenClaw's session transcripts are not compatible. Sessions start clean.
- **Search index** — Rebuilt automatically on first startup from memory files.
- **Todo/task list** — Not transferable. Use `/todo new` to recreate important items.
- **Scratchpad** — Starts empty.

### Reflection differences

OpenClaw uses `compaction.memoryFlush` — a pre-compaction agentic turn that prompts the agent to store durable memories before the session is summarized. Foci separates this into three independent mechanisms, all living under the unified reflection pass:

- **Interval reflection** (`[reflection] interval_enabled`) — Periodic capture during the session, not just at compaction. Covers both memory and autogenerated skill formation.
- **Session-end reflection** (`session_end_enabled`) — Fires on `/reset` and facet reclaim. Runs on an async branch so it doesn't block the caller.
- **Consolidation** (`consolidation_enabled`) — Periodic curation of MEMORY.md from daily files with a configurable size target.

## Tool Capabilities

The agent discovers tools automatically — you don't need to teach it tool names. This section covers capability differences so you know what your agent can and can't do.

### Equivalent capabilities

| Capability | Differences in foci |
|-----------|-------------------|
| **Shell execution** | [Tool piping](TOOLS.md#tool-piping-exec-bridge): shell commands can call `foci_read`, `foci_web_fetch`, and other tools as shell functions, chaining tools through the shell. Any command running longer than 10 seconds is auto-backgrounded with results delivered asynchronously. Process-group kill on timeout (not just the shell process). |
| **File read/write/edit** | `edit` validates syntax on save for Go, JSON, TOML, YAML, Python, XML, shell. Catches errors before the agent moves on. |
| **Web search** | Brave Search API by default. Optionally use Anthropic server-side search (`search_provider = "anthropic"`). |
| **Web fetch** | Client-side with readability extraction by default. |
| **Sub-agent spawning** | Four context modes with different cost/capability tradeoffs: `raw` (prompt only, no tools), `character` (identity only), `explore` (read-only, Haiku), `clone` (full async branch with tools and the parent's conversation context). |
| **Image generation** | Both platforms ship this as a skill, not a core tool. Foci's `image-gen` skill uses OpenRouter; OpenClaw's uses the OpenAI Images API. |

### Capabilities foci adds

| Capability | What it does |
|-----------|-------------|
| **Tmux integration** | Full coding agent orchestration — start sessions, send commands, watch for inactivity, autopilot with automatic completion notifications. OpenClaw can use tmux via shell but has no built-in lifecycle management or autopilot. |
| **Domain-locked HTTP** | `http_request` carries credentials via `{{secret:NAME}}` templates without the agent seeing the secret value. Requests are locked to allowed hostnames per-secret. |
| **Todo list** | Persistent task tracking with priority levels and tags. Survives compaction. `background`-tagged items drive idle work. |
| **Scratchpad** | Working notes that survive compaction. The agent keeps its working state across context resets. |
| **Reminders** | Deferred messages — by duration, date, or "tomorrow". Injected into context when due. |
| **Low-cost summarization** | `summary` tool calls Haiku to extract information from files without loading them into the main context window. |
| **Bitwarden vault** | Search vault items, unlock specific entries with admin approval via Telegram buttons. Credentials cached with configurable TTL. |
| **Memory search** | FTS5/Bleve full-text search across memory files and conversation history, with per-source weighting and date filtering. |
| **Tool result guard** | Oversized tool output is auto-summarized via Haiku to preserve meaningful context while conserving tokens. Configurable: `max_result_chars` (threshold, default 15k), `max_summary_chars` (ceiling, default 300k), `auto_summarise` (on/off). OpenClaw has a separate `contextPruning` system that trims old tool results based on cache TTL — different approach to the same problem. |
| **Mid-turn nudges** | Behavioral rules extracted from character files and injected between tool calls — targeted reminders to keep important rules fresh. |

### Capabilities OpenClaw has that foci doesn't

| Capability | Notes |
|-----------|-------|
| Canvas / A2UI | No visual workspace. |

OpenClaw ships pre-built webhook integrations (e.g. Gmail Pub/Sub pipeline). Foci has a generic hook system that supports arbitrary webhook integrations but doesn't ship specific pre-built ones — you configure the endpoint and the agent handles the payload.

## What's Missing

Features OpenClaw has that foci doesn't:

| Feature | Status |
|---------|--------|
| WhatsApp, Discord, Slack, Teams | Telegram is the only platform today. Others are coming. PRs welcome — the platform abstraction is clean. |
| Native macOS/iOS apps | No plans. Android app is in development. |
| ClawHub skill marketplace (default) | Compatible but not enabled by default — security risk. Agent can set it up on request. |
| WebChat UI | Not planned. |
| CLI TUI (`openclaw tui`) | Not planned. |
| Model failover chains | Not implemented. Single model per agent. |
| Canvas / A2UI | Not planned. |
| Binding-based message routing | Not planned. Per-bot routing only. |
| `tools.fs.workspaceOnly` | Not implemented. Agent has full filesystem access. |
| Temporal decay scoring in memory | Not implemented. FTS5 uses relevance ranking. |
| Config hot-reload (automatic) | Use `/reload` command after editing config. |

## Quick Start

Three ways to import your OpenClaw workspace into foci:

- **Setup wizard:** `./setup.sh` walks you through everything interactively. See [INSTALL.md](INSTALL.md).
- **Command line:** `foci agents new --id myagent --char-mode import --char-import-dir /path/to/openclaw/workspace` copies your character files as-is.
- **Docker:** Set `FOCI_CHAR_MODE=import` and `FOCI_CHAR_IMPORT_DIR=/path/to/workspace` in your `.env`. See [INSTALL.md](INSTALL.md).

Your OpenClaw character files work without modification — just set `system_files` in your agent config to list them in the order you want. Put stable files first, MEMORY.md last.

For full configuration details, see [CONFIG.md](CONFIG.md). For architecture context, see [WIRING.md](WIRING.md).
