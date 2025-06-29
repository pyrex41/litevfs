#!/bin/bash

# Test VFS registration with LD_PRELOAD

set -e

export LD_LIBRARY_PATH="$(pwd)/build:$LD_LIBRARY_PATH"
TEST_DB="test_vfs_preload.db"

echo "=== Testing DistCore VFS with LD_PRELOAD ==="
echo

# Clean up
rm -f "$TEST_DB" "${TEST_DB}-wal" "${TEST_DB}-shm"

# Test 1: Check if VFS gets registered
echo "Test 1: VFS Registration"
echo "Running: LD_PRELOAD=./build/libdistcore.so ./sqlite3 :memory: '.vfslist'"
echo
LD_PRELOAD="./build/libdistcore.so" ./sqlite3 :memory: ".vfslist" 2>&1 | grep -E "(distcore|vfs)" || true
echo

# Test 2: Create database with VFS
echo "Test 2: Create database"
LD_PRELOAD="./build/libdistcore.so" ./sqlite3 "$TEST_DB" <<EOF
CREATE TABLE test_vfs (id INTEGER PRIMARY KEY, data TEXT);
INSERT INTO test_vfs (data) VALUES ('Testing DistCore VFS');
SELECT 'Created and inserted data successfully';
EOF

# Test 3: Read back data
echo
echo "Test 3: Read data"
LD_PRELOAD="./build/libdistcore.so" ./sqlite3 "$TEST_DB" <<EOF
SELECT data FROM test_vfs WHERE id = 1;
EOF

# Test 4: Check database file
echo
echo "Test 4: Database file check"
if [[ -f "$TEST_DB" ]]; then
    SIZE=$(stat -c%s "$TEST_DB")
    echo "✓ Database file exists, size: $SIZE bytes"
else
    echo "✗ Database file not found"
fi

# Test 5: Multiple operations
echo
echo "Test 5: Multiple operations"
LD_PRELOAD="./build/libdistcore.so" ./sqlite3 "$TEST_DB" <<EOF
BEGIN;
INSERT INTO test_vfs (data) VALUES ('Row 2'), ('Row 3'), ('Row 4');
UPDATE test_vfs SET data = 'Updated: ' || data WHERE id > 1;
DELETE FROM test_vfs WHERE id = 4;
COMMIT;
SELECT COUNT(*) AS total_rows FROM test_vfs;
SELECT * FROM test_vfs ORDER BY id;
EOF

# Clean up
rm -f "$TEST_DB" "${TEST_DB}-wal" "${TEST_DB}-shm"

echo
echo "=== VFS tests completed ==="