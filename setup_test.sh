#!/bin/bash
# Test that setup.sh is syntactically valid and --dry-run is idempotent.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SETUP="$SCRIPT_DIR/setup.sh"

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

pass() { echo -e "${GREEN}PASS${NC} $1"; }
fail() { echo -e "${RED}FAIL${NC} $1"; exit 1; }

# Test 1: Syntax check
echo "--- Test 1: Bash syntax check ---"
if bash -n "$SETUP"; then
    pass "setup.sh parses without syntax errors"
else
    fail "setup.sh has syntax errors"
fi

# Test 2: --help works
echo "--- Test 2: --help flag ---"
output=$(bash "$SETUP" --help 2>&1) || true
if echo "$output" | grep -q "Idempotent"; then
    pass "--help shows usage"
else
    fail "--help output unexpected: $output"
fi

# Test 3: --dry-run completes without root
echo "--- Test 3: --dry-run first run ---"
output=$(bash "$SETUP" --dry-run 2>&1)
if echo "$output" | grep -q "Done"; then
    pass "--dry-run completes successfully"
else
    fail "--dry-run failed: $output"
fi

# Test 4: --dry-run is idempotent (running twice produces same output)
echo "--- Test 4: Idempotency (--dry-run twice) ---"
output1=$(bash "$SETUP" --dry-run 2>&1)
output2=$(bash "$SETUP" --dry-run 2>&1)
if [[ "$output1" == "$output2" ]]; then
    pass "--dry-run output is identical on second run"
else
    fail "Output differs between runs"
fi

# Test 5: Unknown flag is rejected
echo "--- Test 5: Unknown flag rejection ---"
if bash "$SETUP" --bogus 2>/dev/null; then
    fail "Should reject unknown flags"
else
    pass "Unknown flag rejected"
fi

# Test 6: Script has --dry-run guard on destructive operations
echo "--- Test 6: Dry-run guards exist ---"
checks=0
for pattern in "run useradd" "run mkdir" "run chown" "run systemctl"; do
    if grep -q "$pattern" "$SETUP"; then
        checks=$((checks + 1))
    fi
done
if [[ $checks -ge 4 ]]; then
    pass "Destructive operations wrapped in run() ($checks guards found)"
else
    fail "Missing dry-run guards ($checks found, expected >= 4)"
fi

# Test 7: Config won't be overwritten (idempotent check exists)
echo "--- Test 7: Config idempotency guard ---"
if grep -q 'if \[\[ -f.*clod.toml' "$SETUP"; then
    pass "Config file existence check present"
else
    fail "Missing config idempotency guard"
fi

# Test 8: Character files use write_if_missing
echo "--- Test 8: Character file idempotency ---"
count=$(grep -c 'write_if_missing' "$SETUP")
if [[ $count -ge 7 ]]; then
    pass "All 7 character files use write_if_missing ($count calls)"
else
    fail "Expected >= 7 write_if_missing calls, got $count"
fi

# Test 9: Secrets file has restricted permissions
echo "--- Test 9: Secrets permissions ---"
if grep -q 'chmod 600.*secrets.toml' "$SETUP"; then
    pass "secrets.toml gets chmod 600"
else
    fail "Missing chmod 600 on secrets.toml"
fi

# Test 10: Service file existence check
echo "--- Test 10: Service idempotency guard ---"
if grep -q 'if \[\[ -f.*SERVICE_FILE' "$SETUP" || grep -q 'if.*-f.*SERVICE_FILE' "$SETUP"; then
    pass "Service file existence check present"
else
    fail "Missing service idempotency guard"
fi

echo ""
echo "All tests passed."
