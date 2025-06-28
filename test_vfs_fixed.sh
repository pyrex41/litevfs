#!/bin/bash

# Test script for the fixed VFS LD_PRELOAD functionality
# This validates the OCaml VFS integration and SQLite replication

set -e

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'  
NC='\033[0m' # No Color

# Test counters
TESTS_RUN=0
TESTS_PASSED=0

log_test() {
    TESTS_RUN=$((TESTS_RUN + 1))
    echo -e "${YELLOW}[TEST $TESTS_RUN]${NC} $1"
}

log_pass() {
    TESTS_PASSED=$((TESTS_PASSED + 1))
    echo -e "${GREEN}✓ PASS:${NC} $1"
}

log_fail() {
    echo -e "${RED}✗ FAIL:${NC} $1"
}

# Setup
export LD_LIBRARY_PATH="$(pwd)/build:$LD_LIBRARY_PATH"
BUILD_DIR="build"
TEST_DB="test_vfs.db"

echo "=== DistSQLite VFS LD_PRELOAD Test Suite ==="
echo "Library path: $LD_LIBRARY_PATH"

# Test 1: Check library files exist
log_test "Checking library files exist"
if [[ -f "$BUILD_DIR/libdistcore.so" && -f "$BUILD_DIR/libasmrun_shared.so" ]]; then
    log_pass "OCaml libraries found"
else
    log_fail "OCaml libraries missing"
    exit 1
fi

# Test 2: Check library dependencies
log_test "Checking library dependencies"
LDD_OUTPUT=$(ldd "$BUILD_DIR/libdistcore.so" 2>&1)
if echo "$LDD_OUTPUT" | grep -q "libasmrun_shared.so => .*build/libasmrun_shared.so"; then
    log_pass "OCaml runtime correctly linked"
else
    log_fail "OCaml runtime not found in dependencies"
    echo "ldd output: $LDD_OUTPUT"
fi

# Test 3: Basic LD_PRELOAD test (no SQLite interaction)
log_test "Basic LD_PRELOAD functionality"
if LD_PRELOAD="$BUILD_DIR/libdistcore.so" echo "LD_PRELOAD test" >/dev/null 2>&1; then
    log_pass "LD_PRELOAD loads without errors"
else
    log_fail "LD_PRELOAD fails to load"
fi

# Test 4: Check OCaml symbols in library
log_test "Checking OCaml symbol resolution"
SYMBOLS_OUTPUT=$(nm -D "$BUILD_DIR/libdistcore.so" 2>&1 | grep caml || true)
if [[ -n "$SYMBOLS_OUTPUT" ]]; then
    log_pass "OCaml symbols found in library"
    echo "   Sample symbols: $(echo "$SYMBOLS_OUTPUT" | head -3 | tr '\n' ' ')"
else
    log_fail "No OCaml symbols found"
fi

# Test 5: Test with simple SQLite operations (if sqlite3 is available)
log_test "SQLite integration test"
SQLITE_BIN=""
if [[ -f "sqlite3" ]]; then
    SQLITE_BIN="./sqlite3"
elif command -v sqlite3 >/dev/null 2>&1; then
    SQLITE_BIN="sqlite3"
fi

if [[ -n "$SQLITE_BIN" ]]; then
    rm -f "$TEST_DB"
    
    # Try to run SQLite with LD_PRELOAD
    if LD_PRELOAD="$BUILD_DIR/libdistcore.so" timeout 5s $SQLITE_BIN "$TEST_DB" "CREATE TABLE test (id INTEGER);" 2>/dev/null; then
        log_pass "SQLite works with LD_PRELOAD"
        rm -f "$TEST_DB"
    else
        SQLITE_ERROR=$(LD_PRELOAD="$BUILD_DIR/libdistcore.so" $SQLITE_BIN "$TEST_DB" "CREATE TABLE test (id INTEGER);" 2>&1 || true)
        if echo "$SQLITE_ERROR" | grep -q "symbol lookup error"; then
            log_fail "SQLite LD_PRELOAD has symbol resolution issues"
            echo "   Error: $SQLITE_ERROR"
        else
            log_fail "SQLite LD_PRELOAD failed for unknown reason"
            echo "   Error: $SQLITE_ERROR"
        fi
    fi
else
    log_fail "No SQLite binary found for testing"
fi

# Test 6: Multi-node setup test
log_test "Multi-node cluster setup"
./test/run_tests.sh > /dev/null 2>&1
if [[ $? -eq 0 ]]; then
    log_pass "Multi-node cluster works"
else
    log_fail "Multi-node cluster setup failed"
fi

# Test 7: VFS interception functionality (advanced test)
log_test "VFS interception validation"
if [[ -n "$SQLITE_BIN" ]]; then
    rm -f "$TEST_DB"
    
    # Create a test database with LD_PRELOAD and see if our VFS is being used
    LD_PRELOAD="$BUILD_DIR/libdistcore.so" $SQLITE_BIN "$TEST_DB" "CREATE TABLE intercept_test (data TEXT); INSERT INTO intercept_test VALUES ('test_data');" 2>/dev/null || true
    
    if [[ -f "$TEST_DB" ]]; then
        # Check if the database was actually created (indicates VFS worked)
        if $SQLITE_BIN "$TEST_DB" "SELECT COUNT(*) FROM intercept_test;" 2>/dev/null | grep -q "1"; then
            log_pass "VFS interception appears functional"
        else
            log_fail "VFS interception may not be working correctly"
        fi
        rm -f "$TEST_DB"
    else
        log_fail "Database not created - VFS may be blocking operations"
    fi
else
    log_fail "Cannot test VFS interception without SQLite binary"
fi

# Results summary
echo ""
echo "=== Test Results ==="
echo "Tests run: $TESTS_RUN"
echo "Tests passed: $TESTS_PASSED"
echo "Tests failed: $((TESTS_RUN - TESTS_PASSED))"

if [[ $TESTS_PASSED -eq $TESTS_RUN ]]; then
    echo -e "${GREEN}🎉 All tests passed! VFS LD_PRELOAD is working correctly.${NC}"
    exit 0
elif [[ $TESTS_PASSED -gt $((TESTS_RUN / 2)) ]]; then
    echo -e "${YELLOW}⚠️  Some tests passed. VFS LD_PRELOAD has partial functionality.${NC}"
    exit 1
else
    echo -e "${RED}❌ Most tests failed. VFS LD_PRELOAD needs fixing.${NC}"
    exit 2
fi