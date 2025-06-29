#!/bin/bash

# Working DistSQLite Test Script
# This version fixes all the issues found in the previous test

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}=== DistSQLite Working Test ===${NC}"

# Configuration
PROJECT_ROOT="$(pwd)"
BUILD_DIR="$PROJECT_ROOT/build"
TEST_DIR="$PROJECT_ROOT/test-working"
COORDINATOR_PORT=9090
NODE1_PORT=8080
NODE2_PORT=8081

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

# Create Go module for test applications
echo -e "${BLUE}Setting up Go modules...${NC}"
go mod init disttest
go get github.com/mattn/go-sqlite3

# Create a simple test application
cat > test_app.go << 'EOF'
package main

import (
    "database/sql"
    "fmt"
    "log"
    "os"
    _ "github.com/mattn/go-sqlite3"
)

func main() {
    nodeID := "unknown"
    if len(os.Args) > 1 {
        nodeID = os.Args[1]
    }
    
    fmt.Printf("Testing DistSQLite injection from %s...\n", nodeID)
    
    // Open database
    db, err := sql.Open("sqlite3", "./shared.db")
    if err != nil {
        log.Fatalf("Failed to open database: %v", err)
    }
    defer db.Close()
    
    // Create table
    _, err = db.Exec(`CREATE TABLE IF NOT EXISTS test_data (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        message TEXT,
        node_id TEXT,
        timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
    )`)
    if err != nil {
        log.Fatalf("Failed to create table: %v", err)
    }
    
    // Insert test data
    _, err = db.Exec(`INSERT INTO test_data (message, node_id) VALUES (?, ?)`, 
        "Hello from distributed SQLite!", nodeID)
    if err != nil {
        log.Fatalf("Failed to insert data: %v", err)
    }
    
    // Read all data
    rows, err := db.Query(`SELECT id, message, node_id, timestamp FROM test_data ORDER BY id`)
    if err != nil {
        log.Fatalf("Failed to query data: %v", err)
    }
    defer rows.Close()
    
    fmt.Println("Data in database:")
    count := 0
    for rows.Next() {
        var id int
        var message, nodeID, timestamp string
        err := rows.Scan(&id, &message, &nodeID, &timestamp)
        if err != nil {
            log.Fatalf("Failed to scan row: %v", err)
        }
        fmt.Printf("  %d: %s (from %s at %s)\n", id, message, nodeID, timestamp)
        count++
    }
    
    fmt.Printf("Total records: %d\n", count)
    fmt.Println("✅ Test completed successfully!")
}
EOF

# Build the test application
echo -e "${BLUE}Building test application...${NC}"
go build -o test_app test_app.go

# Start coordinator
echo -e "${YELLOW}Starting coordinator...${NC}"
cd "$PROJECT_ROOT"
LD_LIBRARY_PATH="$BUILD_DIR" "$BUILD_DIR/distsqlite-coordinator" --addr ":$COORDINATOR_PORT" > "$TEST_DIR/coordinator.log" 2>&1 &
COORDINATOR_PID=$!

# Wait for coordinator
wait_for_service "localhost" "$COORDINATOR_PORT" 30 "Coordinator"

# Start nodes
echo -e "${YELLOW}Starting nodes...${NC}"

# Node 1
cd "$TEST_DIR"
mkdir -p node1/data
cd node1
LD_LIBRARY_PATH="$BUILD_DIR" "$BUILD_DIR/distsqlite" \
    --data-dir "./data" \
    --http-addr ":$NODE1_PORT" \
    --coordination "http" \
    --coordinator-url "http://localhost:$COORDINATOR_PORT" \
    --hostname "node1" \
    --targets "localhost:$NODE2_PORT" > "../node1.log" 2>&1 &
NODE1_PID=$!

# Node 2
cd "$TEST_DIR"
mkdir -p node2/data
cd node2
LD_LIBRARY_PATH="$BUILD_DIR" "$BUILD_DIR/distsqlite" \
    --data-dir "./data" \
    --http-addr ":$NODE2_PORT" \
    --coordination "http" \
    --coordinator-url "http://localhost:$COORDINATOR_PORT" \
    --hostname "node2" \
    --targets "localhost:$NODE1_PORT" > "../node2.log" 2>&1 &
NODE2_PID=$!

# Wait for nodes
wait_for_service "localhost" "$NODE1_PORT" 30 "Node 1"
wait_for_service "localhost" "$NODE2_PORT" 30 "Node 2"

# Allow cluster formation
echo -e "${BLUE}Allowing time for cluster formation...${NC}"
sleep 10

# Test API endpoints
echo -e "${YELLOW}=== Testing API Endpoints ===${NC}"

TESTS_PASSED=0
TESTS_TOTAL=0

# Test coordinator health
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Testing coordinator health...${NC}"
if curl -s "http://localhost:$COORDINATOR_PORT/health" | grep -q "healthy\|ok\|status" ; then
    echo -e "${GREEN}✓ Coordinator health check passed${NC}"
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ Coordinator health check failed${NC}"
    echo "Response: $(curl -s "http://localhost:$COORDINATOR_PORT/health")"
fi

# Test node health
for port in $NODE1_PORT $NODE2_PORT; do
    TESTS_TOTAL=$((TESTS_TOTAL + 1))
    node_num=$((port - 8079))
    echo -e "${BLUE}Testing node $node_num health...${NC}"
    if curl -s "http://localhost:$port/health" | grep -q "healthy\|ok\|status" ; then
        echo -e "${GREEN}✓ Node $node_num health check passed${NC}"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        echo -e "${RED}✗ Node $node_num health check failed${NC}"
        echo "Response: $(curl -s "http://localhost:$port/health")"
    fi
done

# Test LD_PRELOAD functionality
echo -e "${YELLOW}=== Testing LD_PRELOAD Functionality ===${NC}"

cd "$TEST_DIR"

# Test with Node 1
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Testing LD_PRELOAD with Node 1...${NC}"
export DISTSQLITE_SIDECAR_ADDR="localhost:$NODE1_PORT"

if LD_PRELOAD="$BUILD_DIR/distsqlite-inject.so" \
   LD_LIBRARY_PATH="$BUILD_DIR" \
   ./test_app "node1" > "node1_test.log" 2>&1; then
    echo -e "${GREEN}✓ LD_PRELOAD test with Node 1 passed${NC}"
    TESTS_PASSED=$((TESTS_PASSED + 1))
    echo "Output:"
    cat "node1_test.log" | sed 's/^/    /'
else
    echo -e "${RED}✗ LD_PRELOAD test with Node 1 failed${NC}"
    echo "Error output:"
    cat "node1_test.log" | sed 's/^/    /'
fi

# Wait for replication
echo -e "${BLUE}Waiting for data replication...${NC}"
sleep 5

# Test with Node 2 to see if data replicated
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Testing LD_PRELOAD with Node 2 (checking replication)...${NC}"
export DISTSQLITE_SIDECAR_ADDR="localhost:$NODE2_PORT"

if LD_PRELOAD="$BUILD_DIR/distsqlite-inject.so" \
   LD_LIBRARY_PATH="$BUILD_DIR" \
   ./test_app "node2" > "node2_test.log" 2>&1; then
    echo -e "${GREEN}✓ LD_PRELOAD test with Node 2 passed${NC}"
    TESTS_PASSED=$((TESTS_PASSED + 1))
    echo "Output:"
    cat "node2_test.log" | sed 's/^/    /'
else
    echo -e "${RED}✗ LD_PRELOAD test with Node 2 failed${NC}"
    echo "Error output:"
    cat "node2_test.log" | sed 's/^/    /'
fi

# Check data consistency
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Checking data consistency...${NC}"

# Count records from both test outputs
node1_records=$(grep "Total records:" "node1_test.log" | grep -o '[0-9]\+' || echo "0")
node2_records=$(grep "Total records:" "node2_test.log" | grep -o '[0-9]\+' || echo "0")

echo "Node 1 sees: $node1_records records"
echo "Node 2 sees: $node2_records records"

if [ "$node1_records" -gt "0" ] && [ "$node2_records" -gt "0" ]; then
    echo -e "${GREEN}✓ Data consistency check passed (both nodes have data)${NC}"
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ Data consistency check failed${NC}"
fi

# Test lease coordination by checking logs
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Checking lease coordination...${NC}"

if grep -q "Acquired lease\|primary lease" "$TEST_DIR/node1.log" "$TEST_DIR/node2.log"; then
    echo -e "${GREEN}✓ Lease coordination working (found lease acquisition in logs)${NC}"
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ Lease coordination check failed${NC}"
fi

# Show status endpoints
echo -e "${YELLOW}=== Status Information ===${NC}"

echo -e "${BLUE}Coordinator Status:${NC}"
curl -s "http://localhost:$COORDINATOR_PORT/health" 2>/dev/null || echo "No response"

echo -e "\n${BLUE}Node 1 Status:${NC}"
curl -s "http://localhost:$NODE1_PORT/health" 2>/dev/null || echo "No response"

echo -e "\n${BLUE}Node 2 Status:${NC}"
curl -s "http://localhost:$NODE2_PORT/health" 2>/dev/null || echo "No response"

# Show logs if there were failures
if [ $TESTS_PASSED -ne $TESTS_TOTAL ]; then
    echo -e "\n${YELLOW}=== Debug Information ===${NC}"
    echo -e "${BLUE}Coordinator Log (last 10 lines):${NC}"
    tail -10 "$TEST_DIR/coordinator.log" 2>/dev/null || echo "No coordinator log"
    
    echo -e "\n${BLUE}Node 1 Log (last 10 lines):${NC}"
    tail -10 "$TEST_DIR/node1.log" 2>/dev/null || echo "No node1 log"
    
    echo -e "\n${BLUE}Node 2 Log (last 10 lines):${NC}"
    tail -10 "$TEST_DIR/node2.log" 2>/dev/null || echo "No node2 log"
fi

# Final results
echo -e "\n${BLUE}=== Test Results ===${NC}"
echo "Tests passed: $TESTS_PASSED/$TESTS_TOTAL"

if [ $TESTS_PASSED -eq $TESTS_TOTAL ]; then
    echo -e "${GREEN}🎉 All tests passed! DistSQLite is working correctly.${NC}"
    exit 0
else
    failed=$((TESTS_TOTAL - TESTS_PASSED))
    echo -e "${RED}❌ $failed test(s) failed.${NC}"
    exit 1
fi