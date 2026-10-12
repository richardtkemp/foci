#!/bin/bash
# bench-test-build.sh — measure how build flags change the time to COMPILE and
# LINK a package's test binary. It builds only (go test -c); it runs no tests.
#
# Usage (normally via make):
#   make bench-test-build [PKG=./cmd/foci-gw/] [REPS=3] [MODE=both|link|cold] \
#        [VARIANTS='default|-ldflags=-w|-ldflags=-s -w']
#
#   PKG       package whose test binary to build (default ./cmd/foci-gw/)
#   REPS      repetitions per variant; variants are interleaved per rep so a
#             change in machine load hits every variant alike (default 3)
#   MODE      link: build cache warm, so the time is mostly the link step
#             cold: an empty, private GOCACHE per build, so the time is the
#                   full compile of the package and all its dependencies + link
#             both: run both (default)
#   VARIANTS  '|'-separated build-flag sets; the word "default" means no flags.
#             Each set is split like a shell command line, so quote a flag
#             value that holds a space: VARIANTS="default|-ldflags='-s -w'"
#
# Output: one row per variant and mode: median, min and max seconds, and the
# binary size. A variant that fails to build is reported as FAILED with its log.
#
# Holds the heavy lock (/tmp/heavy) for the whole run, like make test, so other
# builds do not distort the timings and the benchmark does not slow them.
# Cold mode is slow (a full compile per build, several minutes each on this
# host); use MODE=link for a quick look.
set -u
PKG=${PKG:-./cmd/foci-gw/}
REPS=${REPS:-3}
MODE=${MODE:-both}
VARIANTS=${VARIANTS:-"default|-ldflags=-w|-ldflags='-s -w'"}

case $MODE in both) modes="link cold" ;; link|cold) modes=$MODE ;; *) echo "MODE must be both, link or cold" >&2; exit 2 ;; esac
case $REPS in ''|*[!0-9]*|0) echo "REPS must be a positive integer" >&2; exit 2 ;; esac

IFS='|' read -r -a variants <<<"$VARIANTS"
[ ${#variants[@]} -gt 0 ] || { echo "no VARIANTS" >&2; exit 2; }

work=$(mkdir -p /tmp/fgw && mktemp -d /tmp/fgw/bench-XXXXXXXX) || exit 2
trap 'rm -rf "$work"' EXIT
results=$work/results.tsv
: >"$results"

# build <variant-index> <mode>: one timed build; appends "idx mode seconds bytes|FAILED"
build() {
	local i=$1 mode=$2 v=${variants[$1]} out=$work/bin-$1 log=$work/build-$1-$2.log
	local -a f=()
	# eval: split like a shell line so a quoted value with a space stays one argument.
	[ "$v" != default ] && eval "f=($v)"
	local -a env=()
	if [ "$mode" = cold ]; then
		rm -rf "$work/cold-cache"
		env=(GOCACHE="$work/cold-cache")
	fi
	local s e
	s=$(date +%s.%N)
	if env "${env[@]}" go test -c -trimpath "${f[@]}" -o "$out" "$PKG" >"$log" 2>&1; then
		e=$(date +%s.%N)
		printf '%s\t%s\t%s\t%s\n' "$i" "$mode" "$(echo "$e - $s" | bc)" "$(stat -c %s "$out")" >>"$results"
		# Also print each timing as it lands, so a run killed before the summary
		# (a time limit, a restart) still leaves its data in the caller's log.
		printf '>>> result: %s %s %.1fs\n' "$v" "$mode" "$(echo "$e - $s" | bc)" >&2
	else
		printf '%s\t%s\tFAILED\t%s\n' "$i" "$mode" "$log" >>"$results"
		cp "$log" "/tmp/fgw/bench-failed-$i-$mode.log"
	fi
	rm -f "$out"
}

[ -e /tmp/heavy ] || : >/tmp/heavy
exec 9</tmp/heavy
echo ">>> waiting for heavy lock [$(date +%T)]" >&2
flock 9
echo ">>> acquired heavy lock [$(date +%T)]; load: $(cut -d' ' -f1-3 /proc/loadavg); $(nproc) cores" >&2

# Prime the shared build cache for every variant so link mode measures the link,
# not a first compile of a flag set the cache has never seen (-gcflags changes
# the compile cache key).
if [[ " $modes " == *" link "* ]]; then
	for i in "${!variants[@]}"; do
		echo ">>> priming: ${variants[$i]}" >&2
		build "$i" prime
	done
fi

for rep in $(seq 1 "$REPS"); do
	for mode in $modes; do
		for i in "${!variants[@]}"; do
			echo ">>> rep $rep/$REPS $mode: ${variants[$i]}" >&2
			build "$i" "$mode"
		done
	done
done

echo
echo "package $PKG, $REPS reps, $(nproc) cores, load at end: $(cut -d' ' -f1-3 /proc/loadavg)"
printf '%-30s %-5s %8s %8s %8s %9s\n' variant mode median min max size
rc=0
for mode in $modes; do
	for i in "${!variants[@]}"; do
		if awk -F'\t' -v i="$i" -v m="$mode" '$1==i && $2==m && $3=="FAILED"' "$results" | grep -q .; then
			printf '%-30s %-5s FAILED (log: /tmp/fgw/bench-failed-%s-%s.log)\n' "${variants[$i]}" "$mode" "$i" "$mode"
			rc=1
			continue
		fi
		awk -F'\t' -v i="$i" -v m="$mode" '$1==i && $2==m {print $3, $4}' "$results" | sort -n |
			awk -v name="${variants[$i]}" -v m="$mode" '
				{ t[NR]=$1; sz=$2 }
				END {
					if (NR == 0) { printf "%-30s %-5s no data\n", name, m; exit }
					med = (NR % 2) ? t[(NR+1)/2] : (t[NR/2] + t[NR/2+1]) / 2
					printf "%-30s %-5s %7.1fs %7.1fs %7.1fs %8.1fM\n", name, m, med, t[1], t[NR], sz/1048576
				}'
	done
done
exit $rc
