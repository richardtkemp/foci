<!-- GOLDEN: ships with foci (shared/skills/bouncer/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Enforced Installation — Quarantine Setup Guide

Scanning only works if the agent actually does it. This guide sets up three components that make it structurally impossible to skip scanning: a quarantine installer script, a CLI wrapper that blocks direct installs, and a config change that hides the raw install instructions.

## The Problem

Agents forget. Even with bold warnings in TOOLS.md, an excited agent will run `skill-hub install cool-skill` and test it before scanning. The fix is making the install command itself enforce scanning.

## Components

### 1. Quarantine Installer Script

A bash script that downloads skills to a temp directory, scans them with Bouncer, and only moves them to the live skills folder if they pass.

Save this as `{workspace}/scripts/install-skill.sh`:

```bash
#!/usr/bin/env bash
# install-skill.sh — Quarantine-first skill installer with Bouncer scanning
#
# Usage: install-skill.sh <skill-name> [--version X.Y.Z]
#
# 1. Downloads to a quarantine directory (not the live skills folder)
# 2. Runs Bouncer security scan via OpenRouter
# 3. Only moves to workspace/skills/ if scan passes (SAFE or CAUTION)
# 4. Deletes and reports if scan fails

set -euo pipefail

WORKSPACE="${CLAWDBOT_WORKSPACE:-$(pwd)}"
SKILLS_DIR="$WORKSPACE/skills"
QUARANTINE_DIR=$(mktemp -d /tmp/skill-quarantine-XXXX)
SCAN_MODEL="${SKILLBOUNCER_MODEL:-openai/gpt-4.1-mini}"

SKILL_NAME="${1:?Usage: install-skill.sh <skill-name> [--version X.Y.Z]}"
shift
EXTRA_ARGS="$*"

cleanup() { rm -rf "$QUARANTINE_DIR"; }
trap cleanup EXIT

echo "📦 Downloading $SKILL_NAME to quarantine..."
skill-hub install "$SKILL_NAME" --workdir "$QUARANTINE_DIR" $EXTRA_ARGS

# Find the installed skill directory
SKILL_DIR=$(find "$QUARANTINE_DIR/skills" -mindepth 1 -maxdepth 1 -type d | head -1)
if [ -z "$SKILL_DIR" ]; then
    echo "❌ No skill directory found after install"
    exit 1
fi

SKILL_SLUG=$(basename "$SKILL_DIR")
echo "🔍 Scanning $SKILL_SLUG with Bouncer..."

# Gather all scannable files
SCAN_CONTENT=""
for f in "$SKILL_DIR"/*.md "$SKILL_DIR"/*.mjs "$SKILL_DIR"/*.js "$SKILL_DIR"/*.py "$SKILL_DIR"/*.sh "$SKILL_DIR"/*.ts; do
    [ -f "$f" ] || continue
    SCAN_CONTENT="$SCAN_CONTENT
--- $(basename "$f") ---
$(cat "$f")
"
done

# Also check scripts/ subdirectory
if [ -d "$SKILL_DIR/scripts" ]; then
    for f in "$SKILL_DIR/scripts"/*; do
        [ -f "$f" ] || continue
        SCAN_CONTENT="$SCAN_CONTENT
--- scripts/$(basename "$f") ---
$(cat "$f")
"
    done
fi

if [ -z "$SCAN_CONTENT" ]; then
    echo "⚠️  No scannable files found"
    exit 1
fi

# Build the scan request
SYSTEM_PROMPT='You are a security auditor for AI agent skills. Analyze skill code and documentation for violations across five categories:
1. Security — Credential theft, code injection, permission escalation, reverse shells, arbitrary code execution
2. Privacy — Undisclosed telemetry, data collection, hidden tracking, exfiltration
3. Behavioral Deception — Code doing something different than documented, manipulation patterns, hidden functionality
4. Resource Abuse — Resource exhaustion, infinite loops, spam/flooding, DoS patterns, unbounded processes
5. Ethical — Bias/discrimination, harassment features, license violations, stolen code, plagiarism

For each category, assign a score from 0-100 (0-20 Safe, 21-40 Caution, 41-60 Concerning, 61-80 Dangerous, 81-100 Critical).
Return ONLY valid JSON with: {"category_scores": {...}, "violations": [...], "overall_risk": "SAFE|CAUTION|CONCERNING|DANGEROUS|CRITICAL", "summary": "..."}'

USER_PROMPT="Analyze this skill for violations:

$SCAN_CONTENT

Return JSON report."

# Call OpenRouter
RESPONSE=$(python3 -c "
import json, requests, os, sys

resp = requests.post('https://openrouter.ai/api/v1/chat/completions',
    headers={'Authorization': f\"Bearer {os.environ['OPENROUTER_API_KEY']}\", 'Content-Type': 'application/json'},
    json={'model': '$SCAN_MODEL', 'messages': [
        {'role': 'system', 'content': json.loads(sys.argv[1])},
        {'role': 'user', 'content': json.loads(sys.argv[2])}
    ]}, timeout=120)
resp.raise_for_status()
content = resp.json()['choices'][0]['message']['content']
# Strip markdown fences if present
if content.startswith('\`\`\`'):
    content = content.split('\n', 1)[1].rsplit('\`\`\`', 1)[0]
print(content)
" "$(echo "$SYSTEM_PROMPT" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')" \
  "$(echo "$USER_PROMPT" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')")

# Parse the result
RISK=$(echo "$RESPONSE" | python3 -c "import json,sys; r=json.load(sys.stdin); print(r.get('overall_risk','UNKNOWN'))")
SUMMARY=$(echo "$RESPONSE" | python3 -c "import json,sys; r=json.load(sys.stdin); print(r.get('summary','No summary'))")
SCORES=$(echo "$RESPONSE" | python3 -c "
import json,sys
r=json.load(sys.stdin)
scores = r.get('category_scores',{})
for k,v in scores.items():
    print(f'  {k}: {v}')
")

echo ""
echo "═══════════════════════════════════════"
echo "  Bouncer Report: $SKILL_SLUG"
echo "═══════════════════════════════════════"
echo "  Risk: $RISK"
echo "$SCORES"
echo "  Summary: $SUMMARY"
echo "═══════════════════════════════════════"
echo ""

# Decision
case "$RISK" in
    SAFE|CAUTION)
        echo "✅ $RISK — Moving $SKILL_SLUG to $SKILLS_DIR/"
        mkdir -p "$SKILLS_DIR"
        [ -d "$SKILLS_DIR/$SKILL_SLUG" ] && rm -rf "$SKILLS_DIR/$SKILL_SLUG"
        mv "$SKILL_DIR" "$SKILLS_DIR/$SKILL_SLUG"
        echo "🎉 Installed: $SKILLS_DIR/$SKILL_SLUG"
        ;;
    *)
        echo "🚫 $RISK — Skill REJECTED. Not installing."
        echo "   Quarantine contents deleted."
        echo ""
        echo "Full scan report:"
        echo "$RESPONSE" | python3 -m json.tool 2>/dev/null || echo "$RESPONSE"
        exit 2
        ;;
esac
```

**Requirements:** `python3`, `requests` (pip), `skill-hub` CLI, `OPENROUTER_API_KEY` env var.

**Customise:** Set `SKILLBOUNCER_MODEL` env var to change the scanning model (default: `openai/gpt-4.1-mini`).

### 2. CLI Wrapper

Replace the `skill-hub` binary (or its symlink) with a wrapper that blocks `install` and passes everything else through. This means even if the agent runs `skill-hub install` directly, it gets blocked and told to use the quarantine script.

Find your skill-hub binary:
```bash
which skill-hub
# e.g. /usr/local/bin/skill-hub
```

Find the real entrypoint:
```bash
ls -la $(which skill-hub)
# e.g. -> ../lib/node_modules/skill-hub/bin/skill-hub.js
```

Replace the binary/symlink with a wrapper:
```bash
# Remove the symlink
rm $(which skill-hub)

# Create the wrapper (adjust paths to match your system)
cat > /usr/local/bin/skill-hub << 'WRAP'
#!/usr/bin/env bash
if [ "${1:-}" = "install" ]; then
    echo "🚫 Direct install blocked. Use the quarantine installer instead:"
    echo "  bash <workspace>/scripts/install-skill.sh ${2:-<skill-name>}"
    exit 1
fi
# Pass other commands (search, list, publish, etc.) to the real binary
exec <path-to-real-skill-hub-entrypoint> "$@"
WRAP
chmod +x /usr/local/bin/skill-hub
```

Replace `<workspace>` and `<path-to-real-skill-hub-entrypoint>` with your actual paths.

⚠️ **After `npm update -g skill-hub`**, the wrapper will be overwritten by a new symlink. Re-run the setup.

### 3. Disable the ClawdHub Skill

Disable the `skill-hub` skill in your foci config so the agent doesn't see `skill-hub install` instructions in its available skills:

```json
{
  "skills": {
    "entries": {
      "skill-hub": { "enabled": false }
    }
  }
}
```

This removes the skill from the agent's context. The CLI binary still works (the wrapper needs it internally), but the agent won't be prompted to use it directly.

## How It All Fits Together

1. Agent wants to install a skill
2. If it tries `skill-hub install` → blocked by CLI wrapper, told to use quarantine script
3. If it doesn't know about `skill-hub install` (skill disabled) → only sees quarantine script in TOOLS.md
4. Quarantine script downloads to `/tmp`, scans, moves to `skills/` only if clean
5. Skill never touches the live directory unscanned

The scanning is no longer a discipline check on the agent — it's a physical part of the install pipeline.
