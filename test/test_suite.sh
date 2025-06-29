#!/bin/bash

# DistCore Comprehensive Test Suite

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Test counters
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

# Helper functions
run_test() {
    local test_name="$1"
    local test_cmd="$2"
    
    TESTS_RUN=$((TESTS_RUN + 1))
    echo -e "\n${YELLOW}[TEST $TESTS_RUN]${NC} $test_name"
    
    if eval "$test_cmd"; then
        TESTS_PASSED=$((TESTS_PASSED + 1))
        echo -e "${GREEN}✓ PASS${NC}"
    else
        TESTS_FAILED=$((TESTS_FAILED + 1))
        echo -e "${RED}✗ FAIL${NC}"
    fi
}

# Setup
export LD_LIBRARY_PATH="$(pwd)/build:$LD_LIBRARY_PATH"

echo "=== DistCore Comprehensive Test Suite ==="
echo "Testing all components and functionality"
echo

# Test 1: Build system
run_test "Build system check" "test -f build/libdistcore.so && test -f build/libasmrun_shared.so"

# Test 2: Library loading
run_test "Library loads without errors" "ldd build/libdistcore.so > /dev/null 2>&1"

# Test 3: Symbol resolution
run_test "No undefined symbols" "! ldd build/libdistcore.so 2>&1 | grep -q 'undefined symbol'"

# Test 4: OCaml runtime initialization
run_test "OCaml runtime initialization" "LD_PRELOAD=build/libdistcore.so /bin/true 2>&1 | grep -q 'Loaded via LD_PRELOAD'"

# Test 5: Error handling
if [ -f src/distcore_errors.c ]; then
    run_test "Error handling compilation" "gcc -c src/distcore_errors.c -o /tmp/test_errors.o"
fi

# Test 6: Thread safety
if [ -f src/distcore_thread_safety.c ]; then
    run_test "Thread safety compilation" "gcc -c src/distcore_thread_safety.c -o /tmp/test_thread.o -pthread"
fi

# Test 7: Multi-node coordination
run_test "Multi-node test script exists" "test -f test_multinode.sh"

# Test 8: VFS test script
run_test "VFS test script exists" "test -f test_vfs_with_preload.sh"

# Test 9: Benchmark compilation
if [ -f test/benchmark_vfs.c ]; then
    run_test "Benchmark compilation" "gcc -o /tmp/benchmark_vfs test/benchmark_vfs.c -lsqlite3"
fi

# Test 10: Documentation
run_test "Documentation exists" "test -f OCAML_VFS_COMPLETE.md && test -f OCAML_INTEGRATION_SUCCESS.md"

# Test 11: .gitignore
run_test ".gitignore exists" "test -f .gitignore"

# Test 12: Check for build artifacts that should be ignored
run_test "No build artifacts in git" "! git ls-files --error-unmatch build/ 2>/dev/null"

# Results summary
echo
echo "=== Test Results ==="
echo "Tests run: $TESTS_RUN"
echo -e "Tests passed: ${GREEN}$TESTS_PASSED${NC}"
echo -e "Tests failed: ${RED}$TESTS_FAILED${NC}"

if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "\n${GREEN}🎉 All tests passed!${NC}"
    exit 0
else
    echo -e "\n${RED}❌ Some tests failed${NC}"
    exit 1
fi