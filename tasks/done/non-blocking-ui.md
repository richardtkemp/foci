# Task: Non-blocking UI Actions During Agent Turns

## Problem

When the agent is mid-turn (processing tool calls, generating response), non-chat UI actions are blocked:
1. **Tool call expansion** (inline keyboard toggle) — doesn't respond until the turn completes
2. **Multiball** (`/multiball` command) — doesn't respond until the turn completes

Both should work immediately regardless of whether the agent is busy.

## Expected Behaviour

- **Tool call expansion:** Callback queries for toggling tool call results should be handled immediately by the Telegram bot layer, independent of the agent loop. These don't need agent involvement — they're just editing a message with stored data.
- **Multiball:** Should branch from the conversation state *before* the current in-progress turn, not wait for it to complete. The user wants a parallel session now, not after the current work finishes.

## Investigation

1. Find where callback queries (inline keyboard) are processed — are they going through the same queue/lock as agent turns?
2. Find where `/multiball` is processed — same question.
3. Both should be handled on a separate path that doesn't block on the agent turn mutex/queue.

## Fix

Ensure these paths are non-blocking:
- Callback queries (tool call expand/collapse, thinking toggle) handled independently of agent turn processing
- `/multiball` forks from current conversation state immediately without waiting for in-progress turn

Update SPEC.md if behaviour is clarified.
