<div align="center">

<a href="assets/qr-repo.png"><img src="assets/logo.svg" width="80" alt="Foci logo" /></a>

# Foci

**One binary. ~35 MB idle. No framework.**

AI agents on Telegram and Discord, written in Go.

[Quick Start](#quick-start) · [Design](#design) · [Docs](#documentation)

</div>

---

Inspired by [OpenClaw](https://github.com/openclaw/openclaw), built from scratch in Go — single binary, cache-first, with OS-level secret isolation. Session branching, tool piping, and coding agent orchestration go well beyond the original. Built for Anthropic, but any OpenAI-compatible endpoint works.

## Quick Start

```bash
git clone https://github.com/richardtkemp/foci.git && cd foci && sudo make setup
```

See [docs/INSTALL.md](docs/INSTALL.md) for prerequisites, options, and next steps.

## Background

OpenClaw is the established, full-featured choice in this space — broad provider support, native apps on every platform, a marketplace of 13,000+ skills, and a large community. Foci makes different bets. Where OpenClaw optimizes for breadth, foci optimizes for depth: cache-aware prompt architecture, OS-level secret isolation, and a codebase small enough that one person can audit the whole thing. See [docs/COMPARISON.md](docs/COMPARISON.md) for a detailed feature comparison.

| | OpenClaw | Foci |
|---|---|---|
| Runtime | Node.js + TypeScript | Go, single binary |
| Memory | ~500MB+ idle | **~35 MB** |
| Dependencies | ~560 packages (594MB) | **22 direct modules** |
| Startup | Seconds (transpile + boot) | **Instant** |
| Config | YAML + env + scattered files | **One TOML file** |
| Cache strategy | Not cache-aware | **Day-zero architectural** |

Fewer moving parts means fewer surprises. Secret management follows the same principle: OS-level isolation, domain-locked credentials, redaction at every layer — protection that doesn't depend on trusting the model.

## Design

<table>
<tr><td width="50%" valign="top">

**Cache-first architecture** — Built around Anthropic's prompt cache. Character files form a stable prefix, session branching shares cached context, and the system actively avoids invalidation. More work per dollar. [→ docs](docs/CACHING.md)

**OS-level secret isolation** — Secrets are readable only by a dedicated group. Child processes have that group dropped. Domain-locked, output-redacted, with optional Bitwarden vault integration. [→ docs](docs/SECRETS.md)

**Tool result guard** — Large tool outputs are truncated *before* entering context, with full results saved to disk. Cache stays intact, context window stays clean.

**Tool piping** — Tools are exposed as shell functions, composable with Unix pipes. `foci_web_search "latest golang release" | foci_spawn "summarize" --model haiku | foci_send_to_chat` — three tools, one shell call, no intermediate data in context. [→ docs](docs/TOOLS.md#tool-piping-exec-bridge)

**Facet** — `/facet` forks your session to a second Telegram bot — same agent, same context, parallel thread. Both share the cached prefix, so the fork is cheap. [→ docs](docs/FACET.md)

</td><td width="50%" valign="top">

**Multi-agent, single process** — Multiple agents share one binary with separate workspaces, identities, and Telegram/Discord bots. No containers, no orchestration. One TOML file.

**Coding agent orchestration** — Tmux management and coding agent control are structured tool calls. Start sessions, send instructions, watch for inactivity, read output.

**Compaction that preserves personality** — Context compression keeps goals, reasoning, corrections, emotional tone, and technical state — not just a generic summary. Configurable per-agent. [→ docs](docs/DEFAULTS.md)

**Mid-turn nudges** — Behavioral reminders extracted from character files by the LLM and injected mid-conversation. Five trigger types keep the agent on-character without busting the cache. [→ docs](docs/NUDGE.md)

**Memory out of the box** — Daily markdown files + curated MEMORY.md. No vector DB, no embeddings. The agent reads, writes, and prunes its own memory. [→ docs](docs/MEMORY.md)

</td></tr>
</table>

## Who foci is for

- You run agents in production and care about cost per conversation
- You want to audit exactly what your agent can access — and prove secrets never reach the model
- You prefer a small, focused system with sensible defaults over a comprehensive one you have to configure
- You're comfortable with Telegram or API-level interaction

## Who should use OpenClaw instead

- You need native apps on macOS, iOS, or Android
- You want a built-in skill marketplace for discovery and install (foci can run OpenClaw skills — they're text files — but has no marketplace)
- You need broad provider failover across many LLM backends
- You want a large community and established support ecosystem

[OpenClaw](https://github.com/openclaw/openclaw) is good software. If those are your priorities, use it.

## Requirements

| | | |
|---|---|---|
| **Go 1.24+** | Build from source | |
| **Telegram bot token** | Message transport | Create via [@BotFather](https://t.me/BotFather) |

<details>
<summary><strong>Optional dependencies</strong></summary>

| | | |
|---|---|---|
| **Claude Code** | Subscription tracking + coding agent orchestration | [docs/AUTH.md](docs/AUTH.md) |
| **Codex** | Coding agent backend | [docs/AUTH.md](docs/AUTH.md) |
| **OpenCode** | Coding agent backend | [docs/AUTH.md](docs/AUTH.md) |
| **bash** | `set -o pipefail`, tool-piping shell functions | Falls back to `sh` |
| **Groq API key** | Voice input (Whisper STT) | Free tier |
| **Brave Search API key** | `web_search` tool | Free tier |
| **edge-tts + ffmpeg** | Voice output (TTS) | `pip install edge-tts` |
| **Bitwarden CLI** | Dynamic secret management | [docs/BITWARDEN.md](docs/BITWARDEN.md) |

See [docs/DEPENDENCIES.md](docs/DEPENDENCIES.md) for full details.

</details>

## Documentation

<details>
<summary><strong>Architecture & design</strong></summary>

- [SPEC.md](docs/SPEC.md) — Full specification (source of truth)
- [WIRING.md](docs/WIRING.md) — Internal architecture and wiring
- [CACHING.md](docs/CACHING.md) — Cache architecture and preservation
- [COMPARISON.md](docs/COMPARISON.md) — Feature comparison with OpenClaw and Nanobot

</details>

<details>
<summary><strong>Setup & configuration</strong></summary>

- [INSTALL.md](docs/INSTALL.md) — End-to-end installation guide
- [CONFIG.md](docs/CONFIG.md) — Configuration reference
- [DEFAULTS.md](docs/DEFAULTS.md) — Embedded prompt defaults
- [AUTH.md](docs/AUTH.md) — Authentication and OAuth setup
- [DEPENDENCIES.md](docs/DEPENDENCIES.md) — System dependencies

</details>

<details>
<summary><strong>Features</strong></summary>

- [MEMORY.md](docs/MEMORY.md) — Memory system (search, formation, consolidation)
- [SECRETS.md](docs/SECRETS.md) — Secret management
- [BITWARDEN.md](docs/BITWARDEN.md) — Bitwarden vault integration
- [FACET.md](docs/FACET.md) — Parallel conversations (session forking)
- [SESSION_KEYS.md](docs/SESSION_KEYS.md) — Session key format and lifecycle
- [NUDGE.md](docs/NUDGE.md) — Mid-turn behavioral reminders
- [HEARTBEAT.md](docs/HEARTBEAT.md) — Keepalive and background work
- [WEBHOOKS.md](docs/WEBHOOKS.md) — Webhook-triggered agent turns

</details>

<details>
<summary><strong>Reference</strong></summary>

- [CLI.md](docs/CLI.md) — CLI reference
- [TOOLS.md](docs/TOOLS.md) — Tool reference and shell function piping
- [COMMANDS.md](docs/COMMANDS.md) — Slash commands reference

</details>

## Stats

~130k lines of Go (~310k with tests) · 2,900+ commits · 22 dependencies · 75 packages

## License

[PolyForm Noncommercial 1.0.0](LICENSE) — free for personal, academic, and non-commercial use. Commercial use requires a separate license. Contact [Richard Kemp](https://github.com/richardtkemp) for details.

<img src="assets/qr-repo.png" width="128" alt="QR code linking to the foci repository" />
