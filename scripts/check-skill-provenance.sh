#!/usr/bin/env bash
# check-skill-provenance.sh — every file shipped under shared/skills/ must say
# where it came from and what happens to a local edit.
#
# Usage: check-skill-provenance.sh [-h|--help] [skills-dir]
#        (default skills-dir: shared/skills, relative to the repo root)
#
# Why this exists (foci_todo #1584): the deployed copy of a skill and its repo
# source drift silently, and the two halves drift for OPPOSITE reasons:
#
#   SKILL.md      is SEED-IF-MISSING — written once if absent, then never
#                 touched again. A deployed SKILL.md can therefore sit at a
#                 version from months ago while everything around it moves.
#   every other   is GOLDEN — overwritten from the repo on every restart, so a
#   file          local edit to it is silently destroyed.
#
# Both bit us in one day: the grep skill's deployed SKILL.md had grown to 8057
# chars against a 2685-char repo source, and foci-debugging's was a 20573-byte
# pre-split monolith against a 2371-byte index (#1583) — hiding the very
# subfiles that had replaced its content. Nothing on either file said what it
# was, so nobody looked.
#
# The markers are declarative only — no foci code reads them. Their whole job
# is to tell a human (or an agent) reading the DEPLOYED copy which rule applies
# before they edit it. This script is what keeps them from being forgotten on
# the next file added.
set -uo pipefail

case "${1:-}" in
  -h|--help)
    sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
esac

SKILLS_DIR="${1:-shared/skills}"

if [ ! -d "$SKILLS_DIR" ]; then
  echo "check-skill-provenance: no such directory: $SKILLS_DIR" >&2
  exit 1
fi

fail=0
checked_skill=0
checked_golden=0

# --- SKILL.md: frontmatter must declare owner + seeded ----------------------
# Mirrors the inverse convention already used by locally-authored skills
# (`owner: clutch`, `seeded: false`), so the two are readable side by side in a
# deployed tree.
while IFS= read -r f; do
  checked_skill=$((checked_skill + 1))
  fm=$(awk '/^---$/{n++; next} n==1{print} n>1{exit}' "$f")
  if ! printf '%s\n' "$fm" | grep -q '^owner: *foci *$'; then
    echo "  $f: frontmatter missing 'owner: foci'" >&2
    fail=1
  fi
  if ! printf '%s\n' "$fm" | grep -q '^seeded: *true *$'; then
    echo "  $f: frontmatter missing 'seeded: true'" >&2
    fail=1
  fi
done < <(find "$SKILLS_DIR" -name SKILL.md | sort)

# --- everything else: GOLDEN marker naming its own skill --------------------
# The marker names the skill directory it belongs to. Checking that the name
# MATCHES the containing dir catches the copy-a-subfile-into-another-skill
# mistake, which would otherwise ship a marker pointing at the wrong repo path.
while IFS= read -r f; do
  checked_golden=$((checked_golden + 1))
  rel="${f#"$SKILLS_DIR"/}"
  skill="${rel%%/*}"
  if ! grep -q 'GOLDEN: ships with foci' "$f"; then
    echo "  $f: missing GOLDEN provenance header" >&2
    echo "      add: <!-- GOLDEN: ships with foci (shared/skills/$skill/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->" >&2
    fail=1
  elif ! grep -q "GOLDEN: ships with foci (shared/skills/$skill/)" "$f"; then
    echo "  $f: GOLDEN header names the wrong skill (expected shared/skills/$skill/)" >&2
    fail=1
  fi
done < <(find "$SKILLS_DIR" -name '*.md' ! -name SKILL.md | sort)

if [ "$fail" -ne 0 ]; then
  echo "check-skill-provenance: FAILED — see above." >&2
  echo "  SKILL.md is seed-if-missing (deployed copy is never overwritten, so it drifts)." >&2
  echo "  Every other .md is golden (deployed copy IS overwritten, so local edits are lost)." >&2
  echo "  Each must say which it is." >&2
  exit 1
fi

echo "  ok: $checked_skill SKILL.md (owner/seeded) + $checked_golden golden file(s) (header)"
