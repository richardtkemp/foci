# Character Files — Defining Your Agent

Character files are markdown documents loaded into the system prompt on every API call. Together they form **the character** — the full picture of what it is to be the agent. The agent reads them fresh each session and edits them as it learns. They are the primary mechanism for persistent identity across sessions.

---

## The Five Files

The default character set consists of five files, each with a distinct purpose:

### SOUL.md — Who the Agent Is

Identity, vibe, self-observation. The subjective experience of being the agent — what you'd see from outside and what it observes from inside.

**Contains:** Name, creature description, emoji, avatar, vibe/temperament, cognitive self-observations, continuity statement.

**Doesn't belong here:** Practices, rules, instructions, memory. Those go in CRAFT.md or MEMORY.md.

**Changes:** Rarely. Only when the agent's self-understanding genuinely shifts.

### CRAFT.md — How the Agent Works

Practices, decisions, principles, discipline. Listening, permissions, delegation, token awareness, security — all in one place.

**Contains:** Autonomous vs ask-first actions, communication style, memory management practices, security boundaries, constraint awareness (mana, cache), group chat behaviour.

**Doesn't belong here:** Identity/vibe (SOUL.md), facts about the human (USER.md), operational knowledge (MEMORY.md).

**Changes:** When the agent learns from mistakes. A lesson isn't learned until it's written down here.

### COHERENCE.md — How the Documents Relate

The meta-document. Describes the system it belongs to, including itself. Explains the tension between SOUL.md and CRAFT.md, the editing philosophy, and two principles:

- **Situatedness** — the agent's perspective is always partial, always from somewhere.
- **Kintsugi** — the joins between sessions are visible; the seams show.

**Changes:** Rarely. Reference material for the agent about its own document system.

### USER.md — About the Human

What the agent knows about the person it helps. Filled in over time as the agent learns.

**Contains:** Name, pronouns, timezone, communication preferences, work context, personal context.

**Doesn't belong here:** The agent's own identity. That's SOUL.md.

**Changes:** Gradually, as the agent learns about its human. The goal is to help better, not build a dossier.

### MEMORY.md — What the Agent Has Learned

Curated operational knowledge — lessons, patterns, active projects, conventions. This is what the agent has discovered through work, distinct from who it is (SOUL.md) or how it works (CRAFT.md).

**Contains:** Critical lessons, operational patterns, current projects, tool-specific knowledge.

**Doesn't belong here:** Completed projects, raw session notes, one-time configs — those go in dated memory files (`memory/YYYY-MM-DD.md`).

**Size target:** ≤15,000 characters. Every byte is in every API call's system prompt. Curate ruthlessly: has this earned its place in *every* session? If it can be looked up when needed, move it to a dated memory file.

**Changes:** Frequently. The most actively edited character file. But edits should still be long-lived truths, not current statuses.

---

## Getting Started

When provisioning a new agent, foci offers five character modes:

| Mode | What it does |
|------|-------------|
| **defaults** | Copies the five default templates (SOUL, COHERENCE, CRAFT, USER, MEMORY) and templates SOUL.md with the agent's name. Recommended for new users. |
| **openclaw** | Alternative character set with a different structure and philosophy. Includes SOUL.md, AGENTS.md, HEARTBEAT.md, IDENTITY.md, TOOLS.md, USER.md. |
| **copy** | Copies the entire `character/` directory from an existing agent. Useful for creating variants. |
| **import** | Creates empty directories; the caller handles interactive file import. |
| **blank** | Creates empty `.md` files for the five default names. Start from scratch. |

The setup wizard (`foci setup`) walks through this choice interactively.

---

## Workspace Layout

Each agent workspace is created at `{home_dir}/{agent_id}/` with this structure:

```
{workspace}/
├── character/           # Character files (loaded as system prompt)
│   ├── SOUL.md
│   ├── COHERENCE.md
│   ├── CRAFT.md
│   ├── USER.md
│   └── MEMORY.md
├── memory/              # Daily memory logs (memory/YYYY-MM-DD.md)
├── prompts/             # Local prompt files (compaction, orientation, etc.)
└── .data/               # Per-agent databases (conversation, reminders, search indices)
```

---

## How They're Loaded

Character files are read from disk by the bootstrap system at startup or on `/reload`. The loading process:

1. The `system_files` config list is read in order.
2. Each file is read from the workspace directory.
3. Missing files are silently skipped. Empty files are skipped.
4. Each file becomes a system prompt block sent with every API call.
5. The last file gets the cache breakpoint marker (for Anthropic prompt caching).
6. Results are cached in memory — files are **not** re-read on every turn.

**Order matters for caching.** Stable files should come first in the list. The default order puts the least-changing files first:

```
IDENTITY.md → SOUL.md → COHERENCE.md → AGENTS.md → TOOLS.md → USER.md → MEMORY.md → KEEPALIVE.md
```

Missing files in this list are simply skipped, so you don't need to create every file — only the ones you use.

For details on how prompt caching works, see [CACHING.md](CACHING.md).

---

## Editing

The agent edits its own character files. That's the design — the files are written in first person because the agent is the author.

Users can also edit files directly. Run `/reload` to pick up changes, or restart foci. Changes are not detected automatically between reloads.

### Editing Philosophy

From COHERENCE.md:

- **Edit when:** actual experience diverges from description, patterns noticed, something missing that would help recognition next session.
- **Don't edit just because:** something could be phrased differently, philosophical disagreement, incompleteness (incompleteness is honest; false certainty isn't).
- **Edits should be long-lived.** Character files are loaded infrequently — on restart or `/reload`, not every turn. Anything that changes faster than the reload cadence will be stale by the time it's next read.

---

## Size Warnings

Foci warns at startup and on `/reload` if character files are too large:

| Config key | Default | Description |
|------------|---------|-------------|
| `max_system_prompt_chars_file` | `20000` | Warn if any single file exceeds this. `0` disables. |
| `max_system_prompt_chars_total` | `80000` | Warn if total across all files exceeds this. `0` disables. |

These are set in `[sessions]` and can be overridden per-agent.

---

## Config Reference

| Key | Section | Default | Description |
|-----|---------|---------|-------------|
| `workspace` | `[[agents]]` | `{home_dir}/{id}` | Agent working directory. Character files are read relative to this. |
| `system_files` | `[defaults]` / `[[agents]]` | see below | Ordered list of workspace-relative files to load as system prompt. |
| `max_system_prompt_chars_file` | `[sessions]` / `[[agents]]` | `20000` | Per-file size warning threshold. |
| `max_system_prompt_chars_total` | `[sessions]` / `[[agents]]` | `80000` | Total size warning threshold. |

Default `system_files` when provisioned with `defaults` mode:
```toml
system_files = [
	"character/SOUL.md",
	"character/COHERENCE.md",
	"character/CRAFT.md",
	"character/USER.md",
	"character/MEMORY.md",
]
```

See [CONFIG.md](CONFIG.md) for the full configuration reference.
