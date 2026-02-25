# Task: Auto-background http_request (like exec)

## Problem

When an agent makes an `http_request` that takes a long time (e.g. image generation, 10-30s), the agent blocks — it can't do anything else while waiting. `exec` already has auto-backgrounding after a configurable delay, and an explicit `background` parameter. `http_request` should work the same way.

## Required Changes

### 1. Auto-background after threshold

If an `http_request` takes longer than a configurable threshold (shared with exec — whatever config value defines exec's auto-background delay, currently ~10s), it should automatically background. The agent gets a message like "Request still running, will deliver result when complete" and can continue working. When the request finishes, the result is delivered back to the agent.

### 2. Explicit `background` parameter

Add an optional `background` boolean parameter to `http_request` (same as exec has). When set to `true`, the request backgrounds immediately without waiting for the threshold.

### 3. Shared config value

The auto-background threshold should be a single shared config value used by both `exec` and `http_request`. Find where exec's threshold is defined and reuse it. If it's currently hardcoded, extract it to config.

## Behaviour

- `background: false` (default) — wait up to threshold, then auto-background
- `background: true` — background immediately, return control to agent
- When backgrounded, result is delivered asynchronously (same mechanism exec uses)

## Files to check

- Look at how exec implements auto-backgrounding and result delivery
- Apply the same pattern to http_request
- Extract shared threshold to config if not already there
- Update SPEC.md and docs/CONFIG.md

## Test & commit

Write/update tests, commit with descriptive message, push.
