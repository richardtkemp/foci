<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Backend tools — HTTP, web, extraction, browser and Android

Per-tool reference for the Claude Code (shell) backend. How `foci_*` shell functions work, and which ones you have, is in **tools-backend.md** — `foci_browser` and `foci_client` are conditional. Every tool accepts `-h`/`--help`; read it before first use this session.

### `foci_http_request` — HTTP with server-side secret resolution
- Positional `url`. `--method`, `--header 'K: V'` (repeatable) or `--headers <json>`, `--body`/`--body-file`, `--query <json>`.
- Secrets: `{{secret:NAME}}` in headers is resolved server-side against `allowed_hosts`; in body/form fields it requires `allowed_in_body` in secrets.toml.
- HTTP Basic auth: `--basic-auth 'user:password'` resolves templates, then base64-encodes, e.g. `--basic-auth '{{secret:NAME}}:'` for an API key as username. Don't hand-build `Authorization: Basic {{secret:…}}`: the secret would go unencoded.
- `--save-to <path>` writes the body to disk (prints `Saved N bytes to <path>`); `--save-from-json-path data.0.url` extracts a field first (and decodes `data:` URIs). `--background` runs async.
- Output is the body only by default. `--include-headers` prepends the `HTTP <status>` line and every response header (e.g. to read Content-Length, Retry-After or Location) — listed in `--help` like every other flag.

### `foci_web_fetch` — URL → clean Markdown
- Positional `url`. Readability extraction → Markdown; `--raw` returns HTML. SSRF-safe. Not for downloading files (use `foci_http_request`); large pages truncated.

### `foci_web_search` — Brave web search
- Positional `query`. Returns titles/URLs/descriptions. Requires a Brave API key configured.

### `foci_summary` — extract from a file via a cheap model
- `foci_summary "what does this define?" --file path` (or pipe: `… | foci_summary "categorise these"`). For targeted extraction, **not** dumping a whole file.

### `foci_browser` — drive a real headless browser
- Positional `action`, e.g. `foci_browser navigate --url https://example.com`, then `foci_browser click --ref s1e5`. The `browser` skill has the full action and parameter reference.
- **Your session has its own browser.** Forks, branches and other chats each get a separate one, so page state never leaks between them. A browser left idle for 30 minutes is stopped; the next call starts a fresh one.
- Booleans are presence-only flags, so `--incognito` can only turn incognito ON. To start on the persistent profile pass JSON: `foci_browser '{"action":"start","incognito":false}'`.
- Use `--returnPath` on `screenshot`, or the base64 image lands in your output.

### `foci_client` — act on the user's Android phone (alarms, timers, calls, contacts, clipboard, links, location, ring, notifications, health, Tasker)
- Only registered when the `app` platform is configured for this agent — presence of the tool doesn't guarantee a phone is actually connected right now.
- Shape: `foci_client <action> [words...] [flags...]`. Words after the action fill its arguments in order; quote a multi-word label. `foci_client --help` lists every action, e.g. `foci_client set_alarm 07:30 "Take T to nursery" --days mon,wed`, `foci_client set_timer 2m tea`, `foci_client open_url https://example.com`, `foci_client tasker_perform NAME [par1] [par2]`.
- `set_alarm` takes a clock time and weekdays only (no date) and sets it silently in the Clock app.
- `read_notifications` returns what is in the shade now, not history. `health` with no type lists every data type and whether it is granted; `foci_client health steps --from 2026-11-01 --to 2026-11-03` reads one type, newest first (default the last 7 days, 100 records). Check `truncated` before you count records. For a sum use `total` or `total_<unit>`, from which Health Connect removes duplicates; adding up records from a phone and a watch counts steps twice. Those fields are absent when Health Connect gives no total.
- The user switches each action on in the app and grants its permission there (Settings > Advanced > Phone actions). A switched-off or ungranted action comes back as an error naming what is missing; nothing prompts the user. `call`, `contact_lookup`, `location`, `read_notifications` and `health` start switched off.
- The Tasker allowlist is empty by default — the user opts tasks in via the app's Advanced settings before `tasker_perform` can reach them.
- No phone, no capable phone (desktop only), a phone app needing an update, and no answer in time each give a plain error string in the result (not a tool failure).
- A Tasker task can reply `"pending"` if it's still running on-device past the sync window; the server keeps waiting up to ~60s for the real result. Only a task that *also* blows that budget comes back pending-with-no-result — that result is dropped.
