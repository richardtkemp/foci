# Task: First-Run Onboarding (#14 + #150)

## Problem
When a user installs foci and sends their first message, the agent has empty template character files and no context about who they're talking to. The experience is disorienting for both the agent and user.

## Current State
- `setup.sh` creates template character files (IDENTITY.md, SOUL.md, USER.md, etc.) with placeholder prompts
- The agent starts and responds, but has no personality or user knowledge
- There's no detection of "first run" vs "existing install"

## Design

### Detection
Add a `first_run_completed` flag to the agent's state store (the key-value store in `data/state.db`). On startup:
1. Check if `first_run_completed` is set for this agent
2. If not set, inject a first-run system message before the first user message

### First-Run Message
When first_run_completed is NOT set, inject a one-time system message (similar to how WELCOME.md is injected) that tells the agent:

```
[FIRST RUN] This is your first session. Your character files contain templates that need to be filled in.

Guide your human through setup:
1. Introduce yourself — explain you're a new foci agent with blank character files
2. Ask their name and how they'd like to communicate
3. Ask what they'd like to call you (the agent)
4. Learn about them — interests, work, communication style
5. As you learn, update the character files (IDENTITY.md, SOUL.md, USER.md, MEMORY.md) in real-time
6. Confirm what you've written and ask if anything needs adjusting

Be warm but not sycophantic. This is the start of a relationship.
```

### Completion
After the agent has updated at least IDENTITY.md and USER.md (or after a configurable number of turns, e.g. 10), set `first_run_completed = true` in the state store. The onboarding message never appears again.

### Implementation

**Files to modify:**
- `main.go` — add first-run detection in the message handling path, similar to welcome file injection
- `state/store.go` — if needed, ensure string key-value operations work (they should already)

**New file:**
- `prompts/first-run.md` — the onboarding prompt (embedded, like compaction prompt)

**Config:**
- No new config needed. The feature is automatic for new installs.
- Could add `[agents.onboarding] enabled = true` but probably unnecessary — if character files have content, the prompt is harmless.

### Edge Cases
- Agent restarts during onboarding: flag not set, so onboarding continues next session
- Multiple agents: each has independent first_run_completed flag
- Existing installs: already have first_run_completed... wait, no they don't. Need migration: if character files are non-template (i.e., have been edited), auto-set the flag on first startup after deploy.

### Migration for Existing Installs
On startup, if `first_run_completed` is NOT set, check if character files differ from the templates in setup.sh. If they do, set `first_run_completed = true` silently. This prevents existing agents from getting the onboarding prompt.

A simple heuristic: if IDENTITY.md or SOUL.md doesn't contain the exact template text ("Who are you? Give yourself a name" / "What's your inner life like?"), mark as completed.

## Getting Started Guide (#150)
This is the human-facing counterpart. Add `docs/GETTING-STARTED.md` that walks through:
1. Install foci (link to INSTALL.md)
2. Configure (link to CONFIG.md, foci.toml.example)
3. Set up authentication (link to AUTH.md)
4. Send your first message — the agent will guide you through character setup
5. Customize further (skills, memory, keepalive)

## Tests
- Test first-run detection (flag not set → inject prompt)
- Test migration (non-template files → auto-complete)
- Test idempotency (flag set → no prompt)
