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
        -port "$LEASE_SERVER_PORT" \
        -db "$DATA_DIR/leaseserver/lease.db" \
        > "$LOG_DIR/leaseserver.log" 2>&1 &
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
        -addr ":$http_port" \
        -db "$DATA_DIR/coord$coord_num/coord.db" \
        -lease-port "$((LEASE_SERVER_PORT + coord_num))" \
        > "$LOG_DIR/coord$coord_num.log" 2>&1 &
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
        --coordination "http" \
        --coordinator-url "http://localhost:$coordinator_http_port" \
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
    echo "🛑 Stop cluster with: ./setup_local_cluster.sh stop"
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