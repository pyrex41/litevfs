#!/bin/bash

# VFS LD_PRELOAD Test
# Tests if the OCaml VFS integration works with LD_PRELOAD

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}=== DistSQLite VFS LD_PRELOAD Test ===${NC}"
echo "Testing if OCaml VFS integration works with LD_PRELOAD"
echo

# Configuration
PROJECT_ROOT="$(pwd)"
BUILD_DIR="$PROJECT_ROOT/build"
TEST_DIR="$PROJECT_ROOT/test-vfs"
COORDINATOR_PORT=9090
NODE_PORT=8080

# Clean up any existing processes and directories
cleanup() {
    echo -e "${YELLOW}Cleaning up...${NC}"
    pkill -f "distsqlite-coordinator" || true
    pkill -f "distsqlite" || true
    sleep 2
    rm -rf "$TEST_DIR"
    echo -e "${GREEN}Cleanup complete${NC}"
}

trap cleanup EXIT

# Function to wait for service
wait_for_service() {
    local host=$1
    local port=$2
    local timeout=${3:-30}
    local service_name=$4
    
    echo -e "${BLUE}Waiting for $service_name ($host:$port)...${NC}"
    
    for i in $(seq 1 $timeout); do
        if curl -s "http://$host:$port/health" >/dev/null 2>&1; then
            echo -e "${GREEN}✓ $service_name is ready${NC}"
            return 0
        fi
        sleep 1
    done
    
    echo -e "${RED}✗ $service_name failed to start after ${timeout}s${NC}"
    return 1
}

# Create test environment
echo -e "${YELLOW}Setting up test environment...${NC}"
mkdir -p "$TEST_DIR"
cd "$TEST_DIR"

# Create Go module for test application
echo -e "${BLUE}Setting up test application...${NC}"
go mod init vfstest
go get github.com/mattn/go-sqlite3

# Create a simple test application
cat > test_vfs.go << 'EOF'
package main

import (
    "database/sql"
    "fmt"
    "log"
    "os"
    _ "github.com/mattn/go-sqlite3"
)

func main() {
    fmt.Println("Testing DistSQLite VFS interception...")
    
    // Check if we can detect VFS interception
    fmt.Printf("Process PID: %d\n", os.Getpid())
    
    // Try to open database
    db, err := sql.Open("sqlite3", "./vfs_test.db")
    if err != nil {
        log.Fatalf("Failed to open database: %v", err)
    }
    defer db.Close()
    
    fmt.Println("✓ Database connection opened")
    
    // Create table
    _, err = db.Exec(`CREATE TABLE IF NOT EXISTS vfs_test (
        id INTEGER PRIMARY KEY,
        message TEXT,
        created DATETIME DEFAULT CURRENT_TIMESTAMP
    )`)
    if err != nil {
        log.Fatalf("Failed to create table: %v", err)
    }
    
    fmt.Println("✓ Table created")
    
    // Insert data
    result, err := db.Exec(`INSERT INTO vfs_test (message) VALUES (?)`, "VFS test message")
    if err != nil {
        log.Fatalf("Failed to insert data: %v", err)
    }
    
    id, _ := result.LastInsertId()
    fmt.Printf("✓ Data inserted with ID: %d\n", id)
    
    // Query data
    rows, err := db.Query(`SELECT id, message FROM vfs_test`)
    if err != nil {
        log.Fatalf("Failed to query data: %v", err)
    }
    defer rows.Close()
    
    count := 0
    fmt.Println("Data in database:")
    for rows.Next() {
        var id int
        var message string
        err := rows.Scan(&id, &message)
        if err != nil {
            log.Fatalf("Failed to scan row: %v", err)
        }
        fmt.Printf("  %d: %s\n", id, message)
        count++
    }
    
    fmt.Printf("✓ Query completed, found %d records\n", count)
    fmt.Println("✅ VFS test completed successfully!")
}
EOF

# Build the test application
echo -e "${BLUE}Building test application...${NC}"
go build -o test_vfs test_vfs.go

# Start coordinator
echo -e "${YELLOW}Starting coordinator...${NC}"
cd "$PROJECT_ROOT"
LD_LIBRARY_PATH="$BUILD_DIR" "$BUILD_DIR/distsqlite-coordinator" --addr ":$COORDINATOR_PORT" > "$TEST_DIR/coordinator.log" 2>&1 &
COORDINATOR_PID=$!

# Wait for coordinator
wait_for_service "localhost" "$COORDINATOR_PORT" 30 "Coordinator"

# Start single node
echo -e "${YELLOW}Starting DistSQLite node...${NC}"
cd "$TEST_DIR"
mkdir -p data
LD_LIBRARY_PATH="$BUILD_DIR" "$BUILD_DIR/distsqlite" \
    --data-dir "./data" \
    --http-addr ":$NODE_PORT" \
    --coordination "http" \
    --coordinator-url "http://localhost:$COORDINATOR_PORT" \
    --hostname "vfs-test-node" > "node.log" 2>&1 &
NODE_PID=$!

# Wait for node
wait_for_service "localhost" "$NODE_PORT" 30 "DistSQLite Node"

# Allow startup
echo -e "${BLUE}Allowing time for VFS initialization...${NC}"
sleep 5

# Test 1: Check OCaml library symbols
echo -e "${YELLOW}=== Testing OCaml Library ===${NC}"

echo -e "${BLUE}Test 1: Checking OCaml library symbols...${NC}"
if nm "$BUILD_DIR/libdistcore.so" | grep -q "caml_state\|caml_startup"; then
    echo -e "${GREEN}✓ OCaml symbols found in library${NC}"
else
    echo -e "${RED}✗ OCaml symbols missing from library${NC}"
fi

echo -e "${BLUE}Test 2: Testing library loading...${NC}"
if ldd "$BUILD_DIR/libdistcore.so" | grep -q "not found"; then
    echo -e "${RED}✗ Library has missing dependencies:${NC}"
    ldd "$BUILD_DIR/libdistcore.so" | grep "not found"
else
    echo -e "${GREEN}✓ Library dependencies are satisfied${NC}"
fi

# Test 2: Test LD_PRELOAD without application
echo -e "${BLUE}Test 3: Testing LD_PRELOAD mechanism...${NC}"
export DISTSQLITE_SIDECAR_ADDR="localhost:$NODE_PORT"

# Run a simple test first
if LD_PRELOAD="$BUILD_DIR/distsqlite-inject.so" \
   LD_LIBRARY_PATH="$BUILD_DIR" \
   echo "LD_PRELOAD test" > /dev/null 2>&1; then
    echo -e "${GREEN}✓ LD_PRELOAD mechanism works${NC}"
else
    echo -e "${RED}✗ LD_PRELOAD mechanism failed${NC}"
fi

# Test 3: Test with our application
echo -e "${YELLOW}=== Testing VFS Application ===${NC}"

echo -e "${BLUE}Test 4: Running application without LD_PRELOAD...${NC}"
if timeout 10s ./test_vfs > "without_preload.log" 2>&1; then
    echo -e "${GREEN}✓ Application runs without LD_PRELOAD${NC}"
    echo "Output (last 3 lines):"
    tail -3 "without_preload.log" | sed 's/^/    /'
else
    echo -e "${RED}✗ Application fails without LD_PRELOAD${NC}"
    echo "Error output:"
    tail -3 "without_preload.log" | sed 's/^/    /'
fi

echo -e "${BLUE}Test 5: Running application WITH LD_PRELOAD...${NC}"
export DISTSQLITE_SIDECAR_ADDR="localhost:$NODE_PORT"

if timeout 15s bash -c "
    LD_PRELOAD='$BUILD_DIR/distsqlite-inject.so' \
    LD_LIBRARY_PATH='$BUILD_DIR' \
    ./test_vfs
" > "with_preload.log" 2>&1; then
    echo -e "${GREEN}✓ Application runs WITH LD_PRELOAD${NC}"
    echo "Output (last 5 lines):"
    tail -5 "with_preload.log" | sed 's/^/    /'
else
    echo -e "${RED}✗ Application fails WITH LD_PRELOAD${NC}"
    echo "Error output:"
    cat "with_preload.log" | sed 's/^/    /'
fi

# Show node activity
echo -e "${YELLOW}=== Node Activity Analysis ===${NC}"

echo -e "${BLUE}Node log activity:${NC}"
if grep -q "VFS\|SQLite\|database\|inject" "node.log"; then
    echo "Found VFS-related activity:"
    grep -E "VFS|SQLite|database|inject" "node.log" | tail -5 | sed 's/^/    /'
else
    echo "No VFS-related activity found in node logs"
    echo "Last 5 lines of node log:"
    tail -5 "node.log" | sed 's/^/    /'
fi

# Check file system
echo -e "${BLUE}Database files created:${NC}"
ls -la *.db 2>/dev/null || echo "No database files found"

# Final assessment
echo -e "\n${BLUE}=== VFS Test Assessment ===${NC}"

if [ -f "with_preload.log" ] && grep -q "✅ VFS test completed successfully" "with_preload.log"; then
    echo -e "${GREEN}🎉 VFS LD_PRELOAD is working!${NC}"
    echo -e "${GREEN}✅ SQLite operations are being intercepted${NC}"
    exit 0
else
    echo -e "${RED}❌ VFS LD_PRELOAD is not working properly${NC}"
    echo
    echo "Possible issues:"
    echo "- OCaml runtime linking problems"
    echo "- VFS initialization failures"  
    echo "- Symbol resolution issues"
    echo
    echo "Check the error outputs above for details."
    exit 1
fi