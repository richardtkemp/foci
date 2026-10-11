#!/usr/bin/env bash
#
# audit-session-metadata-test.sh — assertions for scripts/audit-session-metadata.sh
# (#1126): findings and counts on a fixture DB, the 20-sample cap, exit codes
# (0 with findings, 2 absent table, non-zero on sqlite error with the fixed
# note and no own diagnosis), hostile values (newline, pipe, backslash, NUL)
# keeping one row per line, a dash-prefixed DB path, the registry bridge via
# META_KEYS_BRIDGE (empty output refused; a stub bridge's keys classified),
# the -h residuals wording, and the docs mention.
# Builds throwaway fixture DBs under /tmp; never touches a live DB.
#
# Usage: audit-session-metadata-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,13p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
SCRIPT="$ROOT/scripts/audit-session-metadata.sh"
# Fixture DDL needs a sqlite3 that accepts writes: a PATH sqlite3 may be a
# readonly wrapper, so prefer the real binary when it exists.
SQL=/usr/bin/sqlite3
[ -x "$SQL" ] || SQL=$(command -v sqlite3) || { echo "no sqlite3 for fixtures" >&2; exit 1; }

TMP=$(mktemp -d /tmp/audit-session-metadata-XXXXXX)
trap 'chmod -R u+rwx "$TMP" 2>/dev/null; rm -rf "$TMP"' EXIT
RC=0

check() { # check <label> <expected> <actual>
	if [ "$2" = "$3" ]; then printf 'ok   %s\n' "$1"
	else printf 'FAIL %s\n       want: %s\n       got:  %s\n' "$1" "$2" "$3"; RC=1; fi
}

# has <label> <haystack> <needle> — substring assertion on a string.
has() {
	case "$2" in
	*"$3"*) printf 'ok   %s\n' "$1";;
	*) printf 'FAIL %s\n       missing: %s\n' "$1" "$3"; RC=1;;
	esac
}

# section <file> <header-needle> — the text of one == header == section.
section() {
	awk -v h="$2" 'index($0, h) { f = 1; next } /^== / { f = 0 } f' "$1"
}

# run <db> — the script under test; stdin closed (its SQL is always in argv).
run() { bash "$SCRIPT" "$1" </dev/null; }

# The real schema (internal/session/index.go) — the contract the script reads.
DDL_INDEX="CREATE TABLE session_index (
	session_key        TEXT PRIMARY KEY,
	file_path          TEXT NOT NULL,
	created_at         TEXT NOT NULL,
	last_activity_at   TEXT,
	last_reflection    TEXT,
	last_cache_touch   TEXT,
	last_user_activity_at TEXT,
	parent_session_key TEXT,
	session_type       TEXT NOT NULL,
	status             TEXT NOT NULL DEFAULT 'active',
	agent_id           TEXT NOT NULL DEFAULT '',
	chat_id            INTEGER NOT NULL DEFAULT 0,
	is_root            INTEGER NOT NULL DEFAULT 0
);"
DDL_META="CREATE TABLE session_metadata (
	session_key TEXT NOT NULL,
	key         TEXT NOT NULL,
	value       TEXT,
	PRIMARY KEY (session_key, key)
);"
idx_row() { # idx_row <session_key>
	printf "INSERT INTO session_index (session_key, file_path, created_at, session_type) VALUES ('%s', 'f', '2026-01-01', 'chat');" "$1"
}

### Fixture A: registered keys, one unregistered key, one orphan row.

A="$TMP/a.db"
LONGVAL=$(printf 'v%.0s' $(seq 1 120))
"$SQL" "$A" "PRAGMA journal_mode=wal;
$DDL_INDEX
$DDL_META
$(idx_row main/c1)
$(idx_row main/c2)
INSERT INTO session_metadata (session_key, key, value) VALUES
	('main/c1', 'model', 'claude-x'),
	('main/c2', 'model', 'claude-y'),
	('main/c1', 'no_compact', 'true'),
	('main/c1', 'bogus_legacy', 'legacy value'),
	('main/ghost', 'model', 'ghost-val'),
	('main/c2', 'cc_undelivered', '$LONGVAL');" >/dev/null

run "$A" >"$TMP/a.out" 2>"$TMP/a.err"; rc=$?
check "exit 0 with findings present" "0" "$rc"
has "report names the DB" "$(cat "$TMP/a.out")" "db: $A"
has "sections label their columns" "$(cat "$TMP/a.out")" "session_key|key|value"
regcount=$(cd "$ROOT" && go run ./scripts/print-meta-keys | awk 'NF {n++} END{print n+0}')
has "header counts (registry wired live)" "$(cat "$TMP/a.out")" "total rows: 6  distinct keys: 4  registry keys: $regcount"
has "rows per key: model|3|yes (ghost row included)" "$(section "$TMP/a.out" "== rows per key")" "model|3|yes"
has "rows per key: bogus_legacy|1|no" "$(section "$TMP/a.out" "== rows per key")" "bogus_legacy|1|no"
unreg=$(section "$TMP/a.out" "== unregistered keys")
has "unregistered rows and distinct counts" "$unreg" "rows: 1  distinct keys: 1"
has "unregistered sample names the key (fail-arm)" "$unreg" "main/c1|bogus_legacy|legacy value"
orph=$(section "$TMP/a.out" "== orphan rows")
has "orphan rows counted" "$orph" "rows: 1"
has "orphan sample names the row" "$orph" "main/ghost|model|ghost-val"
has "duplicates n/a via primary key" "$(section "$TMP/a.out" "== duplicate")" "n/a: primary key"
has "biggest value tops list, truncated to 80" "$(section "$TMP/a.out" "== biggest values")" "main/c2|cc_undelivered|120|${LONGVAL:0:80}"

### Fixture B: no session_metadata table -> exit 2.

B="$TMP/b.db"
"$SQL" "$B" "CREATE TABLE other (x TEXT);" >/dev/null
run "$B" >"$TMP/b.out" 2>"$TMP/b.err"; rc=$?
check "absent table exits 2" "2" "$rc"
has "absent table message names the table" "$(cat "$TMP/b.err")" "session_metadata"

### Fixture C: 25 unregistered keys -> samples capped at 20 in both sections.

C="$TMP/c.db"
ins=
for i in $(seq 1 25); do
	ins="$ins INSERT INTO session_metadata (session_key, key, value) VALUES ('main/c1', 'k$i', 'v$i');"
done
"$SQL" "$C" "PRAGMA journal_mode=wal;
$DDL_INDEX
$DDL_META
$(idx_row main/c1)
$ins" >/dev/null
run "$C" >"$TMP/c.out" 2>"$TMP/c.err"; rc=$?
check "capped fixture exits 0" "0" "$rc"
unreg=$(section "$TMP/c.out" "== unregistered keys")
has "25 unregistered rows counted" "$unreg" "rows: 25  distinct keys: 25"
check "unregistered samples capped at exactly 20 lines" "20" \
	"$(printf '%s\n' "$unreg" | awk -F'|' '$1 == "main/c1" && $2 ~ /^k[0-9]+$/ {n++} END{print n+0}')"
has "unregistered capped note" "$unreg" "showing 20 of 25"
has "rows-per-key capped note" "$(section "$TMP/c.out" "== rows per key")" "showing 20 of 25"

### Fixture D: table without the primary key -> duplicates listed, not n/a.

D="$TMP/d.db"
"$SQL" "$D" "PRAGMA journal_mode=wal;
$DDL_INDEX
CREATE TABLE session_metadata (
	session_key TEXT NOT NULL,
	key         TEXT NOT NULL,
	value       TEXT
);
$(idx_row main/c1)
INSERT INTO session_metadata (session_key, key, value) VALUES
	('main/c1', 'model', 'a'),
	('main/c1', 'model', 'b');" >/dev/null
run "$D" >"$TMP/d.out" 2>"$TMP/d.err"; rc=$?
check "permissive-schema fixture exits 0" "0" "$rc"
dups=$(section "$TMP/d.out" "== duplicate")
has "duplicate groups counted" "$dups" "duplicate groups: 1"
has "duplicate pair sampled" "$dups" "main/c1|model|2"

### Fixture E: sqlite error (unwritable dir blocks the WAL -shm) -> sqlite's
### own message, the fixed note, non-zero exit, no diagnosis of our own.

if [ "$(id -u)" = "0" ]; then
	echo "skip  sqlite-error fixture: running as root, chmod 500 would not bite"
else
	EDIR="$TMP/edir"
	mkdir "$EDIR"
	cp "$A" "$EDIR/e.db"
	chmod 500 "$EDIR"
	run "$EDIR/e.db" >"$TMP/e.out" 2>"$TMP/e.err"; rc=$?
	chmod 700 "$EDIR"
	case "$rc" in
	0) echo "FAIL sqlite error exits non-zero"; RC=1;;
	*) echo "ok   sqlite error exits non-zero (rc=$rc)";;
	esac
	NOTE='note: the database was opened read-only (sqlite3 -readonly); opening a WAL-mode database can create -wal/-shm sidecar files next to it (see docs/SESSION_KEYS.md)'
	err=$(cat "$TMP/e.err")
	has "note line printed exactly" "$err" "$NOTE"
	check "sqlite's own message passed through" "yes" \
		"$(grep -Fvx "$NOTE" "$TMP/e.err" | grep -c . | awk '{print ($1 >= 1) ? "yes" : "no"}')"
	case "$err" in
	*"this user"*|*"cannot write"*)
		echo "FAIL own-diagnosis strings present in: $err"; RC=1;;
	*) echo "ok   no own-diagnosis strings";;
	esac
	check "no report printed on error" "0" "$(wc -l <"$TMP/e.out" | tr -d ' ')"
	run "$EDIR/e.db" >"$TMP/e2.out" 2>"$TMP/e2.err"; rc=$?
	check "same DB reports fine once writable (positive control)" "0" "$rc"
fi

### Fixture F: hostile values — a newline, CR, pipes, a backslash and NUL
### bytes in session_key, key and value must not break the one-row-per-line
### layout (review r4 H3/c2).

F="$TMP/f.db"
"$SQL" "$F" "PRAGMA journal_mode=wal;
$DDL_INDEX
$DDL_META
$(idx_row main/c1)
$(idx_row main/c2)
INSERT INTO session_metadata (session_key, key, value) VALUES
	('main/c1', 'model', 'line1' || char(10) || 'line2' || char(13) || 'end'),
	('main/c1', 'no_compact', 'a|b\c'),
	('main/c|1', 'we|ird', 'v'),
	('main/c1', 'cc_undelivered', CAST(X'00410a42' AS BLOB)),
	('main/c2', 'cc_resume_id', CAST(X'00410a42' AS TEXT));" >/dev/null
run "$F" >"$TMP/f.out" 2>"$TMP/f.err"; rc=$?
check "hostile-value fixture exits 0" "0" "$rc"
big=$(section "$TMP/f.out" "== biggest values")
has "newline and CR in value escaped onto one line" "$big" 'main/c1|model|15|line1\nline2\rend'
has "pipe and backslash in value escaped" "$big" 'main/c1|no_compact|5|a\|b\\c'
has "NUL blob shown as placeholder, byte length kept" "$big" 'main/c1|cc_undelivered|4|<blob>'
has "NUL inside text shown as placeholder, byte length kept" "$big" 'main/c2|cc_resume_id|4|<nul>'
has "pipe in session_key and key escaped" "$big" 'main/c\|1|we\|ird|1|v'
check "biggest values stays one line per row" "5" "$(printf '%s\n' "$big" | grep -c '^main')"
has "rows per key: pipe key escaped" "$(section "$TMP/f.out" "== rows per key")" 'we\|ird|1|no'
unreg=$(section "$TMP/f.out" "== unregistered keys")
has "hostile fixture unregistered counts" "$unreg" "rows: 1  distinct keys: 1"
has "unregistered sample with pipes stays one line" "$unreg" 'main/c\|1|we\|ird|v'
orph=$(section "$TMP/f.out" "== orphan rows")
has "hostile fixture orphan counts" "$orph" "rows: 1"
has "orphan sample with pipes stays one line" "$orph" 'main/c\|1|we\|ird|v'

### A relative DB path starting with '-' must be opened, not parsed by
### sqlite3 as an option (review r4 c6).

cp "$A" "$TMP/-x.db"
(cd "$TMP" && run -x.db) >"$TMP/dash.out" 2>"$TMP/dash.err"; rc=$?
check "dash-prefixed relative path exits 0" "0" "$rc"
has "dash-prefixed path opened and reported" "$(cat "$TMP/dash.out")" "db: ./-x.db"

### The registry bridge, heard through the META_KEYS_BRIDGE seam. Empty
### bridge output must refuse the report (an empty key list would mark every
### key unregistered and still exit 0 — review r5 F1); a stub bridge's keys
### are the registered ones.

( META_KEYS_BRIDGE=true run "$A" ) >"$TMP/empty.out" 2>"$TMP/empty.err"; rc=$?
check "empty bridge output refuses the report" "1" "$rc"
has "empty bridge message names the bridge" "$(cat "$TMP/empty.err")" "printed no keys"
check "no report printed on empty bridge output" "0" "$(wc -l <"$TMP/empty.out" | tr -d ' ')"

( META_KEYS_BRIDGE='printf model\n' run "$A" ) >"$TMP/stub.out" 2>"$TMP/stub.err"; rc=$?
check "stub-bridge report exits 0" "0" "$rc"
has "stub bridge key count in header" "$(cat "$TMP/stub.out")" "registry keys: 1"
has "stub bridge key classified registered" "$(section "$TMP/stub.out" "== rows per key")" "model|3|yes"
has "key outside stub bridge classified unregistered" "$(section "$TMP/stub.out" "== rows per key")" "no_compact|1|no"

### Docs and -h.

has "docs mention the script" "$(cat "$ROOT/docs/SESSION_KEYS.md")" "audit-session-metadata.sh"
help=$(bash "$SCRIPT" -h 2>&1); rc=$?
check "-h exits 0" "0" "$rc"
has "-h names the accepted residuals" "$help" "residuals"
has "-h: control bytes in values print raw" "$help" "print raw"
has "-h: one sqlite3 process per section" "$help" "one sqlite3"
has "-h: file: path treated as URI" "$help" "file:"

exit $RC
