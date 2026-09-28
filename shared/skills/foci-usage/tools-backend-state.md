<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Backend tools — todos, reminders and memory search

Per-tool reference for the Claude Code (shell) backend. How `foci_*` shell functions work, and which ones you have, is in **tools-backend.md**. Every tool accepts `-h`/`--help`; read it before first use this session. The stores behind these tools are in **databases.md**.

### `foci_todo` — persistent todo list
- Subcommand-style: `add|list|list-all|search|get|complete|drop|edit|remove` (`create` is an alias for `add`).
- `add --text T [--priority high|medium|low] [--tag TAGS]`. **`--tag` REPLACES the tags parsed from the text body**, it doesn't append.
- `list [--status open|started|done|dropped|active|all] [--tag T] [--priority P] [--sort F] [--reverse] [--limit N]`. **Defaults to `--limit 10`** — use `get <id>` or `--limit 100` to see the full backlog.
- `complete|drop <id>` accept `--reason`/`--note`/`--notes` (all map to the close reason); ID forms `<id>` positional, `--id N`, or `--ids 1,2,3`.
- **No `reopen` verb.** To reopen a closed item, write the DB directly: `UPDATE todos SET status='open', completed_at=NULL, close_reason='' WHERE agent_id=? AND id=?` on `~/<agent>/.data/todo.db`.
- Chain with Unix tools to keep output small: `foci_todo list --status open | wc -l`.
- **Piped output is JSONL:** piped or `$(...)`-captured `list`/`list-all`/`search`/`get` prints one JSON object per item (`id, status, priority, tags[], title, created_at, updated_at, body` excerpt; `get` = full body; closed items add `close_reason`/`closed_at`). A capped list ends `{"truncated":true,...}`; no match = empty. `--format jsonl|md` forces either.

### `foci_remind` — defer a thought
- `--text T --when SPEC`. SPEC: duration (`2h`, `30m`), `tomorrow`, `next_keepalive`, `next_session`, a date (`YYYY-MM-DD`), or an ISO timestamp.
- `--wake` (default false): passive reminders inject as context at the time; `--wake` actively wakes the session with a message to yourself.

### `foci_memory_search` — full-text search of memory + conversation history
- Positional `query`, stemmed FTS. Memory files rank above chat history.
- `--sort relevance|newest|oldest`, `--date-from`/`--date-to YYYY-MM-DD`, `--lines N` (context window).
- Direct lookup: `--query "session#rowID"` (e.g. `agent/c123#42`) pulls surrounding messages.
- This reaches **conversation history that grep can't** — prefer it over grepping the memory dir.
