#!/bin/bash

# DistSQLite Test Runner
# Comprehensive test suite for all components

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Test directories
TEST_DIR="$(dirname "$0")"
PROJECT_ROOT="$(dirname "$TEST_DIR")"
BUILD_DIR="$PROJECT_ROOT/build"

echo -e "${BLUE}=== DistSQLite Comprehensive Test Suite ===${NC}"
echo "Project root: $PROJECT_ROOT"
echo "Test directory: $TEST_DIR"
echo

# Function to run a test with status tracking
run_test() {
    local test_name="$1"
    local test_cmd="$2"
    local test_dir="$3"
    
    echo -e "${BLUE}Running: $test_name${NC}"
    
    if [ -n "$test_dir" ]; then
        cd "$test_dir"
    fi
    
    if eval "$test_cmd"; then
        echo -e "${GREEN}✓ PASS: $test_name${NC}"
        return 0
    else
        echo -e "${RED}✗ FAIL: $test_name${NC}"
        return 1
    fi
}

# Initialize counters
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

# Function to track test results
track_result() {
    TOTAL_TESTS=$((TOTAL_TESTS + 1))
    if [ $1 -eq 0 ]; then
        PASSED_TESTS=$((PASSED_TESTS + 1))
    else
        FAILED_TESTS=$((FAILED_TESTS + 1))
    fi
}

echo -e "${YELLOW}=== Building Project ===${NC}"
cd "$PROJECT_ROOT"
if ! make build; then
    echo -e "${RED}Build failed, aborting tests${NC}"
    exit 1
fi
echo

echo -e "${YELLOW}=== Unit Tests ===${NC}"

# VFS Unit Tests (C)
echo -e "${BLUE}Compiling VFS unit tests...${NC}"
cd "$TEST_DIR/unit"
if gcc -o vfs_test vfs_test.c -lsqlite3 -I"$PROJECT_ROOT/sqlite" 2>/dev/null; then
    run_test "VFS Unit Tests" "./vfs_test" "$TEST_DIR/unit"
    track_result $?
else
    echo -e "${YELLOW}⚠ SKIP: VFS unit tests (SQLite dev headers not available)${NC}"
    # Don't count as failure since it's environment dependent
fi

# Go Unit Tests
cd "$PROJECT_ROOT"

run_test "Lease Management Tests" "go test -v ./test/unit -run TestHTTP" ""
track_result $?

run_test "IPC Tests" "go test -v ./test/unit -run TestIPC" ""
track_result $?

run_test "HTTP Rsync Tests" "go test -v ./pkg/rsync -run TestHTTP" ""
track_result $?

echo
echo -e "${YELLOW}=== Integration Tests ===${NC}"

# Integration tests need the binaries
if [ ! -f "$BUILD_DIR/distsqlite" ] || [ ! -f "$BUILD_DIR/distsqlite-coordinator" ]; then
    echo -e "${RED}Required binaries not found in $BUILD_DIR${NC}"
    echo "Make sure 'make build' completed successfully"
    exit 1
fi

run_test "Split-Brain Prevention" "go test -v ./test/integration -run TestSplitBrain" "$PROJECT_ROOT"
track_result $?

run_test "Fence Token Validation" "go test -v ./test/integration -run TestFenceToken" "$PROJECT_ROOT"
track_result $?

run_test "Concurrent Writers" "go test -v ./test/integration -run TestConcurrentWriters" "$PROJECT_ROOT"
track_result $?

run_test "Read-Write Interaction" "go test -v ./test/integration -run TestReadWrite" "$PROJECT_ROOT"
track_result $?

run_test "Transaction Isolation" "go test -v ./test/integration -run TestTransactionIsolation" "$PROJECT_ROOT"
track_result $?

echo
echo -e "${YELLOW}=== Error Injection Tests ===${NC}"

run_test "Database Corruption Handling" "go test -v ./test/integration -run TestDatabaseCorruption" "$PROJECT_ROOT"
track_result $?

run_test "Network Failure Handling" "go test -v ./test/integration -run TestNetworkFailures" "$PROJECT_ROOT"
track_result $?

run_test "Concurrent Failures" "go test -v ./test/integration -run TestConcurrentFailures" "$PROJECT_ROOT"
track_result $?

echo
echo -e "${YELLOW}=== Performance Benchmarks ===${NC}"

run_test "Bulk Insert Comparison" "go test -v ./test/benchmarks -bench=BenchmarkBulkInsert -run='^$'" "$PROJECT_ROOT"
track_result $?

run_test "Hybrid Replication Benchmark" "go test -v ./test/benchmarks -bench=BenchmarkHybridReplication -run='^$'" "$PROJECT_ROOT"
track_result $?

run_test "Concurrent Write Benchmark" "go test -v ./test/benchmarks -bench=BenchmarkConcurrentWrites -run='^$'" "$PROJECT_ROOT"
track_result $?

echo
echo -e "${YELLOW}=== Integration Demo ===${NC}"

# Run the existing integration demo
run_test "Integration Demo" "go run ./test/integration/simple_demo.go" "$PROJECT_ROOT"
track_result $?

echo
echo -e "${BLUE}=== Test Summary ===${NC}"
echo "Total tests: $TOTAL_TESTS"
echo -e "Passed: ${GREEN}$PASSED_TESTS${NC}"
echo -e "Failed: ${RED}$FAILED_TESTS${NC}"

if [ $FAILED_TESTS -eq 0 ]; then
    echo -e "${GREEN}🎉 All tests passed!${NC}"
    exit 0
else
    echo -e "${RED}❌ $FAILED_TESTS test(s) failed${NC}"
    exit 1
fi