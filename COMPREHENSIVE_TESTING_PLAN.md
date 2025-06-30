# DistSQLite Comprehensive Testing Plan

## Overview

This plan provides comprehensive testing strategies for DistSQLite deployment and validation:
1. **Fly.io Multi-Region Deployment Testing** - Production-like distributed testing
2. **Local Multi-Node Testing** - Development and validation testing

## 📋 Part 1: Fly.io Deployment Testing Plan

### Pre-Deployment Validation

```bash
# 1. Run comprehensive local validation
./test/validation_suite.sh

# 2. Quick validation (faster iteration)
./test/validation_suite.sh quick

# 3. Performance validation only
./test/validation_suite.sh performance
```

### Phase 1: Infrastructure Deployment

```bash
# Deploy complete multi-region setup
./deploy/fly-deploy.sh deploy
```

**What this deploys:**
- **Lease Server** (ORD) - ZMQ-first coordination on port 5558
- **Coordinators** (ORD, IAD, LAX, FRA) - ZMQ transport on ports 5555-5558
- **Nodes** (2 per region, 8 total) - SQLite operations
- **Test Runner** (ORD) - Orchestrates distributed testing

### Phase 2: Connectivity & Health Validation

```bash
# Check deployment status
./deploy/fly-deploy.sh status

# Manual health checks
curl https://distsqlite-test-leaseserver.fly.dev/health
curl https://distsqlite-test-coordinator-ord.fly.dev/health/zmq
curl https://distsqlite-test-coordinator-iad.fly.dev/health/zmq
curl https://distsqlite-test-coordinator-lax.fly.dev/health/zmq
curl https://distsqlite-test-coordinator-fra.fly.dev/health/zmq
```

### Phase 3: Distributed Testing Suite

```bash
# Run complete distributed test suite
./deploy/fly-deploy.sh test
```

**Tests executed:**
- **ZMQ vs HTTP Performance** - Validates 3-5x speedup
- **ZMQ vs HTTP Lease Performance** - Validates 5-10x speedup
- **Cross-Region Replication** - Tests data consistency
- **Split-Brain Prevention** - Network partition simulation
- **Concurrent Operations** - Multi-writer validation
- **Performance Benchmarks** - 10-100x faster than baseline

### Phase 4: Advanced Testing Scenarios

```bash
# SSH into test runner for custom tests
flyctl ssh console --app distsqlite-test-tests

# Run specific test scenarios
cd /app

# Test specific components
go run test/integration/zmq_transport_test.go
go run test/integration/split_brain_test.go
go run test/integration/concurrent_test.go
go run test/integration/fallback_test.go

# Performance benchmarking
go test -bench=. ./test/benchmarks/

# Stress testing
go test -v ./test/integration -run TestConcurrent -count=10
```

### Phase 5: Monitoring & Debugging

```bash
# View real-time logs
flyctl logs --app distsqlite-test-leaseserver
flyctl logs --app distsqlite-test-coordinator-ord
flyctl logs --app distsqlite-test-tests

# SSH into any component for debugging
flyctl ssh console --app distsqlite-test-coordinator-ord
flyctl ssh console --app distsqlite-test-node-ord-1

# Check ZMQ connectivity manually
/app/bin/zmq-test --endpoint coordinator.internal:5555
/app/bin/zmq-test --endpoint leaseserver.internal:5558 --mode lease-test
```

### Phase 6: Performance Validation

Expected results based on "10-100x faster than Turso":
- **ZMQ Transport**: 3-5x faster than HTTP
- **ZMQ Leases**: 5-10x faster than HTTP
- **Small datasets**: 10x faster than baseline
- **Medium datasets**: 50x faster than baseline
- **Large datasets**: 100x faster than baseline
- **Cross-region latency**: <100ms replication
- **Lease failover**: <500ms with ZMQ

### Phase 7: Cleanup

```bash
# Clean up all deployed resources
./deploy/fly-deploy.sh cleanup
```

---

## 🏠 Part 2: Local Multi-Node Testing Setup

### Local Architecture

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   Lease Server  │    │   Coordinator   │    │   Coordinator   │
│   ZMQ:5558      │    │   ZMQ:5555      │    │   ZMQ:5556      │
│   HTTP:8080     │    │   HTTP:8080     │    │   HTTP:8081     │
│   (Primary)     │    │   (Region 1)    │    │   (Region 2)    │
└─────────────────┘    └─────────────────┘    └─────────────────┘
         │                       │                       │
         │              ┌────────┼────────┐             │
         │              │                 │             │
    ┌─────────────┐ ┌─────────────┐ ┌─────────────┐ ┌─────────────┐
    │   Node 1    │ │   Node 2    │ │   Node 3    │ │   Node 4    │
    │   (Reg 1)   │ │   (Reg 1)   │ │   (Reg 2)   │ │   (Reg 2)   │
    └─────────────┘ └─────────────┘ └─────────────┘ └─────────────┘
```

### Setup Script

Create `setup_local_cluster.sh`:

```bash
#!/bin/bash
set -e

echo "🏠 Setting up local DistSQLite cluster"

# Configuration
LEASE_SERVER_PORT=8080
LEASE_SERVER_ZMQ_PORT=5558
COORD1_HTTP_PORT=8081
COORD1_ZMQ_PORT=5555
COORD2_HTTP_PORT=8082
COORD2_ZMQ_PORT=5556
DATA_DIR="./local-cluster-data"
LOG_DIR="./local-cluster-logs"

# Cleanup previous setup
cleanup() {
    echo "🧹 Cleaning up previous cluster..."
    pkill -f distsqlite || true
    rm -rf "$DATA_DIR" "$LOG_DIR"
    make ipc-clean
    sleep 2
}

# Create directories
setup_dirs() {
    mkdir -p "$DATA_DIR"/{leaseserver,coord1,coord2,node1,node2,node3,node4}
    mkdir -p "$LOG_DIR"
}

# Build if needed
build_if_needed() {
    if [ ! -f "build/distsqlite-leaseserver" ] || [ ! -f "build/distsqlite-coordinator" ] || [ ! -f "build/distsqlite" ]; then
        echo "🔨 Building DistSQLite..."
        make build
    fi
}

# Start lease server
start_lease_server() {
    echo "🔐 Starting Lease Server (HTTP:$LEASE_SERVER_PORT, ZMQ:$LEASE_SERVER_ZMQ_PORT)..."
    ./build/distsqlite-leaseserver \
        --http-addr ":$LEASE_SERVER_PORT" \
        --zmq-addr ":$LEASE_SERVER_ZMQ_PORT" \
        --data-dir "$DATA_DIR/leaseserver" \
        --log-file "$LOG_DIR/leaseserver.log" &
    echo $! > "$LOG_DIR/leaseserver.pid"
    sleep 2
}

# Start coordinators
start_coordinator() {
    local coord_num=$1
    local http_port=$2
    local zmq_port=$3
    
    echo "🎯 Starting Coordinator $coord_num (HTTP:$http_port, ZMQ:$zmq_port)..."
    ./build/distsqlite-coordinator \
        --http-addr ":$http_port" \
        --zmq-addr ":$zmq_port" \
        --lease-server-http "http://localhost:$LEASE_SERVER_PORT" \
        --lease-server-zmq "tcp://localhost:$LEASE_SERVER_ZMQ_PORT" \
        --data-dir "$DATA_DIR/coord$coord_num" \
        --log-file "$LOG_DIR/coord$coord_num.log" &
    echo $! > "$LOG_DIR/coord$coord_num.pid"
    sleep 1
}

# Start nodes
start_node() {
    local node_num=$1
    local coordinator_http_port=$2
    local coordinator_zmq_port=$3
    
    echo "🌐 Starting Node $node_num (connecting to coordinator on $coordinator_http_port/$coordinator_zmq_port)..."
    ./build/distsqlite \
        --coordinator-http "http://localhost:$coordinator_http_port" \
        --coordinator-zmq "tcp://localhost:$coordinator_zmq_port" \
        --data-dir "$DATA_DIR/node$node_num" \
        --log-file "$LOG_DIR/node$node_num.log" &
    echo $! > "$LOG_DIR/node$node_num.pid"
    sleep 1
}

# Wait for services
wait_for_service() {
    local service=$1
    local port=$2
    local timeout=${3:-30}
    
    echo "⏳ Waiting for $service on port $port..."
    for i in $(seq 1 $timeout); do
        if nc -z localhost "$port" 2>/dev/null; then
            echo "✅ $service is ready"
            return 0
        fi
        sleep 1
    done
    echo "❌ $service failed to start"
    return 1
}

# Main setup function
setup_cluster() {
    cleanup
    setup_dirs
    build_if_needed
    
    # Start services
    start_lease_server
    wait_for_service "Lease Server" $LEASE_SERVER_PORT
    
    start_coordinator 1 $COORD1_HTTP_PORT $COORD1_ZMQ_PORT
    start_coordinator 2 $COORD2_HTTP_PORT $COORD2_ZMQ_PORT
    wait_for_service "Coordinator 1" $COORD1_HTTP_PORT
    wait_for_service "Coordinator 2" $COORD2_HTTP_PORT
    
    # Start nodes (2 per coordinator)
    start_node 1 $COORD1_HTTP_PORT $COORD1_ZMQ_PORT
    start_node 2 $COORD1_HTTP_PORT $COORD1_ZMQ_PORT
    start_node 3 $COORD2_HTTP_PORT $COORD2_ZMQ_PORT
    start_node 4 $COORD2_HTTP_PORT $COORD2_ZMQ_PORT
    
    echo ""
    echo "🎉 Local DistSQLite cluster is running!"
    echo ""
    echo "📊 Service URLs:"
    echo "  Lease Server: http://localhost:$LEASE_SERVER_PORT/health"
    echo "  Coordinator 1: http://localhost:$COORD1_HTTP_PORT/health"
    echo "  Coordinator 2: http://localhost:$COORD2_HTTP_PORT/health"
    echo ""
    echo "📁 Data directories: $DATA_DIR/"
    echo "📝 Log files: $LOG_DIR/"
    echo ""
    echo "🧪 Run tests with: ./test_local_cluster.sh"
    echo "🛑 Stop cluster with: ./stop_local_cluster.sh"
}

# Command handling
case "${1:-start}" in
    "start")
        setup_cluster
        ;;
    "stop")
        cleanup
        echo "✅ Local cluster stopped"
        ;;
    "status")
        echo "📊 Local Cluster Status:"
        echo "Lease Server: $(curl -s http://localhost:$LEASE_SERVER_PORT/health || echo 'DOWN')"
        echo "Coordinator 1: $(curl -s http://localhost:$COORD1_HTTP_PORT/health || echo 'DOWN')"
        echo "Coordinator 2: $(curl -s http://localhost:$COORD2_HTTP_PORT/health || echo 'DOWN')"
        ;;
    "logs")
        echo "📝 Recent log entries:"
        tail -10 "$LOG_DIR"/*.log
        ;;
    *)
        echo "Usage: $0 [start|stop|status|logs]"
        exit 1
        ;;
esac
```

### Local Testing Suite

Create `test_local_cluster.sh`:

```bash
#!/bin/bash
set -e

echo "🧪 Local DistSQLite Cluster Testing"

# Configuration
LEASE_SERVER="http://localhost:8080"
COORD1="http://localhost:8081"
COORD2="http://localhost:8082"
TEST_RESULTS_DIR="./local-test-results"

mkdir -p "$TEST_RESULTS_DIR"

# Test functions
test_connectivity() {
    echo "🔗 Testing connectivity..."
    
    curl -s "$LEASE_SERVER/health" > /dev/null && echo "✅ Lease Server OK" || echo "❌ Lease Server DOWN"
    curl -s "$COORD1/health" > /dev/null && echo "✅ Coordinator 1 OK" || echo "❌ Coordinator 1 DOWN"
    curl -s "$COORD2/health" > /dev/null && echo "✅ Coordinator 2 OK" || echo "❌ Coordinator 2 DOWN"
}

test_zmq_transport() {
    echo "⚡ Testing ZMQ transport..."
    
    # Test ZMQ connectivity
    timeout 10s ./build/zmq-test --endpoint tcp://localhost:5555 --mode benchmark --iterations 1000 > "$TEST_RESULTS_DIR/zmq-test.log" 2>&1 && echo "✅ ZMQ Transport OK" || echo "❌ ZMQ Transport FAILED"
}

test_lease_operations() {
    echo "🔐 Testing lease operations..."
    
    # Acquire lease
    LEASE_ID="test-lease-$(date +%s)"
    curl -s -X POST "$LEASE_SERVER/lease/acquire" -d "{\"id\":\"$LEASE_ID\",\"ttl\":\"30s\"}" > "$TEST_RESULTS_DIR/lease-acquire.json"
    
    if grep -q "acquired" "$TEST_RESULTS_DIR/lease-acquire.json"; then
        echo "✅ Lease acquisition OK"
        
        # Release lease
        curl -s -X POST "$LEASE_SERVER/lease/release" -d "{\"id\":\"$LEASE_ID\"}" > "$TEST_RESULTS_DIR/lease-release.json"
        
        if grep -q "released" "$TEST_RESULTS_DIR/lease-release.json"; then
            echo "✅ Lease release OK"
        else
            echo "❌ Lease release FAILED"
        fi
    else
        echo "❌ Lease acquisition FAILED"
    fi
}

test_data_operations() {
    echo "📊 Testing data operations..."
    
    # Create test table
    curl -s -X POST "$COORD1/sql" -H "Content-Type: application/json" -d '{"sql": "CREATE TABLE IF NOT EXISTS test_local (id INTEGER PRIMARY KEY, data TEXT, timestamp DATETIME DEFAULT CURRENT_TIMESTAMP)"}' > "$TEST_RESULTS_DIR/create-table.json"
    
    # Insert test data
    curl -s -X POST "$COORD1/sql" -H "Content-Type: application/json" -d '{"sql": "INSERT INTO test_local (data) VALUES (\"test data 1\"), (\"test data 2\"), (\"test data 3\")"}' > "$TEST_RESULTS_DIR/insert-data.json"
    
    # Query data from coordinator 1
    curl -s -X POST "$COORD1/sql" -H "Content-Type: application/json" -d '{"sql": "SELECT COUNT(*) FROM test_local"}' > "$TEST_RESULTS_DIR/query-coord1.json"
    
    # Wait for replication
    sleep 3
    
    # Query data from coordinator 2
    curl -s -X POST "$COORD2/sql" -H "Content-Type: application/json" -d '{"sql": "SELECT COUNT(*) FROM test_local"}' > "$TEST_RESULTS_DIR/query-coord2.json"
    
    # Check if data replicated
    if grep -q "3" "$TEST_RESULTS_DIR/query-coord1.json" && grep -q "3" "$TEST_RESULTS_DIR/query-coord2.json"; then
        echo "✅ Data replication OK"
    else
        echo "❌ Data replication FAILED"
    fi
}

test_performance() {
    echo "🚀 Running performance tests..."
    
    # Run Go benchmarks
    go test -bench=. ./test/benchmarks/ > "$TEST_RESULTS_DIR/go-benchmarks.log" 2>&1 && echo "✅ Go benchmarks OK" || echo "❌ Go benchmarks FAILED"
    
    # Run OCaml benchmarks
    cd bench && dune exec ./bench_distcore.exe > "../$TEST_RESULTS_DIR/ocaml-benchmarks.log" 2>&1 && echo "✅ OCaml benchmarks OK" || echo "❌ OCaml benchmarks FAILED" && cd ..
}

test_failure_scenarios() {
    echo "🔥 Testing failure scenarios..."
    
    # Test coordinator failover
    echo "  Testing coordinator failover..."
    
    # Stop coordinator 1
    if [ -f "./local-cluster-logs/coord1.pid" ]; then
        kill "$(cat ./local-cluster-logs/coord1.pid)" || true
        sleep 2
        
        # Try to query via coordinator 2 (should still work)
        curl -s -X POST "$COORD2/sql" -H "Content-Type: application/json" -d '{"sql": "SELECT COUNT(*) FROM test_local"}' > "$TEST_RESULTS_DIR/failover-test.json"
        
        if grep -q "3" "$TEST_RESULTS_DIR/failover-test.json"; then
            echo "✅ Coordinator failover OK"
        else
            echo "❌ Coordinator failover FAILED"
        fi
        
        # Restart coordinator 1
        ./build/distsqlite-coordinator \
            --http-addr ":8081" \
            --zmq-addr ":5555" \
            --lease-server-http "http://localhost:8080" \
            --lease-server-zmq "tcp://localhost:5558" \
            --data-dir "./local-cluster-data/coord1" \
            --log-file "./local-cluster-logs/coord1.log" &
        echo $! > "./local-cluster-logs/coord1.pid"
        sleep 2
    fi
}

test_concurrent_operations() {
    echo "🔄 Testing concurrent operations..."
    
    # Run concurrent writes
    for i in {1..5}; do
        curl -s -X POST "$COORD1/sql" -H "Content-Type: application/json" -d "{\"sql\": \"INSERT INTO test_local (data) VALUES ('concurrent-$i')\"}" > "$TEST_RESULTS_DIR/concurrent-$i.json" &
    done
    
    wait
    
    # Check final count
    sleep 2
    curl -s -X POST "$COORD1/sql" -H "Content-Type: application/json" -d '{"sql": "SELECT COUNT(*) FROM test_local"}' > "$TEST_RESULTS_DIR/final-count.json"
    
    if grep -q "8" "$TEST_RESULTS_DIR/final-count.json"; then
        echo "✅ Concurrent operations OK"
    else
        echo "❌ Concurrent operations FAILED"
    fi
}

generate_report() {
    echo "📄 Generating test report..."
    
    cat > "$TEST_RESULTS_DIR/local-test-report.md" << EOF
# Local DistSQLite Cluster Test Report

**Date:** $(date)
**Cluster Configuration:**
- Lease Server: http://localhost:8080 (ZMQ: 5558)
- Coordinator 1: http://localhost:8081 (ZMQ: 5555)
- Coordinator 2: http://localhost:8082 (ZMQ: 5556)
- Nodes: 4 total (2 per coordinator)

## Test Results

### Connectivity Tests
$(cat "$TEST_RESULTS_DIR/connectivity.log" 2>/dev/null || echo "No connectivity logs")

### Performance Tests
$(cat "$TEST_RESULTS_DIR/go-benchmarks.log" 2>/dev/null | tail -20 || echo "No Go benchmark results")
$(cat "$TEST_RESULTS_DIR/ocaml-benchmarks.log" 2>/dev/null | tail -20 || echo "No OCaml benchmark results")

### Failure Scenarios
$(cat "$TEST_RESULTS_DIR/failover-test.json" 2>/dev/null || echo "No failover test results")

## Summary
Local cluster testing completed. Check individual result files for detailed information.

## Next Steps
1. Review any failed tests in the log files
2. Run performance comparison with baseline
3. Test with real applications using LD_PRELOAD
4. Proceed to Fly.io deployment if all tests pass
EOF

    echo "📊 Test report generated: $TEST_RESULTS_DIR/local-test-report.md"
}

# Main execution
main() {
    echo "🎯 Starting comprehensive local cluster tests..."
    
    test_connectivity > "$TEST_RESULTS_DIR/connectivity.log" 2>&1
    test_zmq_transport
    test_lease_operations
    test_data_operations
    test_performance
    test_failure_scenarios
    test_concurrent_operations
    generate_report
    
    echo ""
    echo "🎉 Local cluster testing completed!"
    echo "📋 Check results in: $TEST_RESULTS_DIR/"
}

main "$@"
```

### Application Testing

Create `test_application_integration.sh`:

```bash
#!/bin/bash
set -e

echo "🔗 Testing Application Integration"

# Test with LD_PRELOAD injection
test_ld_preload() {
    echo "📚 Testing LD_PRELOAD integration..."
    
    cd test/integration
    LD_PRELOAD=../../build/distsqlite-inject.so LD_LIBRARY_PATH=../../build go run simple_demo.go
    cd ../..
}

# Test with multiple applications
test_multiple_apps() {
    echo "🔄 Testing multiple applications..."
    
    # Start 3 applications simultaneously
    for i in {1..3}; do
        cd test/integration
        LD_PRELOAD=../../build/distsqlite-inject.so LD_LIBRARY_PATH=../../build go run simple_demo.go &
        cd ../..
    done
    
    wait
    echo "✅ Multiple applications completed"
}

# Test with real SQLite operations
test_sqlite_operations() {
    echo "🗃️ Testing SQLite operations..."
    
    # Create test program
    cat > test_app.c << 'EOF'
#include <sqlite3.h>
#include <stdio.h>
#include <stdlib.h>

int main() {
    sqlite3 *db;
    char *err_msg = 0;
    
    int rc = sqlite3_open("test_distributed.db", &db);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "Cannot open database: %s\n", sqlite3_errmsg(db));
        sqlite3_close(db);
        return 1;
    }
    
    // Create table
    char *sql = "CREATE TABLE IF NOT EXISTS test_app (id INTEGER PRIMARY KEY, name TEXT);";
    rc = sqlite3_exec(db, sql, 0, 0, &err_msg);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "SQL error: %s\n", err_msg);
        sqlite3_free(err_msg);
        sqlite3_close(db);
        return 1;
    }
    
    // Insert data
    sql = "INSERT INTO test_app (name) VALUES ('Test Application');";
    rc = sqlite3_exec(db, sql, 0, 0, &err_msg);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "SQL error: %s\n", err_msg);
        sqlite3_free(err_msg);
        sqlite3_close(db);
        return 1;
    }
    
    printf("✅ SQLite operations successful\n");
    sqlite3_close(db);
    return 0;
}
EOF
    
    # Compile and run with LD_PRELOAD
    gcc -o test_app test_app.c -lsqlite3
    LD_PRELOAD=./build/distsqlite-inject.so LD_LIBRARY_PATH=./build ./test_app
    rm test_app test_app.c
}

test_ld_preload
test_multiple_apps
test_sqlite_operations
```

## Quick Start Commands

### For Fly.io Testing:
```bash
# 1. Validate locally first
./test/validation_suite.sh

# 2. Deploy to Fly
./deploy/fly-deploy.sh deploy

# 3. Run distributed tests
./deploy/fly-deploy.sh test

# 4. Monitor and debug
./deploy/fly-deploy.sh status
flyctl logs --app distsqlite-test-tests

# 5. Cleanup
./deploy/fly-deploy.sh cleanup
```

### For Local Testing:
```bash
# 1. Set up local cluster
chmod +x setup_local_cluster.sh
./setup_local_cluster.sh start

# 2. Run tests
chmod +x test_local_cluster.sh
./test_local_cluster.sh

# 3. Test applications
chmod +x test_application_integration.sh
./test_application_integration.sh

# 4. Stop cluster
./setup_local_cluster.sh stop
```

## Success Criteria

### Fly.io Deployment
- ✅ All health checks pass
- ✅ ZMQ transport 3-5x faster than HTTP
- ✅ Cross-region replication <100ms
- ✅ Split-brain prevention works
- ✅ Lease failover <500ms
- ✅ Performance meets 10-100x targets

### Local Testing
- ✅ All 4 nodes connect successfully
- ✅ Data replication between coordinators
- ✅ Lease operations work correctly
- ✅ Applications work with LD_PRELOAD
- ✅ Concurrent operations handle correctly
- ✅ Failover scenarios work

This comprehensive testing plan leverages your existing excellent infrastructure and provides thorough validation for both production deployment and local development scenarios.