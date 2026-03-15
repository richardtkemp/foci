---
name: browser
description: Browser tool — full action and parameter reference
---

# Browser Tool Reference

Control a browser using accessibility tree snapshots and element refs.

## Concept

The browser renders pages and captures an accessibility tree snapshot as YAML.
Each interactive element gets a ref like [ref=s1e5]. Use these refs to interact.

## Workflow

1. `navigate` to a URL → auto-returns snapshot
2. Read the snapshot to find element refs
3. Use `click`/`fill`/`select` with the ref to interact
4. Each action auto-returns a fresh snapshot

## Actions

| Action | Params | Notes |
|---|---|---|
| snapshot | — | Capture current page accessibility tree |
| navigate | url | Go to URL. Auto-snapshot. |
| click | ref, element | Click element. Auto-snapshot. |
| fill | ref, element, value, submit | Fill single input. Auto-snapshot. |
| fill (multi) | fields [{ref,value},...], submit | Fill multiple fields. Single snapshot scoped to form context. |
| select | ref, element, values | Select option(s). Auto-snapshot. |
| press | key | Press keyboard key (Enter, Tab, Escape, etc.) |
| go_back | — | Browser back. Auto-snapshot. |
| go_forward | — | Browser forward. Auto-snapshot. |
| reload | — | Reload page. Auto-snapshot. |
| screenshot | fullPage, returnPath | Capture screenshot |
| pdf | — | Save page as PDF |
| evaluate | script | Run JavaScript in page context |
| wait | waitType: load or idle | Wait for page state |
| close | — | Close browser |

## Parameters

- **ref** — The `[ref=...]` value from the snapshot. This is the actual element locator.
- **element** — A human-readable description (e.g., "Login button"). For logging only.
- **value** — Text to fill into an input field.
- **fields** — Array of `{ref, value}` objects for filling multiple fields at once.
- **submit** — Boolean. Press Enter after filling.
- **key** — Key name to press (Enter, Tab, Escape, etc.).
- **script** — JavaScript code to evaluate in the page context.
- **waitType** — Either `load` (page load) or `idle` (network idle).
- **fullPage** — Boolean. Capture full scrollable page instead of viewport.
- **returnPath** — Boolean. Return file path instead of base64-encoded image.
