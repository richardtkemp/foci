#!/usr/bin/env bash
# Sum API costs per day from all api log files (current + archived).
# Usage: scripts/api-cost-summary.sh [log_dir]
#   log_dir defaults to /home/foci/logs

set -uo pipefail

LOG_DIR="${1:-/home/foci/logs}"
ARCHIVE_DIR="$LOG_DIR/archive"

# Stream all api log entries (archived gzipped + current) into jq
{
	# Archived api logs (not payload logs)
	if [[ -d "$ARCHIVE_DIR" ]]; then
		for f in "$ARCHIVE_DIR"/api-*.jsonl.gz; do
			[[ -f "$f" ]] || continue
			# Skip payload files
			[[ "$f" == *api-payload-* ]] && continue
			zcat "$f" 2>/dev/null || true
		done
	fi

	# Current api log
	if [[ -f "$LOG_DIR/api.jsonl" ]]; then
		cat "$LOG_DIR/api.jsonl"
	fi
} | jq -rs '
	map(select(.cost_usd != null))
	| group_by(.ts[:10])
	| map({
		date: .[0].ts[:10],
		cost: (map(.cost_usd) | add),
		calls: length
	})
	| sort_by(.date)
	| . as $days
	| ($days | map(.cost) | add) as $total
	| ($days | map(.calls) | add) as $total_calls
	| "Date         Cost       Calls",
	  "----------   --------   -----",
	  ($days[] | "\(.date)   $\(.cost | . * 100 | round / 100 | tostring | if test("\\.") then (split(".") | .[0] + "." + (.[1] + "00")[:2]) else . + ".00" end | .[: 8] | . + (" " * (8 - length)))   \(.calls)"),
	  "----------   --------   -----",
	  "TOTAL        $\($total | . * 100 | round / 100 | tostring | if test("\\.") then (split(".") | .[0] + "." + (.[1] + "00")[:2]) else . + ".00" end | .[: 8] | . + (" " * (8 - length)))   \($total_calls)"
'
