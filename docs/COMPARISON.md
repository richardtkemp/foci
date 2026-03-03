# Feature Comparison: Foci vs OpenClaw vs Nanobot

Foci, [OpenClaw](https://github.com/openclaw/openclaw), and [Nanobot](https://github.com/HKUDS/nanobot) are self-hosted AI agent platforms. This table compares their capabilities as of March 2026.

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Language | Go | Node.js/TypeScript | Python |
| Binary size / runtime | ~15 MB static binary | Node 22+, ~500 MB+ | Python 3.11+, pip |
| Typical RAM | ~30 MB | ~500 MB+ | ~45 MB |
| Core LOC | ~35k | ~250k+ | ~4k |
| License | Proprietary | MIT | MIT |

## Model & Provider Support

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Anthropic Claude | ✅ | ✅ | ✅ |
| Other popular platforms | ❌ | ✅ | ✅ |
| Model failover chain | ❌ | ✅ ordered fallbacks | ❌ |
| Model aliasing | ✅ | ✅ | ✅ |
| Per-session model switch | ✅ | ✅ | ❌ |
| Extended thinking | ✅ | ✅ | ✅ |

## Chat Channels

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Telegram | ✅ | ✅ | ✅ |
| Other popular platforms | ❌ | ✅ | ✅ |
| Android app | 🔜 coming soon | 🔜 coming soon | ❌ |
| WebChat UI | ❌ | ✅ Control UI | ❌ |
| CLI interactive mode | ❌ | ✅ `openclaw tui` | ✅ `nanobot agent` |
| HTTP gateway | ✅ REST API | ✅ WebSocket hub | ❌ |

## Tool System

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Shell execution | ✅ with async execution | ✅ | ✅ |
| File read/write/edit | ✅ syntax validation on edit | ✅ | ✅ |
| HTTP requests | ✅ domain-locked API secret protection | ❌ | ❌ |
| Web search | ✅ Anthropic or Brave | ✅ Brave | ✅ Brave |
| Web fetch | ✅ | ✅ | ✅ |
| Low-cost summarization | ✅ Haiku-powered | ❌ | ❌ |
| Tmux integration | ✅ full lifecycle, autopilot | ❌ | ❌ |
| Browser automation | ❌ | ✅ full CDP control | ❌ |
| Reminders / alarms | ✅ time/duration/date | ❌ | ❌ |
| Scratchpad | ✅ survives compaction | ❌ | ❌ |
| Todo / task list | ✅ priority + tags | ❌ | ❌ |
| Cross-session messaging | ✅ | ✅ | ❌ |
| Sub-agent spawning | ✅ 3 context modes | ✅ `sessions_spawn` | ✅ background |
| [Bitwarden vault access](SECRETS.md) | ✅ approval-gated | ❌ | ❌ |
| Canvas / visual workspace | ❌ | ✅ A2UI, HTML/CSS/JS | ❌ |
| PDF analysis | ❌ | ✅ | ❌ |
| Tool result guard | ✅ auto-summarize large output to preserve meaningful context while conserving tokens | ❌ | ✅ truncation at 500 chars |
| [Tool piping](TOOLS.md#tool-piping-exec-bridge) | ✅ tools ↔ shell ↔ each other | ❌ | ❌ |
| Loop detection | ✅ configurable threshold | ✅ pattern-based detectors | ❌ |

## Session Management

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Session branching | ✅ cron/spawn/multiball | ❌ | ❌ |
| Crash recovery | ✅ orphan repair on startup | ❌ | ❌ |
| Parallel conversations | ✅ multiball bot pool | ❌ | ❌ |

## Context Management

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Preserved messages | ✅ configurable count | ✅ configurable token count | ❌ |
| Scratchpad preservation | ✅ survives compaction | ❌ | ❌ |
| Compaction archives | ✅ numbered rotation | ✅ stored in transcript | ❌ |
| Async-pending guard | ✅ defers if results pending | ❌ | ❌ |
| Context breakdown | ✅ | ✅ | ❌ |

## Prompt Caching

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Anthropic cache support | ✅ | ✅ | ✅ |
| Cache breakpoints | ✅ 2 per request | ✅ configurable | ✅ on system prompt |
| Cache keepalive | ✅ | ✅ | ❌ |
| Cache-aware architecture | ✅ all design decisions | ❌ very frequent cache busts | ❌ |
| Cache monitoring | ✅ `/cache` + SQLite log | ✅ JSONL trace log | ❌ |
| Cache bust detection | ✅ alerts on >50% drop | ❌ | ❌ |
| Cache preservation on branch | ✅ shares parent prefix | ❌ | ❌ |

## Memory System

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Search method | ✅ fast, local, powerful (FTS5/Bleve) | ✅ various good options, including semantic search | Grep via exec |
| Weighted memory sources | ✅ per-source multipliers | ❌ | ❌ |
| Conversation indexing | ✅ | ✅ | ❌ |
| Auto-reindex on change | ✅ fsnotify | ❌ | ❌ |
| Periodic consolidation | ✅ built-in explicit task, sensible defaults | ❌ manual | ✅ threshold-based |
| Interval memory formation | ✅ built-in explicit task, sensible defaults | ❌ | ❌ |
| Temporal decay scoring | ❌ | ✅ configurable half-life | ❌ |
| Per-agent memory isolation | ✅ | ✅ | ❌ |

## Cost & Usage Tracking

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Per-turn cost display | ✅ injected as metadata | ✅ `/usage full` footer | ❌ |
| Cumulative cost tracking | ✅ `/cost` | ✅ `/usage cost` | ❌ |
| Quota monitoring | ✅ Anthropic usage API | ❌ | ❌ |
| API call log | ✅ JSONL + SQLite | ✅ JSONL | ❌ |
| Budget gating | ✅ smart scheduling of background work to preserve quota | ❌ | ❌ |
| Full payload recording | ✅ optional | ✅ optional | ❌ |

## Multi-Agent

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Multiple agents per process | ✅ | ✅ | ❌ |
| Per-agent config override | ✅ | ✅ | ❌ |
| Agent-to-agent messaging | ✅ | ✅ | ❌ |
| Agent isolation | ✅ secrets only accessible to their owning agent | ✅ | ❌ |
| Binding-based routing | ❌ per-bot routing | ✅ specificity hierarchy | ❌ |
| Orchestrator pattern | ✅ spawn + clone | ✅ spawning + sub-agents | ✅ SpawnTool |

## Voice & Speech

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Speech-to-text | ✅ | ✅ | ❌ |
| Text-to-speech | ✅ | ✅ | ❌ |
| Voice mode toggle | ✅ | ✅ | ❌ |
| Wake word detection | ❌ | ✅ native apps only | ❌ |
| Voice note transcription | ✅ | ✅ | ✅ |

## Security

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Secrets never visible to agent | ✅ | ❌ | ❌ |
| Secret redaction | ✅ always | ✅ logging only | ❌ |
| Domain-locked HTTP | ✅ per-secret allowed hosts | ❌ | ❌ |
| OS-level file permissions | ✅ Unix group enforcement | ✅ 600/700 modes | ❌ |
| Native sandbox / container isolation | ✅ can run in Docker | ✅ Docker per-session/agent | ✅ can run in Docker |
| Workspace-only filesystem | ❌ | ✅ `tools.fs.workspaceOnly` | ✅ `restrictToWorkspace` |
| User allowlist | ✅ | ✅ | ✅ |
| Security audit | ✅ verifies secret store integrity on startup | ✅ deep | ❌ |
| Bitwarden integration | ✅ approval-gated unlock | ❌ | ❌ |
| Elevated mode escape hatch | ❌ [aisudo](https://github.com/richardtkemp/ai-sudo) | ✅ `/elevated` | ❌ |

## Configuration

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Config format | TOML | JSON5 | JSON |
| Separate secrets file | ✅ inaccessible to agent | ❌ inline SecretRef | ❌ env vars |
| Per-agent overrides | ✅ all keys | ✅ per agent | ❌ |
| Hot-reload | ✅ `/reload` | ✅ automatic | ❌ |
| Interactive setup wizard | ✅ setup.sh | ✅ `openclaw onboard` | ✅ `nanobot onboard` |
| Validation at load | ✅ warns on unknown keys | ✅ strict, blocks startup | ✅ Pydantic schema |
| Env var substitution | ❌ | ✅ `${VAR}` syntax | ❌ |

## Workspace Bootstrap Files

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| IDENTITY.md | ✅ | ✅ | ✅ |
| SOUL.md | ✅ | ✅ | ✅ |
| AGENTS.md | ✅ | ✅ | ✅ |
| TOOLS.md | ✅ | ✅ | ✅ |
| USER.md | ✅ | ✅ | ✅ |
| MEMORY.md | ✅ | ✅ | ✅ |
| HEARTBEAT.md | ✅ | ✅ | ✅ |
| COHERENCE.md | ✅ | ❌ | ❌ |
| Configurable file order | ✅ cache-optimal ordering | ❌ | ❌ |
| Blank file skipping | ❌ always loaded | ✅ | ❌ |

## Scheduling & Automation

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Cron jobs | ✅ via system cron + CLI | ✅ built-in CronService | ✅ CronService |
| Heartbeat / periodic tasks | ✅ keepalive + background work | ✅ configurable interval | ✅ HeartbeatService |
| Webhook ingestion | ❌ | ✅ token-validated hooks | ❌ |
| Idle-triggered background work | ✅ mana-gated, todo-driven | ❌ | ❌ |
| Gmail integration | ❌ | ✅ Pub/Sub | ❌ |

## MCP Support

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| MCP client (consume tools) | ❌ | ✅ via mcporter | ✅ stdio + HTTP |
| MCP server (expose agents) | ❌ | ❌ | ❌ |
| MCP ecosystem access | ❌ | ✅ 13k+ servers | ✅ |

## Skills & Plugins

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Skill format | SKILL.md + frontmatter | ClawHub registry | SKILL.md + frontmatter |
| Skill marketplace | ❌ | ✅ ClawHub, 13.7k+ skills | ✅ ClawHub integration |
| Per-agent skill dirs | ✅ | ❌ | ❌ |
| Progressive disclosure | ❌ full inject or on-demand | ❌ | ✅ summary + read on demand |
| Plugin system | ❌ | ✅ channel plugins | ❌ |

## System Monitoring

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Process memory monitoring | ✅ tmux + system guards | ❌ | ❌ |
| Memory pressure gating | ✅ PSI-based | ❌ | ❌ |
| Auto-kill runaway processes | ✅ SIGTERM→SIGKILL | ❌ | ❌ |
| Warning queue injection | ✅ passive + proactive | ❌ | ❌ |
| Log rotation | ✅ built-in + logrotate | ❌ | ❌ |
| Startup crash diagnosis | ✅ clean/crash/reboot | ❌ | ❌ |

## Deployment

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Single binary | ✅ | ❌ Node.js runtime | ❌ Python runtime |
| systemd integration | ✅ setup.sh | ✅ `openclaw onboard` | ✅ user service |
| Docker | ❌ | ✅ Compose + sandbox | ✅ Compose |
| Nix | ❌ | ✅ | ❌ |
| Native apps (macOS/iOS/Android) | 🔜 Android coming soon | ✅ all three | ❌ |
| Idempotent setup script | ✅ | ✅ | ✅ |

## Message Handling

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Message transforms (regex) | ✅ per-agent | ❌ | ❌ |
| Queue modes | NYI | ✅ steer/followup/collect/interrupt | ❌ |
| Block streaming | NYI | ✅ chunked delivery | ❌ |
| Deferred partial replies | ✅ batch or immediate | ❌ | ❌ |
| Tool call display modes | ✅ off/preview/full | ✅ verbose toggle | ❌ |
| [Optimised comprehension](https://arxiv.org/abs/2512.14982) | ✅ | ❌ | ❌ |

## Platform Support

| | **Foci** | **OpenClaw** | **Nanobot** |
|---|---|---|---|
| Linux | ✅ | ✅ | ✅ |
| macOS | NYI | ✅ native app | ✅ |
| iOS app | ❌ | ✅ | ❌ |
| Android app | 🔜 client, coming soon | ✅ | ❌ |
| Camera / screen control | ❌ | ✅ Apple only | ❌ |
