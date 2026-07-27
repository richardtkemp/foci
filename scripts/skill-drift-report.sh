#!/usr/bin/env bash
# skill-drift-report.sh — report how a DEPLOYED skill tree differs from the
# foci repo it was seeded from.
#
# Usage: skill-drift-report.sh [-h|--help] [deployed-skills-dir] [repo-skills-dir]
#        defaults: ~/shared/skills  and  <this repo>/shared/skills
#
# This REPORTS; it never fails. Divergence is not automatically wrong:
# SKILL.md is seed-if-missing precisely so an install can customise it, and a
# golden file differing usually just means the deployed copy is behind a landed
# change and the next restart will re-seed it. Nothing on disk can distinguish
# "deployed is stale" from "someone edited it locally and is about to lose the
# edit", so the judgement stays with the reader. The point is to make the drift
# VISIBLE rather than silent.
#
# Why this exists, separately from check-skill-provenance.sh (foci_todo #1584):
# that script asks "is every file in the REPO labelled?" — source hygiene at
# commit time. It cannot see a deployed tree, and pointing it at one produces
# nonsense. But the failure that actually bites is drift between the two:
#
#   grep             deployed 8057 chars vs 2685 in the repo
#   foci-debugging   deployed 20573 bytes of PRE-SPLIT monolith vs a 2371-byte
#                    index — hiding the very subfiles that had replaced it
#   foci-development 29 lines of deploy/rollback knowledge that existed ONLY
#                    in the deployed copy, so no other agent had it
#
# All three drifted for weeks unnoticed, and were found one at a time by hand.
# This script finds them in one pass.
set -uo pipefail

case "${1:-}" in
  -h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
esac

DEPLOYED="${1:-$HOME/shared/skills}"
REPO="${2:-$(cd "$(dirname "$0")/.." && pwd)/shared/skills}"

for d in "$DEPLOYED" "$REPO"; do
  [ -d "$d" ] || { echo "skill-drift-report: no such directory: $d" >&2; exit 1; }
done

echo "deployed: $DEPLOYED"
echo "repo:     $REPO"
echo

broken=0 drifted=0 clean=0 local_only=0

for repo_skill in "$REPO"/*/; do
  name=$(basename "$repo_skill")
  dep="$DEPLOYED/$name"
  if [ ! -d "$dep" ]; then
    echo "MISSING   $name — shipped by foci but not deployed (will seed on next restart)"
    continue
  fi

  # Golden files: overwritten on restart, so a local edit is doomed. This is
  # the only condition worth failing on.
  golden_diff=""
  while IFS= read -r rf; do
    rel="${rf#"$repo_skill"}"
    df="$dep/$rel"
    [ -f "$df" ] || { golden_diff+=" $rel(absent)"; continue; }
    cmp -s "$rf" "$df" || golden_diff+=" $rel"
  done < <(find "$repo_skill" -name '*.md' ! -name SKILL.md)

  skill_diff=""
  if [ -f "$dep/SKILL.md" ] && ! cmp -s "$repo_skill/SKILL.md" "$dep/SKILL.md"; then
    rl=$(wc -l < "$repo_skill/SKILL.md"); dl=$(wc -l < "$dep/SKILL.md")
    only_dep=$(diff "$dep/SKILL.md" "$repo_skill/SKILL.md" | grep -c '^<' || true)
    skill_diff="SKILL.md repo=${rl}L deployed=${dl}L, ${only_dep} line(s) only in deployed"
  fi

  # A deployed-only sibling is how content gets stranded on one install
  # (app-delivery.md, enforced-installation.md both hid this way).
  stranded=""
  while IFS= read -r df; do
    rel="${df#"$dep/"}"
    [ -f "$repo_skill/$rel" ] || stranded+=" $rel"
  done < <(find "$dep" -name '*.md' ! -name SKILL.md)

  if [ -n "$golden_diff" ]; then
    echo "GOLDEN    $name — golden file(s) differ; the next restart REPLACES the deployed copy:$golden_diff"
    echo "          (benign if deployed is merely behind; a local edit to these is about to be lost)"
    broken=$((broken+1))
  fi
  [ -n "$stranded" ] && { echo "STRANDED  $name — deployed-only file(s), no other agent has these:$stranded"; drifted=$((drifted+1)); }
  [ -n "$skill_diff" ] && { echo "DRIFT     $name — $skill_diff"; drifted=$((drifted+1)); }
  [ -z "$golden_diff$stranded$skill_diff" ] && clean=$((clean+1))
done

for dep_skill in "$DEPLOYED"/*/; do
  name=$(basename "$dep_skill")
  [ -d "$REPO/$name" ] || local_only=$((local_only+1))
done

echo
echo "$clean identical, $drifted drifted, $local_only locally-authored (not foci-shipped, nothing to compare)"
[ "$broken" -ne 0 ] && echo "$broken skill(s) with golden-file divergence — check whether the deployed side holds an edit worth moving into the repo."
exit 0
