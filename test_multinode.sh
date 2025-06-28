#!/bin/bash

# Multi-node testing script for DistSQLite
# Tests distributed functionality with multiple local nodes

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}=== DistSQLite Multi-Node Test ===${NC}"

# Configuration
PROJECT_ROOT="$(pwd)"
BUILD_DIR="$PROJECT_ROOT/build"
TEST_DIR="$PROJECT_ROOT/test-nodes"
COORDINATOR_PORT=9090
NODE1_PORT=8080
NODE2_PORT=8081
NODE3_PORT=8082

# Clean up any existing processes
cleanup() {
    echo -e "${YELLOW}Cleaning up processes...${NC}"
    pkill -f "distsqlite-coordinator" || true
    pkill -f "distsqlite" || true
    sleep 2
    
    # Clean up test directories
    rm -rf "$TEST_DIR"
    echo -e "${GREEN}Cleanup complete${NC}"
}

# Set up cleanup on exit
trap cleanup EXIT

# Function to check if a service is ready
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

# Function to run a test
run_test() {
    local test_name="$1"
    local test_cmd="$2"
    
    echo -e "${BLUE}Running: $test_name${NC}"
    
    if eval "$test_cmd"; then
        echo -e "${GREEN}✓ PASS: $test_name${NC}"
        return 0
    else
        echo -e "${RED}✗ FAIL: $test_name${NC}"
        return 1
    fi
}

# Prepare test environment
echo -e "${YELLOW}Setting up test environment...${NC}"

# Create test directories
mkdir -p "$TEST_DIR"/{node1,node2,node3}

# Copy configuration template
cat > "$TEST_DIR/base-config.yml" << 'EOF'
# Base configuration for test nodes
data-dir: "./data"
http-addr: ":8080"
coordination: "http"
coordinator-url: "http://localhost:9090"
replication:
  bulk-threshold: 1048576
  max-delay: "5s"
lease:
  ttl: "10s"
  lock-delay: "1s"
logging:
  level: "info"
  format: "text"
EOF

# Create node-specific configs
for i in {1..3}; do
    port=$((8079 + i))
    sed "s/:8080/:$port/g" "$TEST_DIR/base-config.yml" > "$TEST_DIR/node$i/config.yml"
    echo "hostname: \"node$i\"" >> "$TEST_DIR/node$i/config.yml"
    echo "targets:" >> "$TEST_DIR/node$i/config.yml"
    for j in {1..3}; do
        if [ $i -ne $j ]; then
            target_port=$((8079 + j))
            echo "  - \"localhost:$target_port\"" >> "$TEST_DIR/node$i/config.yml"
        fi
    done
done

# Verify binaries exist
echo -e "${BLUE}Checking binaries...${NC}"
for binary in distsqlite distsqlite-coordinator distsqlite-inject.so libdistcore.so; do
    if [ ! -f "$BUILD_DIR/$binary" ]; then
        echo -e "${RED}Binary not found: $BUILD_DIR/$binary${NC}"
        echo "Please run 'make build' first"
        exit 1
    fi
done
echo -e "${GREEN}✓ All binaries found${NC}"

# Start the coordinator
echo -e "${YELLOW}Starting HTTP coordinator...${NC}"
cd "$PROJECT_ROOT"
LD_LIBRARY_PATH="$BUILD_DIR" "$BUILD_DIR/distsqlite-coordinator" --addr ":$COORDINATOR_PORT" &
COORDINATOR_PID=$!

# Wait for coordinator
wait_for_service "localhost" "$COORDINATOR_PORT" 30 "HTTP Coordinator"

# Start the nodes
echo -e "${YELLOW}Starting DistSQLite nodes...${NC}"

# Node 1 (Primary)
cd "$TEST_DIR/node1"
mkdir -p data
LD_LIBRARY_PATH="$BUILD_DIR" "$BUILD_DIR/distsqlite" \
    --data-dir "./data" \
    --http-addr ":8080" \
    --coordination "http" \
    --coordinator-url "http://localhost:9090" \
    --hostname "node1" \
    --targets "localhost:8081,localhost:8082" &
NODE1_PID=$!

# Node 2 (Secondary)
cd "$TEST_DIR/node2"
mkdir -p data
LD_LIBRARY_PATH="$BUILD_DIR" "$BUILD_DIR/distsqlite" \
    --data-dir "./data" \
    --http-addr ":8081" \
    --coordination "http" \
    --coordinator-url "http://localhost:9090" \
    --hostname "node2" \
    --targets "localhost:8080,localhost:8082" &
NODE2_PID=$!

# Node 3 (Secondary)
cd "$TEST_DIR/node3"
mkdir -p data
LD_LIBRARY_PATH="$BUILD_DIR" "$BUILD_DIR/distsqlite" \
    --data-dir "./data" \
    --http-addr ":8082" \
    --coordination "http" \
    --coordinator-url "http://localhost:9090" \
    --hostname "node3" \
    --targets "localhost:8080,localhost:8081" &
NODE3_PID=$!

# Wait for all nodes
wait_for_service "localhost" "$NODE1_PORT" 30 "Node 1"
wait_for_service "localhost" "$NODE2_PORT" 30 "Node 2"
wait_for_service "localhost" "$NODE3_PORT" 30 "Node 3"

# Give systems time to establish connections
echo -e "${BLUE}Allowing time for cluster formation...${NC}"
sleep 5

# Test basic functionality
echo -e "${YELLOW}=== Running Tests ===${NC}"

TESTS_PASSED=0
TESTS_TOTAL=0

# Test 1: Check coordinator status
TESTS_TOTAL=$((TESTS_TOTAL + 1))
if run_test "Coordinator Status" "curl -s http://localhost:$COORDINATOR_PORT/status | jq -e '.status == \"healthy\"' > /dev/null"; then
    TESTS_PASSED=$((TESTS_PASSED + 1))
fi

# Test 2: Check node health
for i in {1..3}; do
    port=$((8079 + i))
    TESTS_TOTAL=$((TESTS_TOTAL + 1))
    if run_test "Node $i Health" "curl -s http://localhost:$port/health | jq -e '.status == \"healthy\"' > /dev/null"; then
        TESTS_PASSED=$((TESTS_PASSED + 1))
    fi
done

# Test 3: Run application with LD_PRELOAD on different nodes
echo -e "${BLUE}Testing LD_PRELOAD injection on different nodes...${NC}"

# Create a simple test application
cat > "$TEST_DIR/test_app.go" << 'EOF'
package main

import (
    "database/sql"
    "fmt"
    "log"
    "os"
    "time"
    _ "github.com/mattn/go-sqlite3"
)

func main() {
    fmt.Println("Testing DistSQLite injection...")
    
    db, err := sql.Open("sqlite3", "./test.db")
    if err != nil {
        log.Fatalf("Failed to open database: %v", err)
    }
    defer db.Close()
    
    // Create table
    _, err = db.Exec(`CREATE TABLE IF NOT EXISTS test_data (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        message TEXT,
        timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
        node_id TEXT
    )`)
    if err != nil {
        log.Fatalf("Failed to create table: %v", err)
    }
    
    // Insert test data with node identifier
    nodeID := "unknown"
    if len(os.Args) > 1 {
        nodeID = os.Args[1]
    }
    
    _, err = db.Exec(`INSERT INTO test_data (message, node_id) VALUES (?, ?)`, 
        "Hello from distributed SQLite!", nodeID)
    if err != nil {
        log.Fatalf("Failed to insert data: %v", err)
    }
    
    // Read back all data
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
    fmt.Println("Test completed successfully!")
}
EOF

# Test each node with LD_PRELOAD
for i in {1..3}; do
    TESTS_TOTAL=$((TESTS_TOTAL + 1))
    port=$((8079 + i))
    
    echo -e "${BLUE}Testing LD_PRELOAD on node $i (port $port)...${NC}"
    
    cd "$TEST_DIR/node$i"
    
    # Set environment to point to this node
    export DISTSQLITE_SIDECAR_ADDR="localhost:$port"
    
    # Run the test application with LD_PRELOAD
    if LD_PRELOAD="$BUILD_DIR/distsqlite-inject.so" \
       LD_LIBRARY_PATH="$BUILD_DIR" \
       go run ../test_app.go "node$i" > "test_output_$i.log" 2>&1; then
        echo -e "${GREEN}✓ PASS: LD_PRELOAD test on node $i${NC}"
        TESTS_PASSED=$((TESTS_PASSED + 1))
        
        # Show the output
        echo "Output from node $i:"
        cat "test_output_$i.log" | sed 's/^/    /'
    else
        echo -e "${RED}✗ FAIL: LD_PRELOAD test on node $i${NC}"
        echo "Error output:"
        cat "test_output_$i.log" | sed 's/^/    /'
    fi
    
    echo
done

# Test 4: Data consistency across nodes
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Testing data consistency across nodes...${NC}"

# Give time for replication
sleep 5

# Check if all nodes can see the data
all_consistent=true
for i in {1..3}; do
    port=$((8079 + i))
    
    # Query the node directly (bypass LD_PRELOAD)
    count=$(curl -s "http://localhost:$port/stats" | jq -r '.database.records // 0' 2>/dev/null || echo "0")
    
    echo "Node $i has $count records"
    
    if [ "$count" = "0" ]; then
        all_consistent=false
    fi
done

if $all_consistent; then
    echo -e "${GREEN}✓ PASS: Data consistency test${NC}"
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ FAIL: Data consistency test${NC}"
fi

# Test 5: Lease coordination
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Testing lease coordination...${NC}"

# Check which node has the primary lease
primary_found=false
primary_count=0

for i in {1..3}; do
    port=$((8079 + i))
    status=$(curl -s "http://localhost:$port/status" | jq -r '.role // "unknown"' 2>/dev/null || echo "unknown")
    echo "Node $i status: $status"
    
    if [ "$status" = "primary" ]; then
        primary_found=true
        primary_count=$((primary_count + 1))
    fi
done

if $primary_found && [ $primary_count -eq 1 ]; then
    echo -e "${GREEN}✓ PASS: Lease coordination test (exactly one primary)${NC}"
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ FAIL: Lease coordination test (found $primary_count primaries)${NC}"
fi

# Print summary
echo
echo -e "${BLUE}=== Test Summary ===${NC}"
echo "Tests passed: $TESTS_PASSED/$TESTS_TOTAL"

if [ $TESTS_PASSED -eq $TESTS_TOTAL ]; then
    echo -e "${GREEN}🎉 All tests passed! DistSQLite is working correctly with multiple nodes.${NC}"
    exit 0
else
    failed=$((TESTS_TOTAL - TESTS_PASSED))
    echo -e "${RED}❌ $failed test(s) failed.${NC}"
    exit 1
fi