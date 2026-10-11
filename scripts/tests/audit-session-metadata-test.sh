#!/usr/bin/env bash
#
# audit-session-metadata-test.sh — assertions for scripts/audit-session-metadata.sh
# (#1126): findings and counts on a fixture DB, the 20-sample cap, exit codes
# (0 with findings, 2 absent table, non-zero on sqlite error with the fixed
# note and no own diagnosis), and the docs mention. Builds throwaway fixture
# DBs under /tmp; never touches a live DB.
#
# Usage: audit-session-metadata-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,10p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

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
	('main/c2', 'cc_undelivered', '$LONGVAL');"

run "$A" >"$TMP/a.out" 2>"$TMP/a.err"; rc=$?
check "exit 0 with findings present" "0" "$rc"
has "report names the DB" "$(cat "$TMP/a.out")" "db: $A"
regcount=$(cd "$ROOT" && go run ./scripts/print-meta-keys | awk 'END{print NR}')
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
"$SQL" "$B" "CREATE TABLE other (x TEXT);"
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
$ins"
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
	('main/c1', 'model', 'b');"
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

### Docs and -h.

has "docs mention the script" "$(cat "$ROOT/docs/SESSION_KEYS.md")" "audit-session-metadata.sh"
bash "$SCRIPT" -h >/dev/null 2>&1
check "-h exits 0" "0" "$?"
has "-h names the accepted residuals" "$(bash "$SCRIPT" -h 2>&1)" "residuals"

exit $RC
