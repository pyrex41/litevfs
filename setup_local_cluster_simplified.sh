#!/bin/bash
set -e

echo "🏠 Setting up simplified local DistSQLite cluster"

# Configuration
COORD1_HTTP_PORT=8081
COORD1_LEASE_PORT=8080
COORD2_HTTP_PORT=8082
COORD2_LEASE_PORT=8083
DATA_DIR="./local-cluster-data"
LOG_DIR="./local-cluster-logs"

# Cleanup previous setup
cleanup() {
    echo "🧹 Cleaning up previous cluster..."
    pkill -f distsqlite || true
    rm -rf "$DATA_DIR" "$LOG_DIR"
    make ipc-clean 2>/dev/null || true
    sleep 2
}

# Create directories
setup_dirs() {
    mkdir -p "$DATA_DIR"/{coord1,coord2,node1,node2,node3,node4}
    mkdir -p "$LOG_DIR"
}

# Build if needed
build_if_needed() {
    if [ ! -f "build/distsqlite-coordinator" ] || [ ! -f "build/distsqlite" ]; then
        echo "🔨 Building DistSQLite..."
        make build
    fi
}

# Start coordinators with embedded lease servers
start_coordinator() {
    local coord_num=$1
    local http_port=$2
    local lease_port=$3
    
    echo "🎯 Starting Coordinator $coord_num (HTTP:$http_port, Lease:$lease_port)..."
    ./build/distsqlite-coordinator \
        -addr ":$http_port" \
        -db "$DATA_DIR/coord$coord_num/coord.db" \
        -lease-port "$lease_port" \
        > "$LOG_DIR/coord$coord_num.log" 2>&1 &
    echo $! > "$LOG_DIR/coord$coord_num.pid"
    sleep 2
}

# Start nodes
start_node() {
    local node_num=$1
    local coordinator_url=$2
    
    echo "🌐 Starting Node $node_num (connecting to $coordinator_url)..."
    ./build/distsqlite \
        --coordination "http" \
        --coordinator-url "$coordinator_url" \
        --data-dir "$DATA_DIR/node$node_num" \
        --hostname "node$node_num" \
        --http-addr ":$((8090 + node_num))" \
        > "$LOG_DIR/node$node_num.log" 2>&1 &
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
    
    # Start coordinators with embedded lease servers
    start_coordinator 1 $COORD1_HTTP_PORT $COORD1_LEASE_PORT
    start_coordinator 2 $COORD2_HTTP_PORT $COORD2_LEASE_PORT
    
    wait_for_service "Coordinator 1" $COORD1_HTTP_PORT
    wait_for_service "Coordinator 2" $COORD2_HTTP_PORT
    
    # Start nodes (2 per coordinator)
    start_node 1 "http://localhost:$COORD1_HTTP_PORT"
    start_node 2 "http://localhost:$COORD1_HTTP_PORT"
    start_node 3 "http://localhost:$COORD2_HTTP_PORT"
    start_node 4 "http://localhost:$COORD2_HTTP_PORT"
    
    echo ""
    echo "🎉 Simplified DistSQLite cluster is running!"
    echo ""
    echo "📊 Service URLs:"
    echo "  Coordinator 1: http://localhost:$COORD1_HTTP_PORT/health"
    echo "  Coordinator 2: http://localhost:$COORD2_HTTP_PORT/health"
    echo "  Lease Server 1: http://localhost:$COORD1_LEASE_PORT/health"
    echo "  Lease Server 2: http://localhost:$COORD2_LEASE_PORT/health"
    echo ""
    echo "📁 Data directories: $DATA_DIR/"
    echo "📝 Log files: $LOG_DIR/"
    echo ""
    echo "🧪 Run tests with: ./test_simplified_cluster.sh"
    echo "🛑 Stop cluster with: ./setup_local_cluster_simplified.sh stop"
}

# Command handling
case "${1:-start}" in
    "start")
        setup_cluster
        ;;
    "stop")
        cleanup
        echo "✅ Simplified cluster stopped"
        ;;
    "status")
        echo "📊 Simplified Cluster Status:"
        echo "Coordinator 1: $(curl -s http://localhost:$COORD1_HTTP_PORT/health || echo 'DOWN')"
        echo "Coordinator 2: $(curl -s http://localhost:$COORD2_HTTP_PORT/health || echo 'DOWN')"
        echo "Lease Server 1: $(curl -s http://localhost:$COORD1_LEASE_PORT/health || echo 'DOWN')"
        echo "Lease Server 2: $(curl -s http://localhost:$COORD2_LEASE_PORT/health || echo 'DOWN')"
        ;;
    "logs")
        echo "📝 Recent log entries:"
        tail -10 "$LOG_DIR"/*.log 2>/dev/null || echo "No logs available"
        ;;
    *)
        echo "Usage: $0 [start|stop|status|logs]"
        exit 1
        ;;
esac