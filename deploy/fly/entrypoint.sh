#!/bin/bash
set -e

# Fly.io environment detection
FLY_REGION=${FLY_REGION:-"ord"}
FLY_APP_NAME=${FLY_APP_NAME:-"distsqlite-test"}
DISTSQLITE_ROLE=${DISTSQLITE_ROLE:-"node"}

echo "Starting DistSQLite in Fly.io region: $FLY_REGION"
echo "Role: $DISTSQLITE_ROLE"

# Configure based on role
case "$DISTSQLITE_ROLE" in
    "coordinator")
        echo "Starting coordinator mode..."
        
        # Start Consul in background if not using external consul
        if [ -z "$CONSUL_HTTP_ADDR" ]; then
            echo "Starting local Consul..."
            consul agent -config-dir=/app/consul -data-dir=/app/data/consul &
            CONSUL_PID=$!
            sleep 5
        fi
        
        # Start coordinator
        exec /app/bin/distsqlite-coordinator \
            --config /app/config/fly-coordinator.yml \
            --region $FLY_REGION \
            --app-name $FLY_APP_NAME
        ;;
        
    "node")
        echo "Starting node mode..."
        
        # Wait for coordinator to be available
        echo "Waiting for coordinator..."
        while ! nc -z coordinator.internal 5555; do
            sleep 1
        done
        
        # Start node
        exec /app/bin/distsqlite \
            --config /app/config/fly-node.yml \
            --region $FLY_REGION \
            --coordinator coordinator.internal:5555
        ;;
        
    "leaseserver")
        echo "Starting lease server mode..."
        
        exec /app/bin/distsqlite-leaseserver \
            --config /app/config/fly-leaseserver.yml \
            --bind 0.0.0.0:8080
        ;;
        
    "test")
        echo "Starting test mode..."
        
        # Wait for all services
        echo "Waiting for services..."
        while ! nc -z coordinator.internal 5555; do sleep 1; done
        while ! nc -z leaseserver.internal 8080; do sleep 1; done
        
        # Run test suite
        cd /app
        exec ./test/run_distributed_tests.sh
        ;;
        
    *)
        echo "Unknown role: $DISTSQLITE_ROLE"
        exit 1
        ;;
esac