<!-- GOLDEN: ships with foci (shared/skills/bouncer/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Enforced installation — quarantine setup

Agents skip the scan, even with bold warnings. So make the install command itself scan: a quarantine installer, a wrapper that blocks direct `skill-hub install`, and the raw install instructions hidden from the agent. Written for the `skill-hub` (ClawdHub) CLI; it is not installed on every host (`which skill-hub`).

### 1. Quarantine installer

Save as `{workspace}/scripts/install-skill.sh`:

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

**Requirements:** `python3`, `requests`, the `skill-hub` CLI, and `OPENROUTER_API_KEY` in the environment.

**Differences from `SKILL.md` → "Quick scan" (check before you rely on it):**
- A foci agent shell does not export `OPENROUTER_API_KEY` (the secret resolves only inside `foci_http_request`), so export it in the environment that runs the script.
- `CLAWDBOT_WORKSPACE` is an OpenClaw variable that foci does not set; the script falls back to the current dir, so run it from the workspace.
- Its default model is `openai/gpt-4.1-mini` (`SKILLBOUNCER_MODEL` overrides it), and its inline system prompt lacks the "NOT a violation" rules in `auditing-model-prompt.md`, so it may flag benign commands that the quick scan passes.

### 2. CLI Wrapper

Replace the `skill-hub` symlink (`ls -la $(which skill-hub)` shows the real entrypoint) with a wrapper that blocks `install` and passes everything else through:
```bash
rm $(which skill-hub)
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

⚠️ **`npm update -g skill-hub` puts the symlink back.** Re-run this step after every update.

### 3. Hide the `skill-hub` skill

Keep the `skill-hub` skill out of every loaded skills dir, so the agent never sees its `skill-hub install` instructions. foci has no per-skill disable key: the skills dirs are listed in `docs/CONFIG.md` → `[skills]`. The wrapper still uses the binary.
