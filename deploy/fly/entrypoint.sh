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
        
        # Start coordinator
        exec /app/bin/distsqlite-coordinator \
            --config /app/config/fly-coordinator.yml \
            --region $FLY_REGION \
            --app-name $FLY_APP_NAME
        ;;
        
    "node")
        echo "Starting node mode..."
        
        # Wait for coordinator to be available with exponential backoff
        echo "Waiting for coordinator..."
        retry_count=0
        max_retries=30
        delay=1
        
        while ! nc -z coordinator.internal 5555; do
            if [ $retry_count -ge $max_retries ]; then
                echo "ERROR: Coordinator not available after $max_retries attempts"
                exit 1
            fi
            echo "Coordinator not ready, retrying in ${delay}s (attempt $((retry_count + 1))/$max_retries)"
            sleep $delay
            retry_count=$((retry_count + 1))
            # Exponential backoff with max delay of 8s
            delay=$((delay < 8 ? delay * 2 : 8))
        done
        echo "Coordinator is ready!"
        
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
        
        # Wait for all services with proper error handling
        echo "Waiting for services..."
        
        wait_for_service() {
            local service=$1
            local port=$2
            local retry_count=0
            local max_retries=30
            local delay=1
            
            echo "Waiting for $service:$port..."
            while ! nc -z "$service" "$port"; do
                if [ $retry_count -ge $max_retries ]; then
                    echo "ERROR: $service:$port not available after $max_retries attempts"
                    return 1
                fi
                echo "$service:$port not ready, retrying in ${delay}s (attempt $((retry_count + 1))/$max_retries)"
                sleep $delay
                retry_count=$((retry_count + 1))
                delay=$((delay < 8 ? delay * 2 : 8))
            done
            echo "$service:$port is ready!"
        }
        
        wait_for_service coordinator.internal 5555 || exit 1
        wait_for_service leaseserver.internal 8080 || exit 1
        
        # Run test suite
        cd /app
        exec ./test/run_distributed_tests.sh
        ;;
        
    *)
        echo "Unknown role: $DISTSQLITE_ROLE"
        exit 1
        ;;
esac