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
    
    # Test ZMQ connectivity if zmq-test binary exists
    if [ -f "./build/zmq-test" ]; then
        timeout 10s ./build/zmq-test --endpoint tcp://localhost:5555 --mode benchmark --iterations 1000 > "$TEST_RESULTS_DIR/zmq-test.log" 2>&1 && echo "✅ ZMQ Transport OK" || echo "❌ ZMQ Transport FAILED"
    else
        echo "⚠️ ZMQ test binary not found - skipping ZMQ transport test"
    fi
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
    
    # Run Go benchmarks if available
    if [ -d "./test/benchmarks" ]; then
        go test -bench=. ./test/benchmarks/ > "$TEST_RESULTS_DIR/go-benchmarks.log" 2>&1 && echo "✅ Go benchmarks OK" || echo "❌ Go benchmarks FAILED"
    fi
    
    # Run OCaml benchmarks if available
    if [ -d "./bench" ]; then
        cd bench && dune exec ./bench_distcore.exe > "../$TEST_RESULTS_DIR/ocaml-benchmarks.log" 2>&1 && echo "✅ OCaml benchmarks OK" || echo "❌ OCaml benchmarks FAILED" && cd ..
    fi
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
            -addr ":8081" \
            -db "./local-cluster-data/coord1/coord.db" \
            -lease-port "8080" \
            > "./local-cluster-logs/coord1.log" 2>&1 &
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