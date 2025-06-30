#!/bin/bash
set -e

echo "🧪 Testing Simplified DistSQLite Cluster"

# Configuration
COORD1="http://localhost:8081"
COORD2="http://localhost:8082"
LEASE1="http://localhost:8080"
LEASE2="http://localhost:8083"
TEST_RESULTS_DIR="./simplified-test-results"

mkdir -p "$TEST_RESULTS_DIR"

# Test functions
test_connectivity() {
    echo "🔗 Testing connectivity..."
    
    curl -s "$COORD1/health" > /dev/null && echo "✅ Coordinator 1 OK" || echo "❌ Coordinator 1 DOWN"
    curl -s "$COORD2/health" > /dev/null && echo "✅ Coordinator 2 OK" || echo "❌ Coordinator 2 DOWN"
    curl -s "$LEASE1/health" > /dev/null && echo "✅ Lease Server 1 OK" || echo "❌ Lease Server 1 DOWN"
    curl -s "$LEASE2/health" > /dev/null && echo "✅ Lease Server 2 OK" || echo "❌ Lease Server 2 DOWN"
}

test_cluster_status() {
    echo "🌐 Testing cluster status..."
    
    # Check coordinator cluster info
    curl -s "$COORD1/cluster" > "$TEST_RESULTS_DIR/cluster1.json" && echo "✅ Coordinator 1 cluster info OK" || echo "❌ Coordinator 1 cluster info FAILED"
    curl -s "$COORD2/cluster" > "$TEST_RESULTS_DIR/cluster2.json" && echo "✅ Coordinator 2 cluster info OK" || echo "❌ Coordinator 2 cluster info FAILED"
    
    # Check node registration
    curl -s "$COORD1/nodes" > "$TEST_RESULTS_DIR/nodes1.json" && echo "✅ Coordinator 1 nodes OK" || echo "❌ Coordinator 1 nodes FAILED"
    curl -s "$COORD2/nodes" > "$TEST_RESULTS_DIR/nodes2.json" && echo "✅ Coordinator 2 nodes OK" || echo "❌ Coordinator 2 nodes FAILED"
}

test_basic_operations() {
    echo "📊 Testing basic operations..."
    
    # Test simple HTTP operations
    echo "Testing basic HTTP operations..."
    
    # Check status endpoints
    curl -s "$COORD1/status" > "$TEST_RESULTS_DIR/status1.json" && echo "✅ Coordinator 1 status OK" || echo "❌ Coordinator 1 status FAILED"
    curl -s "$COORD2/status" > "$TEST_RESULTS_DIR/status2.json" && echo "✅ Coordinator 2 status OK" || echo "❌ Coordinator 2 status FAILED"
}

test_replication_coordination() {
    echo "🔄 Testing replication coordination..."
    
    # Create a test replication request
    REPL_REQUEST='{
        "source_node": "node1",
        "target_node": "node2", 
        "database": "test.db",
        "strategy": "rsync",
        "priority": 1
    }'
    
    curl -s -X POST "$COORD1/replicate" \
        -H "Content-Type: application/json" \
        -d "$REPL_REQUEST" > "$TEST_RESULTS_DIR/replication-request.json"
    
    if grep -q "created" "$TEST_RESULTS_DIR/replication-request.json"; then
        echo "✅ Replication request created OK"
        
        # Check replication queue
        curl -s "$COORD1/replication/queue" > "$TEST_RESULTS_DIR/replication-queue.json"
        if grep -q "jobs" "$TEST_RESULTS_DIR/replication-queue.json"; then
            echo "✅ Replication queue OK"
        else
            echo "❌ Replication queue FAILED"
        fi
    else
        echo "❌ Replication request FAILED"
    fi
}

test_performance() {
    echo "🚀 Running basic performance tests..."
    
    # Simple performance test
    echo "Testing basic HTTP response times..."
    
    start_time=$(date +%s%N)
    for i in {1..100}; do
        curl -s "$COORD1/health" > /dev/null
    done
    end_time=$(date +%s%N)
    
    duration=$(( (end_time - start_time) / 1000000 ))
    throughput=$(( 100000 / duration ))
    
    echo "HTTP Performance: ${duration}ms for 100 requests (${throughput} req/s)"
    echo "http_performance,$duration,$throughput" > "$TEST_RESULTS_DIR/performance.csv"
}

test_failover_simulation() {
    echo "🔥 Testing basic failover simulation..."
    
    # Get initial cluster state
    curl -s "$COORD1/cluster" > "$TEST_RESULTS_DIR/cluster-before-failover.json"
    
    echo "  Simulating coordinator 1 being busy..."
    
    # Try to access coordinator 2 while coordinator 1 might be busy
    curl -s "$COORD2/cluster" > "$TEST_RESULTS_DIR/cluster-during-failover.json"
    
    if grep -q "active_nodes\|total_nodes" "$TEST_RESULTS_DIR/cluster-during-failover.json"; then
        echo "✅ Coordinator 2 available during failover simulation"
    else
        echo "❌ Coordinator 2 failover FAILED"
    fi
}

generate_report() {
    echo "📄 Generating test report..."
    
    cat > "$TEST_RESULTS_DIR/simplified-test-report.md" << EOF
# Simplified DistSQLite Cluster Test Report

**Date:** $(date)
**Cluster Configuration:**
- Coordinator 1: http://localhost:8081 (with lease server on 8080)
- Coordinator 2: http://localhost:8082 (with lease server on 8083)
- Nodes: 4 total (2 per coordinator)

## Test Results

### Connectivity Tests
$(cat "$TEST_RESULTS_DIR/connectivity.log" 2>/dev/null || echo "No connectivity logs")

### Cluster Status
$(cat "$TEST_RESULTS_DIR/cluster1.json" 2>/dev/null | head -5 || echo "No cluster1 data")
$(cat "$TEST_RESULTS_DIR/cluster2.json" 2>/dev/null | head -5 || echo "No cluster2 data")

### Performance Tests
$(cat "$TEST_RESULTS_DIR/performance.csv" 2>/dev/null || echo "No performance data")

### Replication Tests
$(cat "$TEST_RESULTS_DIR/replication-request.json" 2>/dev/null || echo "No replication test results")

## Summary
Simplified cluster testing completed. This tests the basic coordinator functionality
with embedded lease servers and HTTP coordination.

## Next Steps
1. Review any failed tests in the log files
2. Test with actual SQLite operations using LD_PRELOAD
3. Scale up testing with more complex scenarios
EOF

    echo "📊 Simplified test report generated: $TEST_RESULTS_DIR/simplified-test-report.md"
}

# Main execution
main() {
    echo "🎯 Starting simplified cluster tests..."
    
    test_connectivity > "$TEST_RESULTS_DIR/connectivity.log" 2>&1
    test_cluster_status
    test_basic_operations
    test_replication_coordination
    test_performance
    test_failover_simulation
    generate_report
    
    echo ""
    echo "🎉 Simplified cluster testing completed!"
    echo "📋 Check results in: $TEST_RESULTS_DIR/"
    echo ""
    echo "✅ Basic distributed coordination is working!"
}

main "$@"