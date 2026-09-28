<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Backend tools — HTTP, web, extraction, browser and Android

Per-tool reference for the Claude Code (shell) backend. How `foci_*` shell functions work, and which ones you have, is in **tools-backend.md** — `foci_browser` and `foci_app_android` are conditional. Every tool accepts `-h`/`--help`; read it before first use this session.

### `foci_http_request` — HTTP with server-side secret resolution
- Positional `url`. `--method`, `--header 'K: V'` (repeatable) or `--headers <json>`, `--body`/`--body-file`, `--query <json>`.
- Secrets: `{{secret:NAME}}` in headers is resolved server-side against `allowed_hosts`; in body/form fields it requires `allowed_in_body` in secrets.toml.
- `--save-to <path>` writes the body to disk (prints `Saved N bytes to <path>`); `--save-from-json-path data.0.url` extracts a field first (and decodes `data:` URIs). `--background` runs async.
- Output is the body only by default. `--include-headers` prepends the `HTTP <status>` line and every response header (e.g. to read Content-Length, Retry-After or Location) — listed in `--help` like every other flag.
- Filter responses with jq so only what you need hits context: `foci_http_request URL | jq '.[].name'`.

### `foci_web_fetch` — URL → clean Markdown
- Positional `url`. Readability extraction → Markdown; `--raw` returns HTML. SSRF-safe. Not for downloading files (use `foci_http_request`); large pages truncated.

### `foci_web_search` — Brave web search
- Positional `query`. Returns titles/URLs/descriptions. Requires a Brave API key configured.

### `foci_summary` — extract from a file via a cheap model
- `foci_summary "what does this define?" --file path` (or pipe: `… | foci_summary "categorise these"`). For targeted extraction, **not** dumping a whole file. Great for piping noisy data through a cheap model before it hits your context.

### `foci_browser` — drive a real headless browser
- Positional `action`, e.g. `foci_browser navigate --url https://example.com`, then `foci_browser click --ref s1e5`. The `browser` skill has the full action and parameter reference.
- **Your session has its own browser.** Forks, branches and other chats each get a separate one, so page state never leaks between them. A browser left idle for 30 minutes is stopped; the next call starts a fresh one.
- Booleans are presence-only flags, so `--incognito` can only turn incognito ON. To start on the persistent profile pass JSON: `foci_browser '{"action":"start","incognito":false}'`.
- Use `--returnPath` on `screenshot`, or the base64 image lands in your output.

### `foci_app_android` — run a task on the user's connected Android device (via Tasker)
- Only registered when the `app` platform is configured for this agent — presence of the tool doesn't guarantee a device is actually connected right now (see below).
- `--action list` returns the device's allowlisted tasks as JSON. `--action perform --task NAME [--par1 V] [--par2 V]` runs a named task (`par1`/`par2` map to Tasker's `%par1`/`%par2`; stringly-typed — JSON-stringify structured args into `par1`).
- **The on-device allowlist is empty by default** — the user has to opt tasks in via the app's Advanced settings before `perform` can reach them.
- No device connected → a plain error string in the result (not a tool failure), so react to it rather than treating the call as having crashed.
- A task can reply `"pending"` if it's still running on-device past the sync window; the server keeps waiting up to ~60s for the real result, so most slow tasks still resolve synchronously. Only a task that *also* blows that budget comes back pending-with-no-result — that result is dropped, there's no later async delivery.
