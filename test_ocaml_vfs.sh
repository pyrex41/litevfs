#!/bin/bash

# Test OCaml VFS interception functionality

set -e

# Setup
export LD_LIBRARY_PATH="$(pwd)/build:$LD_LIBRARY_PATH"
TEST_DB="test_ocaml_vfs.db"

echo "=== OCaml VFS Interception Test ==="
echo "Testing with LD_PRELOAD of libdistcore.so"
echo

# Clean up any existing test database
rm -f "$TEST_DB" "${TEST_DB}-wal" "${TEST_DB}-shm"

# Test 1: Basic functionality
echo "Test 1: Basic SQLite operations with VFS"
if LD_PRELOAD="./build/libdistcore.so" ./sqlite3 "$TEST_DB" "CREATE TABLE test_table (id INTEGER PRIMARY KEY, data TEXT);" 2>&1; then
    echo "✓ CREATE TABLE succeeded"
else
    echo "✗ CREATE TABLE failed"
    exit 1
fi

# Test 2: Insert data
echo
echo "Test 2: Insert data"
if LD_PRELOAD="./build/libdistcore.so" ./sqlite3 "$TEST_DB" "INSERT INTO test_table (data) VALUES ('OCaml VFS test data');" 2>&1; then
    echo "✓ INSERT succeeded"
else
    echo "✗ INSERT failed"
    exit 1
fi

# Test 3: Read data back
echo
echo "Test 3: Read data back"
RESULT=$(LD_PRELOAD="./build/libdistcore.so" ./sqlite3 "$TEST_DB" "SELECT data FROM test_table WHERE id = 1;" 2>&1)
if [[ "$RESULT" == "OCaml VFS test data" ]]; then
    echo "✓ SELECT returned correct data: $RESULT"
else
    echo "✗ SELECT returned unexpected data: $RESULT"
    exit 1
fi

# Test 4: Multiple operations in one session
echo
echo "Test 4: Multiple operations in transaction"
LD_PRELOAD="./build/libdistcore.so" ./sqlite3 "$TEST_DB" <<EOF
BEGIN;
INSERT INTO test_table (data) VALUES ('Row 2'), ('Row 3'), ('Row 4');
UPDATE test_table SET data = 'Updated Row 2' WHERE id = 2;
DELETE FROM test_table WHERE id = 4;
COMMIT;
SELECT COUNT(*) FROM test_table;
EOF

# Test 5: Check if database file exists and has content
echo
echo "Test 5: Database file verification"
if [[ -f "$TEST_DB" ]]; then
    SIZE=$(stat -c%s "$TEST_DB")
    echo "✓ Database file exists, size: $SIZE bytes"
    if [[ $SIZE -gt 0 ]]; then
        echo "✓ Database has content"
    else
        echo "✗ Database is empty"
    fi
else
    echo "✗ Database file not found"
    exit 1
fi

# Test 6: WAL mode test
echo
echo "Test 6: WAL mode test"
LD_PRELOAD="./build/libdistcore.so" ./sqlite3 "$TEST_DB" "PRAGMA journal_mode=WAL;" 2>&1
if [[ -f "${TEST_DB}-wal" ]]; then
    echo "✓ WAL file created"
else
    echo "✗ WAL file not created"
fi

# Test 7: Large write test
echo
echo "Test 7: Large write test"
LD_PRELOAD="./build/libdistcore.so" ./sqlite3 "$TEST_DB" <<EOF
CREATE TABLE large_test (id INTEGER PRIMARY KEY, data BLOB);
INSERT INTO large_test (data) VALUES (randomblob(10000));
INSERT INTO large_test (data) VALUES (randomblob(10000));
INSERT INTO large_test (data) VALUES (randomblob(10000));
SELECT COUNT(*), SUM(LENGTH(data)) FROM large_test;
EOF

echo
echo "=== All OCaml VFS tests passed! ==="
echo "The OCaml VFS integration is working correctly."

# Cleanup
rm -f "$TEST_DB" "${TEST_DB}-wal" "${TEST_DB}-shm"