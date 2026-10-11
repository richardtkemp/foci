#!/usr/bin/env bash
#
# audit-session-metadata.sh — one-off read-only report on the session_metadata
# table of a foci state DB (#1126; ruling 2026-10-10: report only — no cleanup,
# no startup warning, no writes).
#
# Usage: audit-session-metadata.sh <path-to-session-db>
#   The session DB is the file holding the session_metadata table: state.db,
#   the file the gateway opens as cfg.DataPath("state.db")
#   (cmd/foci-gw/sessions_init.go) — ~/data/state.db on a default install.
#
# The registered column comes live from the ONE registry of keys
# (internal/session SessionMetadataKeys) via `go run ./scripts/print-meta-keys`
# in this checkout — the key list is never copied into this script.
#
# Sections print a column header and pipe-separated rows; text columns are
# escaped for that layout (\n, \r, \|, \\) so one row always prints on one
# line. Values sqlite3 cannot print faithfully (a NUL byte, non-text storage)
# show as <nul>/<typeof>; the biggest-values section reports byte lengths.
#
# Exit:  0 report printed (findings never change the status; the DB is opened
#          read-only, but opening a WAL-mode DB can create -wal/-shm sidecar
#          files next to it — see docs/SESSION_KEYS.md)
#        2 the DB has no session_metadata table
#        1 bad usage, go or sqlite3 missing, registry fetch failed, sqlite error
# Accepted residuals (review r3): no output-format controls, one sqlite3
# process per section, and a <db> argument starting with "file:" is treated by
# sqlite3 as a URI.
set -uo pipefail

TRUNC=80      # sample values are truncated to this many characters
SAMPLES=20    # sample rows shown per section
TOP_VALUES=10 # rows shown in the biggest-values section

case "${1:-}" in
-h|--help) sed -n '2,28p' "$0" | sed 's/^#//; s/^ //'; exit 0;;
esac

if [ $# -ne 1 ] || [ -z "$1" ]; then
	echo "usage: audit-session-metadata.sh <path-to-session-db> (-h for details)" >&2
	exit 1
fi
DB=$1
# A path starting with '-' is parsed by sqlite3 as an option; prefixing ./
# names the same file and keeps it out of the option parser.
case $DB in
-*) DB="./$DB" ;;
esac

for bin in go sqlite3; do
	command -v "$bin" >/dev/null 2>&1 || {
		echo "audit-session-metadata: '$bin' not found on PATH" >&2
		exit 1
	}
done

ROOT=$(cd "$(dirname "$0")/.." && pwd) || exit 1

# The registry keys, one per line, printed by the Go registry itself. The
# tool's (and go's) own stderr passes through untouched on failure.
KEYS=$(cd "$ROOT" && go run ./scripts/print-meta-keys) || {
	echo "audit-session-metadata: go run ./scripts/print-meta-keys failed" >&2
	exit 1
}
REG_KEYS=$(printf '%s\n' "$KEYS" | awk 'END{print NR}')
[ "$REG_KEYS" -gt 0 ] || {
	echo "audit-session-metadata: print-meta-keys printed no keys" >&2
	exit 1
}

# KEYLIST — the registry keys as a SQL literal list ('a','b',…), any embedded
# single quote doubled. One builder, used by every section that classifies keys.
KEYLIST=
while IFS= read -r k; do
	[ -n "$k" ] || continue
	esc=${k//\'/\'\'}
	KEYLIST="${KEYLIST:+$KEYLIST, }'$esc'"
done <<<"$KEYS"

# sqlite_failed — the one fixed note printed after any sqlite error. It claims
# nothing about causes, permissions or the user: sqlite's own message has
# already passed through unchanged on stderr.
sqlite_failed() {
	echo "note: the database was opened read-only (sqlite3 -readonly); opening a WAL-mode database can create -wal/-shm sidecar files next to it (see docs/SESSION_KEYS.md)" >&2
	exit 1
}

# q <sql> — run one read-only query. stdin is /dev/null so PATH wrappers that
# buffer stdin cannot hang the report (the SQL is always in argv).
q() {
	sqlite3 -readonly "$DB" "$1" </dev/null || sqlite_failed
}

# disp <expr> [limit] — SQL display expression for one text column of the
# report's pipe-separated layout: NULL -> <null>, non-text storage ->
# <typeof>, a NUL byte (sqlite3's list mode prints such values truncated)
# -> <nul>, otherwise backslash, newline, CR and the | separator escaped so
# one row always prints on exactly one line. The checks run on the raw
# expression — substr/length stop at a NUL, so truncation happens only inside
# the escape branch, with <limit> characters (values). char(92) is the
# backslash: as the SQL literal '\' it would need shell-hostile quoting
# inside the double-quoted q() strings.
disp() {
	local body=$1
	[ $# -gt 1 ] && body="substr($1,1,$2)"
	printf '%s' "CASE WHEN $1 IS NULL THEN '<null>' WHEN typeof($1) <> 'text' THEN '<' || typeof($1) || '>' WHEN instr(CAST($1 AS BLOB), x'00') > 0 THEN '<nul>' ELSE replace(replace(replace(replace($body, char(92), char(92) || char(92)), char(10), char(92) || 'n'), char(13), char(92) || 'r'), '|', char(92) || '|') END"
}

# capped <shown> <total> — the "up to N" note line when a section is capped.
capped() {
	[ "$2" -gt "$1" ] && echo "showing $1 of $2"
	return 0
}

# Absent table is the one finding that is not a report: refuse (exit 2).
have=$(q "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_metadata'") || exit $?
if [ "$have" != "1" ]; then
	echo "audit-session-metadata: no session_metadata table in $DB" >&2
	exit 2
fi

total=$(q "SELECT count(*) FROM session_metadata") || exit $?
distinct=$(q "SELECT count(*) FROM (SELECT DISTINCT key FROM session_metadata)") || exit $?
echo "db: $DB"
echo "total rows: $total  distinct keys: $distinct  registry keys: $REG_KEYS"

echo
echo "== rows per key =="
echo "key|rows|registered"
q "SELECT $(disp key), COUNT(*), CASE WHEN key IN ($KEYLIST) THEN 'yes' ELSE 'no' END
	FROM session_metadata
	GROUP BY key
	ORDER BY COUNT(*) DESC, key
	LIMIT $SAMPLES"
capped "$SAMPLES" "$distinct"

echo
echo "== unregistered keys (not in SessionMetadataKeys) =="
unreg_rows=$(q "SELECT COUNT(*) FROM session_metadata WHERE key IS NULL OR key NOT IN ($KEYLIST)") || exit $?
unreg_keys=$(q "SELECT COUNT(DISTINCT key) FROM session_metadata WHERE key IS NULL OR key NOT IN ($KEYLIST)") || exit $?
echo "rows: $unreg_rows  distinct keys: $unreg_keys"
echo "session_key|key|value"
q "SELECT $(disp session_key), $(disp key), $(disp value "$TRUNC")
	FROM session_metadata
	WHERE key IS NULL OR key NOT IN ($KEYLIST)
	ORDER BY session_key, key
	LIMIT $SAMPLES"
capped "$SAMPLES" "$unreg_rows"

echo
echo "== orphan rows (session_key not in session_index) =="
orphan_rows=$(q "SELECT COUNT(*) FROM session_metadata m
	WHERE NOT EXISTS (SELECT 1 FROM session_index s WHERE s.session_key = m.session_key)") || exit $?
echo "rows: $orphan_rows"
echo "session_key|key|value"
q "SELECT $(disp m.session_key), $(disp m.key), $(disp m.value "$TRUNC")
	FROM session_metadata m
	WHERE NOT EXISTS (SELECT 1 FROM session_index s WHERE s.session_key = m.session_key)
	ORDER BY m.session_key, m.key
	LIMIT $SAMPLES"
capped "$SAMPLES" "$orphan_rows"

echo
echo "== duplicate (session_key, key) rows =="
pk_pair=$(q "SELECT count(*) FROM pragma_table_info('session_metadata') WHERE pk > 0 AND name IN ('session_key','key')") || exit $?
pk_other=$(q "SELECT count(*) FROM pragma_table_info('session_metadata') WHERE pk > 0 AND name NOT IN ('session_key','key')") || exit $?
if [ "$pk_pair" = "2" ] && [ "$pk_other" = "0" ]; then
	echo "n/a: primary key — PRIMARY KEY (session_key, key) in the schema (internal/session/index.go) forbids the same pair twice"
else
	dup_groups=$(q "SELECT count(*) FROM (SELECT 1 FROM session_metadata GROUP BY session_key, key HAVING COUNT(*) > 1)") || exit $?
	echo "duplicate groups: $dup_groups"
	echo "session_key|key|rows"
	q "SELECT $(disp session_key), $(disp key), COUNT(*)
		FROM session_metadata
		GROUP BY session_key, key
		HAVING COUNT(*) > 1
		ORDER BY COUNT(*) DESC, session_key, key
		LIMIT $SAMPLES"
	capped "$SAMPLES" "$dup_groups"
fi

echo
echo "== biggest values (top $TOP_VALUES by length) =="
echo "session_key|key|bytes|value"
q "SELECT $(disp session_key), $(disp key), ifnull(length(CAST(value AS BLOB)), 0), $(disp value "$TRUNC")
	FROM session_metadata
	ORDER BY length(CAST(value AS BLOB)) DESC, session_key, key
	LIMIT $TOP_VALUES"

# The table existed, so the report ran: findings never change the status.
exit 0
