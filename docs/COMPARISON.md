# Feature Comparison: Foci vs OpenClaw vs Nanobot vs Hermes

Foci, [OpenClaw](https://github.com/openclaw/openclaw), [Nanobot](https://github.com/HKUDS/nanobot), and [Hermes Agent](https://github.com/NousResearch/hermes-agent) are self-hosted AI agent platforms. OpenClaw is the established, broadly-featured choice. Foci makes different architectural bets — trading breadth of platform and provider support for depth in cache architecture, secret isolation, and operational simplicity. Nanobot is a lightweight Python alternative. Hermes Agent is Nous Research's MIT-licensed agent, notable for self-improving skills, broad messaging-platform support, and a large community.

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Language | Go | Node.js/TypeScript | Python | Python |
| Binary size / runtime | ~50 MB static binary | Node 22+, ~500 MB+ | Python 3.11+, pip | Python 3.11+ + Node/ffmpeg |
| Typical RAM | ~35 MB | ~500 MB+ | ~45 MB | — |
| Core LOC | ~53k | ~250k+ | ~4k | — |
| License | Proprietary | MIT | MIT | MIT |

## Security

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Secrets never visible to agent | ✅ | ❌ | ❌ | ✅ strips secret env from child procs |
| Secret redaction | ✅ always | ✅ logging only | ❌ | ✅ tool output |
| Domain-locked HTTP | ✅ per-secret allowed hosts | ❌ | ❌ | ❌ blocklist/SSRF only |
| OS-level file permissions | ✅ Unix group enforcement | ✅ 600/700 modes | ❌ | ✅ 0600 creds |
| Native sandbox / container isolation | ✅ can run in Docker | ✅ Docker per-session/agent | ✅ can run in Docker | ✅ Docker + Modal/Daytona |
| Workspace-only filesystem | ❌ | ✅ `tools.fs.workspaceOnly` | ✅ `restrictToWorkspace` | ✅ container only |
| User allowlist | ✅ | ✅ | ✅ | ✅ |
| Security audit | ✅ verifies secret store integrity on startup | ✅ deep | ❌ | ❌ |
| Bitwarden integration | ✅ approval-gated unlock | ❌ | ❌ | ❌ |
| Elevated mode escape hatch | ❌ [aisudo](https://github.com/richardtkemp/ai-sudo) | ✅ `tools.elevated` | ❌ | ✅ `/yolo` |

## Session Management

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Session branching | ✅ cron/spawn/facet | ❌ | ❌ | ❌ |
| Crash recovery | ✅ orphan repair on startup | ❌ | ❌ | ❌ manual `/resume` only |
| Parallel conversations | ✅ facet bot pool | ❌ | ❌ | ✅ |

## Tool System

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Shell execution | ✅ with async execution | ✅ | ✅ | ✅ |
| Permission auto-approve | ✅ glob rules with shell-operator splitting | ✅ exec allowlist / ask modes | ❌ | ✅ allowlist + ask modes |
| File read/write/edit | ✅ syntax validation on edit | ✅ | ✅ | ✅ |
| HTTP requests | ✅ domain-locked API secret protection | ❌ | ❌ | ❌ |
| Web search | ✅ Anthropic or Brave | ✅ Brave | ✅ Brave | ✅ |
| Web fetch | ✅ | ✅ | ✅ | ✅ |
| Low-cost summarization | ✅ Haiku-powered | ❌ | ❌ | ✅ cheap-model compaction |
| Tmux integration | ✅ full lifecycle, autopilot | ❌ | ❌ | ❌ |
| Browser automation | ✅ full CDP via go-rod | ✅ full CDP control | ❌ | ✅ 10+ browser tools |
| Reminders / alarms | ✅ time/duration/date | ✅ via cron tool | ❌ | ✅ via cron tool |
| Scratchpad | ✅ survives compaction | ❌ | ❌ | ❌ |
| Todo / task list | ✅ priority + tags | ❌ | ❌ | ✅ |
| Cross-session messaging | ✅ | ✅ | ❌ | ✅ |
| Sub-agent spawning | ✅ 3 context modes | ✅ `sessions_spawn` | ✅ background | ✅ `delegate_task` |
| Canvas / visual workspace | ❌ | ✅ A2UI, HTML/CSS/JS | ❌ | ❌ |
| PDF analysis | ✅ native document blocks | ✅ | ❌ | ❌ URL extract only |
| Tool result guard | ✅ auto-summarize large output to preserve meaningful context while conserving tokens | ❌ | ✅ truncation at 500 chars | ✅ truncation |
| [Tool piping](TOOLS.md#tool-piping-exec-bridge) | ✅ tools ↔ shell ↔ each other | ❌ | ❌ | ❌ |
| Loop detection | ✅ configurable threshold | ✅ pattern-based detectors | ❌ | ❌ |
| Mid-turn behavioral nudges | ✅ LLM-extracted from character files, 5 trigger types | ❌ | ❌ | ✅ budget/memory nudges |

## Prompt Caching

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Anthropic cache support | ✅ | ✅ | ✅ | ✅ |
| Cache breakpoints | ✅ 2 per request | ✅ configurable | ✅ on system prompt | ✅ up to 4 per request |
| Cache keepalive | ✅ | ✅ | ❌ | ✅ 1h TTL |
| Cache-aware architecture | ✅ all design decisions | ❌ very frequent cache busts | ❌ | ✅ frozen-snapshot prefix |
| Cache monitoring | ✅ `/cache` + SQLite log | ✅ JSONL trace log | ❌ | ✅ dashboard hit-rate |
| Cache bust detection | ✅ alerts on >50% drop | ❌ | ❌ | ❌ |
| Cache preservation on branch | ✅ shares parent prefix | ❌ | ❌ | ❌ |

## Context Management

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Preserved messages | ✅ configurable count | ✅ configurable token count | ❌ | ✅ `protect_last_n` |
| Scratchpad preservation | ✅ survives compaction | ❌ | ❌ | ❌ |
| Compaction archives | ✅ timestamp-based rotation | ✅ stored in transcript | ❌ | ❌ in-place summary |
| Async-pending guard | ✅ defers if results pending | ❌ | ❌ | ❌ |
| Context breakdown | ✅ | ✅ | ❌ | ❌ aggregate only |

## Memory System

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Search method | ✅ fast, local, powerful (FTS5/Bleve) | ✅ various good options, including semantic search | Grep via exec | ✅ FTS5 (semantic via plugins) |
| Weighted memory sources | ✅ per-source multipliers | ❌ | ❌ | ❌ |
| Conversation indexing | ✅ | ✅ | ❌ | ✅ FTS5 |
| Auto-reindex on change | ✅ fsnotify | ✅ delta thresholds + file-watch | ❌ | ❌ |
| Periodic consolidation | ✅ built-in explicit task, sensible defaults | ✅ "Dreaming" nightly cron | ✅ threshold-based | ❌ agent-triggered only |
| Interval memory formation | ✅ built-in explicit task, sensible defaults | ❌ | ❌ | ✅ nudge-based |
| Temporal decay scoring | ❌ | ✅ configurable half-life | ❌ | ❌ |
| Per-agent memory isolation | ✅ | ✅ | ❌ | ✅ |

## Cost & Usage Tracking

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Per-turn cost display | ✅ injected as metadata | ✅ `/usage full` footer | ❌ | ✅ status bar |
| Cumulative cost tracking | ✅ `/cost` | ✅ `/usage cost` | ❌ | ✅ `/usage` |
| Quota monitoring | ✅ Anthropic usage API | ❌ | ❌ | ✅ Google only (`/gquota`) |
| API call log | ✅ JSONL + SQLite | ✅ JSONL | ❌ | ❌ |
| Budget gating | ✅ smart scheduling of background work to preserve quota | ❌ | ❌ | ✅ turn-budget |
| Full payload recording | ✅ optional | ✅ optional | ❌ | ❌ |

## Multi-Agent

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Multiple agents per process | ✅ | ✅ | ❌ | ✅ profiles |
| Per-agent config override | ✅ | ✅ | ❌ | ✅ profiles |
| Agent-to-agent messaging | ✅ | ✅ | ❌ | ❌ orchestrator→worker only |
| Agent isolation | ✅ secrets only accessible to their owning agent | ✅ | ❌ | ✅ |
| Binding-based routing | ❌ per-bot routing | ✅ specificity hierarchy | ❌ | ❌ |
| Orchestrator pattern | ✅ spawn + clone | ✅ spawning + sub-agents | ✅ SpawnTool | ✅ orchestrator-worker |

## Message Handling

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Message transforms (regex) | ✅ per-agent | ❌ | ❌ | ❌ |
| Queue modes | NYI | ✅ steer/followup/collect/interrupt | ❌ | ✅ interrupt/queue/steer |
| Block streaming | NYI | ✅ chunked delivery | ❌ | ✅ progressive edits |
| Deferred partial replies | ✅ batch or immediate | ❌ | ❌ | ❌ |
| Tool call display modes | ✅ off/preview/full | ✅ verbose toggle | ❌ | ✅ off/new/all/verbose |
| [Optimised comprehension](https://arxiv.org/abs/2512.14982) | ✅ | ❌ | ❌ | ❌ |

## Scheduling & Automation

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Cron jobs | ✅ via system cron + CLI | ✅ built-in CronService | ✅ CronService | ✅ built-in scheduler |
| Heartbeat / periodic tasks | ✅ keepalive + background work | ✅ configurable interval | ✅ HeartbeatService | ✅ |
| Webhook ingestion | ✅ config-declared hooks | ✅ token-validated hooks | ❌ | ❌ |
| Idle-triggered background work | ✅ mana-gated, todo-driven | ❌ | ❌ | ❌ |
| Gmail integration | ❌ | ✅ Pub/Sub | ❌ | ❌ generic email only |

## Voice & Speech

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Speech-to-text | ✅ | ✅ | ❌ | ✅ |
| Text-to-speech | ✅ | ✅ | ❌ | ✅ |
| Voice mode toggle | ✅ | ✅ | ❌ | ✅ |
| Wake word detection | ❌ | ✅ native apps only | ❌ | ❌ |
| Voice note transcription | ✅ | ✅ | ✅ | ✅ |

## Model & Provider Support

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Anthropic Claude | ✅ | ✅ | ✅ | ✅ |
| Other popular platforms | ✅ Gemini, OpenAI-compatible (OpenRouter, DeepSeek, etc.) | ✅ | ✅ | ✅ Gemini, OpenAI-compat, 300+ via OpenRouter |
| Coding agent backends | ✅ Claude Code (stream-JSON + tmux), OpenCode (HTTP/SSE) | ✅ Claude Code / Codex / OpenCode runtimes | ❌ | ✅ via ACP (Copilot, Claude, Codex) |
| Model failover chain | ❌ | ✅ ordered fallbacks | ❌ | ✅ `fallback_providers` |
| Model aliasing | ✅ | ✅ | ✅ | ✅ |
| Per-session model switch | ✅ | ✅ | ❌ | ✅ `/model` |
| Extended thinking | ✅ | ✅ | ✅ | ✅ `reasoning_effort` |

## Workspace Bootstrap Files

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| IDENTITY.md | ✅ | ✅ | ✅ | ❌ (SOUL.md covers it) |
| SOUL.md | ✅ | ✅ | ✅ | ✅ |
| AGENTS.md | ✅ | ✅ | ✅ | ✅ |
| TOOLS.md | ✅ | ✅ | ✅ | ❌ |
| USER.md | ✅ | ✅ | ✅ | ✅ |
| MEMORY.md | ✅ | ✅ | ✅ | ✅ |
| HEARTBEAT.md | ✅ | ✅ | ✅ | ✅ |
| COHERENCE.md | ✅ | ❌ | ❌ | ❌ |
| Configurable file order | ✅ cache-optimal ordering | ❌ | ❌ | ❌ |
| Blank file skipping | ✅ | ✅ | ❌ | ✅ |

## Skills & Plugins

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Skill format | SKILL.md + frontmatter | ClawHub registry | SKILL.md + frontmatter | SKILL.md + frontmatter |
| Self-improving skills | ✅ | ❌ | ❌ | ✅ |
| Skill marketplace | ❌ | ✅ ClawHub, 13.7k+ skills | ✅ ClawHub integration | ✅ Skills Hub (multi-source) |
| Per-agent skill dirs | ✅ | ✅ | ❌ | ✅ |
| Progressive disclosure | ❌ full inject or on-demand | ❌ | ✅ summary + read on demand | ✅ 3-level disclosure |
| Plugin system | ❌ | ✅ channel plugins | ❌ | ✅ |

## MCP Support

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| MCP client (consume tools) | ✅ via mcp.toml | ✅ via mcporter | ✅ stdio + HTTP | ✅ stdio + HTTP |
| MCP server (expose agents) | ❌ | ✅ `openclaw mcp serve` | ❌ | ✅ `hermes mcp serve` |
| MCP ecosystem access | ✅ via mcp.toml | ✅ 13k+ servers | ✅ | ✅ curated catalog |

## Configuration

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Config format | TOML | JSON5 | JSON | YAML |
| Separate secrets file | ✅ inaccessible to agent | ❌ inline SecretRef | ❌ env vars | ✅ `.env` |
| Per-agent overrides | ✅ all keys | ✅ per agent | ❌ | ✅ via profiles |
| Hot-reload | ✅ `/reload` | ✅ automatic | ❌ | ✅ partial |
| Interactive setup wizard | ✅ setup.sh | ✅ `openclaw onboard` | ✅ `nanobot onboard` | ✅ `hermes setup` |
| Validation at load | ✅ warns on unknown keys | ✅ strict, blocks startup | ✅ Pydantic schema | ❌ on-demand only |
| Env var substitution | ❌ | ✅ `${VAR}` syntax | ❌ | ✅ `${VAR}` syntax |
| Timezone override | ✅ IANA timezone per-instance | ✅ `userTimezone` (IANA) | ❌ | ✅ `HERMES_TIMEZONE` (IANA) |

## Chat Channels

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Telegram | ✅ | ✅ | ✅ | ✅ |
| Discord | ✅ | ✅ | ❌ | ✅ |
| Other popular platforms | ❌ | ✅ | ✅ | ✅ 24+ platforms |
| Android app | 🔜 coming soon | ✅ andClaw (Play Store) | ❌ | ❌ Termux only |
| WebChat UI | ❌ | ✅ Control UI | ❌ | ✅ Open WebUI |
| CLI interactive mode | ❌ | ✅ `openclaw tui` | ✅ `nanobot agent` | ✅ |
| HTTP gateway | ✅ REST API | ✅ WebSocket hub | ❌ | ✅ REST (OpenAI-compatible) |

## System Monitoring

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Process memory monitoring | ✅ tmux + system guards | ❌ | ❌ | ❌ (systemd-delegated) |
| Memory pressure gating | ✅ PSI-based | ❌ | ❌ | ❌ |
| Auto-kill runaway processes | ✅ SIGTERM→SIGKILL | ❌ | ❌ | ❌ |
| Warning queue injection | ✅ passive + proactive | ❌ | ❌ | ❌ |
| Log rotation | ✅ built-in + logrotate | ✅ built-in size-based | ❌ | ❌ |
| Startup crash diagnosis | ✅ clean/crash/reboot | ❌ | ❌ | ❌ |

## Deployment

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Single binary | ✅ | ❌ Node.js runtime | ❌ Python runtime | ❌ Python runtime |
| systemd integration | ✅ setup.sh | ✅ `openclaw onboard` | ✅ user service | ✅ documented |
| Docker | ✅ Dockerfile + Compose | ✅ Compose + sandbox | ✅ Compose | ✅ Compose + sandbox |
| Nix | ❌ | ✅ | ❌ | ✅ flake + NixOS module |
| Native apps (macOS/iOS/Android) | 🔜 Android coming soon | ✅ macOS + Android (iOS alpha) | ❌ | ❌ desktop only (Electron) |
| Idempotent setup script | ✅ | ✅ | ✅ | ✅ curl installer |

## Platform Support

| | **Foci** | **OpenClaw** | **Nanobot** | **Hermes** |
|---|---|---|---|---|
| Linux | ✅ | ✅ | ✅ | ✅ |
| macOS | NYI | ✅ native app | ✅ | ✅ |
| iOS app | ❌ | ❌ alpha, unreleased | ❌ | ❌ |
| Android app | 🔜 client, coming soon | ✅ | ❌ | ❌ Termux only |
| Camera / screen control | ❌ | ✅ Apple only | ❌ | ❌ |
