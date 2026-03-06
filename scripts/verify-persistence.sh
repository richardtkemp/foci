#!/bin/bash
# Formal verification that no struct with map state lacks persistence infrastructure
# This proves structural soundness of the persistence design.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DB_DIR="$PROJECT_ROOT/codeql-db"
QUERY_FILE="$SCRIPT_DIR/verify-persistence.ql"
RESULTS_FILE="$PROJECT_ROOT/.codeql-results.bqrs"

echo "=== CodeQL Persistence Verification ==="
echo ""

# Check if database exists
if [ ! -d "$DB_DIR" ]; then
    echo "📦 Creating CodeQL database (first run, ~30s)..."
    cd "$PROJECT_ROOT"
    gh codeql database create "$DB_DIR" --language=go
    echo "✅ Database created"
    echo ""
fi

# Install dependencies if needed
if [ ! -f "$SCRIPT_DIR/codeql-pack.lock.yml" ]; then
    echo "📥 Installing CodeQL dependencies..."
    cd "$SCRIPT_DIR"
    gh codeql pack install
    echo ""
fi

# Run verification query
echo "🔍 Running persistence verification query..."
cd "$SCRIPT_DIR"
if gh codeql query run verify-persistence.ql --database="$DB_DIR" --output="$RESULTS_FILE" 2>&1 | grep -q "Evaluation done"; then
    # Decode results
    RESULT_COUNT=$(gh codeql bqrs decode "$RESULTS_FILE" --format=csv 2>/dev/null | tail -n +2 | wc -l)

    if [ "$RESULT_COUNT" -eq 0 ]; then
        echo "✅ VERIFICATION PASSED: No persistence gaps found"
        echo ""
        echo "This formally verifies that all struct types with map fields"
        echo "either have persistence methods OR are in the known-ephemeral list."
        rm -f "$RESULTS_FILE"
        exit 0
    else
        echo "❌ VERIFICATION FAILED: Found $RESULT_COUNT persistence gap(s)"
        echo ""
        gh codeql bqrs decode "$RESULTS_FILE" --format=text
        echo ""
        echo "Fix: Either add persistence methods (Save/Persist/Set/Restore)"
        echo "     or add the struct to isKnownEphemeralStruct() in the query"
        rm -f "$RESULTS_FILE"
        exit 1
    fi
else
    echo "❌ Query execution failed"
    exit 1
fi
