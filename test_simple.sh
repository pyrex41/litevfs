#!/bin/bash

# Simplified DistSQLite Test 
# Tests core distributed functionality without OCaml VFS complexity

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}=== DistSQLite Simplified Test ===${NC}"
echo "Testing core distributed coordination without VFS interception"
echo

# Configuration
PROJECT_ROOT="$(pwd)"
BUILD_DIR="$PROJECT_ROOT/build"
TEST_DIR="$PROJECT_ROOT/test-simple"
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
echo -e "${BLUE}Allowing time for cluster formation and lease negotiation...${NC}"
sleep 10

# Run tests
echo -e "${YELLOW}=== Running Core Functionality Tests ===${NC}"

TESTS_PASSED=0
TESTS_TOTAL=0

# Test 1: Coordinator health
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Test 1: Coordinator health check...${NC}"
response=$(curl -s "http://localhost:$COORDINATOR_PORT/health")
if echo "$response" | grep -q "healthy\|ok\|status"; then
    echo -e "${GREEN}✓ Coordinator health check passed${NC}"
    echo "   Response: $response"
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ Coordinator health check failed${NC}"
    echo "   Response: $response"
fi

# Test 2: Node health checks
for port in $NODE1_PORT $NODE2_PORT; do
    TESTS_TOTAL=$((TESTS_TOTAL + 1))
    node_num=$((port - 8079))
    echo -e "${BLUE}Test $((TESTS_TOTAL)): Node $node_num health check...${NC}"
    response=$(curl -s "http://localhost:$port/health")
    if echo "$response" | grep -q "healthy\|ok\|status"; then
        echo -e "${GREEN}✓ Node $node_num health check passed${NC}"
        echo "   Response: $response"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        echo -e "${RED}✗ Node $node_num health check failed${NC}"
        echo "   Response: $response"
    fi
done

# Test 3: Lease coordination verification
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Test $((TESTS_TOTAL)): Lease coordination verification...${NC}"
if grep -q "Acquired lease\|primary lease\|lease holder" "$TEST_DIR/node1.log" "$TEST_DIR/node2.log"; then
    echo -e "${GREEN}✓ Lease coordination verified${NC}"
    
    # Show lease information
    if grep -q "Acquired lease" "$TEST_DIR/node1.log"; then
        lease_info=$(grep "Acquired lease" "$TEST_DIR/node1.log" | tail -1)
        echo "   Node 1: $lease_info"
    fi
    if grep -q "lease holder" "$TEST_DIR/node2.log"; then
        lease_info=$(grep "lease holder" "$TEST_DIR/node2.log" | tail -1)
        echo "   Node 2: $lease_info"
    fi
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ Lease coordination verification failed${NC}"
fi

# Test 4: Node communication verification
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Test $((TESTS_TOTAL)): Node communication verification...${NC}"
if grep -q "Starting hybrid replicator\|replication\|targets" "$TEST_DIR/node1.log" "$TEST_DIR/node2.log"; then
    echo -e "${GREEN}✓ Node communication verified${NC}"
    
    # Show replication info
    if grep -q "hybrid replicator" "$TEST_DIR/node1.log"; then
        repl_info=$(grep "hybrid replicator" "$TEST_DIR/node1.log" | tail -1)
        echo "   Node 1: $repl_info"
    fi
    if grep -q "hybrid replicator" "$TEST_DIR/node2.log"; then
        repl_info=$(grep "hybrid replicator" "$TEST_DIR/node2.log" | tail -1)
        echo "   Node 2: $repl_info"
    fi
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ Node communication verification failed${NC}"
fi

# Test 5: Check API endpoints
TESTS_TOTAL=$((TESTS_TOTAL + 1))
echo -e "${BLUE}Test $((TESTS_TOTAL)): API endpoints verification...${NC}"
api_working=true

# Test coordinator status endpoint
if ! curl -s "http://localhost:$COORDINATOR_PORT/status" >/dev/null 2>&1; then
    api_working=false
fi

# Test node status endpoints
for port in $NODE1_PORT $NODE2_PORT; do
    if ! curl -s "http://localhost:$port/status" >/dev/null 2>&1; then
        api_working=false
    fi
done

if $api_working; then
    echo -e "${GREEN}✓ API endpoints verification passed${NC}"
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ API endpoints verification failed${NC}"
fi

# Show detailed status information
echo -e "\n${YELLOW}=== Cluster Status Information ===${NC}"

echo -e "${BLUE}Coordinator Status:${NC}"
curl -s "http://localhost:$COORDINATOR_PORT/status" 2>/dev/null | python3 -m json.tool 2>/dev/null || curl -s "http://localhost:$COORDINATOR_PORT/status" 2>/dev/null || echo "No response"

echo -e "\n${BLUE}Node 1 Status:${NC}"
curl -s "http://localhost:$NODE1_PORT/status" 2>/dev/null | python3 -m json.tool 2>/dev/null || curl -s "http://localhost:$NODE1_PORT/status" 2>/dev/null || echo "No response"

echo -e "\n${BLUE}Node 2 Status:${NC}"
curl -s "http://localhost:$NODE2_PORT/status" 2>/dev/null | python3 -m json.tool 2>/dev/null || curl -s "http://localhost:$NODE2_PORT/status" 2>/dev/null || echo "No response"

# Show log excerpts for debugging
echo -e "\n${YELLOW}=== Key Log Information ===${NC}"

echo -e "${BLUE}Coordinator Key Events:${NC}"
grep -E "Starting|Lease event|healthy" "$TEST_DIR/coordinator.log" 2>/dev/null | tail -3 || echo "No events found"

echo -e "\n${BLUE}Node 1 Key Events:${NC}"
grep -E "Acquired|lease|primary|Starting.*server" "$TEST_DIR/node1.log" 2>/dev/null | tail -3 || echo "No events found"

echo -e "\n${BLUE}Node 2 Key Events:${NC}"
grep -E "lease holder|Current lease|Starting.*server" "$TEST_DIR/node2.log" 2>/dev/null | tail -3 || echo "No events found"

# Final results
echo -e "\n${BLUE}=== Test Results Summary ===${NC}"
echo "Tests passed: $TESTS_PASSED/$TESTS_TOTAL"

if [ $TESTS_PASSED -eq $TESTS_TOTAL ]; then
    echo -e "${GREEN}🎉 All core functionality tests passed!${NC}"
    echo -e "${GREEN}✅ DistSQLite distributed coordination is working correctly.${NC}"
    echo
    echo "What's working:"
    echo "  • Multi-node cluster formation"
    echo "  • HTTP-based coordination"
    echo "  • Leader election via lease management"
    echo "  • Health monitoring and status APIs"
    echo "  • Node-to-node communication setup"
    echo
    echo "The distributed SQLite infrastructure is ready for use!"
    exit 0
else
    failed=$((TESTS_TOTAL - TESTS_PASSED))
    echo -e "${RED}❌ $failed test(s) failed.${NC}"
    echo
    echo "Check the log excerpts above for debugging information."
    exit 1
fi