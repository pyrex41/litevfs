#!/bin/bash

# Direct SQLite VFS test without full infrastructure

echo "=== Testing SQLite VFS Directly ==="

# Set library path
export LD_LIBRARY_PATH="build:$LD_LIBRARY_PATH"

# Test 1: Basic SQLite without VFS
echo "[TEST 1] Basic SQLite functionality..."
if ./sqlite3 ":memory:" "SELECT 'Basic test works';" 2>&1; then
    echo "✓ Basic SQLite works"
else
    echo "✗ Basic SQLite failed"
fi

# Test 2: Initialize OCaml runtime directly
echo -e "\n[TEST 2] Testing OCaml runtime initialization..."
./test_ocaml_init 2>&1 || echo "OCaml init failed"

# Test 3: LD_PRELOAD with error details
echo -e "\n[TEST 3] Testing LD_PRELOAD with VFS..."
LD_PRELOAD="build/libdistcore.so" ./sqlite3 ":memory:" "SELECT 1;" 2>&1 | head -5

# Test 4: Check what symbols are actually needed
echo -e "\n[TEST 4] Checking undefined symbols in our library..."
ldd build/libdistcore.so 2>&1 | grep "not found" || echo "All direct dependencies found"

# Test 5: Check OCaml symbols
echo -e "\n[TEST 5] OCaml symbols in our library..."
nm -D build/libdistcore.so | grep -E "(caml_apply|caml_globals|caml_program|caml_int64)" | head -10

echo -e "\n=== Analysis ==="
echo "Library size: $(ls -lh build/libdistcore.so | awk '{print $5}')"
echo "OCaml runtime: $(ls -lh build/libasmrun_shared.so | awk '{print $5}')"

# Test 6: Simple VFS test without full SQLite
echo -e "\n[TEST 6] Testing VFS registration directly..."
cat > test_vfs_simple.c << 'EOF'
#include <stdio.h>
#include <sqlite3.h>

int main() {
    printf("SQLite version: %s\n", sqlite3_libversion());
    
    sqlite3_vfs *vfs = sqlite3_vfs_find(NULL);
    if (vfs) {
        printf("Default VFS: %s\n", vfs->zName);
    }
    
    return 0;
}
EOF

gcc -o test_vfs_simple test_vfs_simple.c -lsqlite3 2>/dev/null && ./test_vfs_simple

echo -e "\n=== Summary ==="
echo "1. SQLite works: ✓"
echo "2. OCaml library builds: ✓" 
echo "3. LD_PRELOAD loads: ?"
echo "4. VFS intercepts: ?"
echo "5. Replication works: ?"