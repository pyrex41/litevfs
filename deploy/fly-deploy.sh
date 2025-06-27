#!/bin/bash
set -e

# DistSQLite Fly.io Multi-Region Deployment Script
# Deploys and tests DistSQLite across multiple Fly.io regions using ZMQ

echo "🚀 DistSQLite Fly.io Multi-Region Deployment"

# Configuration
APP_NAME="distsqlite-test"
REGIONS=("ord" "iad" "lax" "fra")  # Chicago, N.Virginia, Los Angeles, Frankfurt
PRIMARY_REGION="ord"
ZMQ_BASE_PORT=5555

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log() {
    echo -e "${BLUE}[$(date '+%H:%M:%S')]${NC} $1"
}

success() {
    echo -e "${GREEN}✅ $1${NC}"
}

warn() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

error() {
    echo -e "${RED}❌ $1${NC}"
    exit 1
}

# Check prerequisites
check_prereqs() {
    log "Checking prerequisites..."
    
    if ! command -v flyctl &> /dev/null; then
        error "flyctl CLI not found. Install from https://fly.io/docs/hands-on/install-flyctl/"
    fi
    
    if ! flyctl auth whoami &> /dev/null; then
        error "Not logged into Fly.io. Run: flyctl auth login"
    fi
    
    if ! command -v make &> /dev/null; then
        error "make not found. Please install build tools."
    fi
    
    success "Prerequisites check passed"
}

# Build the application
build_app() {
    log "Building DistSQLite..."
    
    # Ensure binaries are built
    make build || error "Build failed"
    
    # Check required binaries exist
    for binary in distsqlite distsqlite-coordinator distsqlite-leaseserver; do
        if [[ ! -f "./$binary" ]]; then
            error "Binary $binary not found. Build may have failed."
        fi
    done
    
    success "Build completed"
}

# Deploy lease server (single instance)
deploy_leaseserver() {
    log "Deploying lease server to primary region ($PRIMARY_REGION)..."
    
    # Create leaseserver app
    cat > fly-leaseserver.toml << EOF
app = "${APP_NAME}-leaseserver"
primary_region = "$PRIMARY_REGION"

[build]
  dockerfile = "Dockerfile.fly"

[env]
  DISTSQLITE_ROLE = "leaseserver"
  FLY_PRIMARY_REGION = "$PRIMARY_REGION"

# ZMQ lease coordination port
[[services]]
  internal_port = 5558
  protocol = "tcp"
  
  [[services.ports]]
    port = 5558

# HTTP API port
[[services]]
  internal_port = 8080
  protocol = "tcp"
  
  [[services.ports]]
    handlers = ["http"]
    port = 80
    
  [[services.ports]]
    handlers = ["tls", "http"] 
    port = 443

  [services.concurrency]
    hard_limit = 1000
    soft_limit = 800
    type = "connections"

[checks]
  [checks.lease_health]
    grace_period = "10s"
    interval = "30s"
    method = "get"
    path = "/health"
    port = 8080
    timeout = "10s"
    type = "http"
EOF

    flyctl deploy --config fly-leaseserver.toml --app "${APP_NAME}-leaseserver" || {
        warn "Creating new leaseserver app..."
        flyctl apps create "${APP_NAME}-leaseserver" --org personal
        flyctl deploy --config fly-leaseserver.toml --app "${APP_NAME}-leaseserver"
    }
    
    success "Lease server deployed"
}

# Deploy coordinator (one per region)
deploy_coordinators() {
    log "Deploying coordinators across regions..."
    
    for region in "${REGIONS[@]}"; do
        log "Deploying coordinator to $region..."
        
        # Create region-specific coordinator config
        cat > "fly-coordinator-${region}.toml" << EOF
app = "${APP_NAME}-coordinator-${region}"
primary_region = "$region"

[build]
  dockerfile = "Dockerfile.fly"

[env]
  DISTSQLITE_ROLE = "coordinator"
  FLY_REGION = "$region"
  FLY_PRIMARY_REGION = "$PRIMARY_REGION"
  LEASE_SERVER_URL = "http://${APP_NAME}-leaseserver.internal:8080"

[[services]]
  internal_port = 5555
  protocol = "tcp"
  
  [[services.ports]]
    port = 5555

[[services]]
  internal_port = 5556
  protocol = "tcp"
  
  [[services.ports]]
    port = 5556

[[services]]
  internal_port = 5557
  protocol = "tcp"
  
  [[services.ports]]
    port = 5557

[[services]]
  internal_port = 5558
  protocol = "tcp"
  
  [[services.ports]]
    port = 5558

[[services]]
  internal_port = 8080
  protocol = "tcp"
  
  [[services.ports]]
    handlers = ["http"]
    port = 80

[checks]
  [checks.zmq_health]
    grace_period = "15s"
    interval = "30s"
    method = "get"  
    path = "/health/zmq"
    port = 8080
    timeout = "10s"
    type = "http"
EOF

        flyctl deploy --config "fly-coordinator-${region}.toml" --app "${APP_NAME}-coordinator-${region}" || {
            warn "Creating new coordinator app for $region..."
            flyctl apps create "${APP_NAME}-coordinator-${region}" --org personal
            flyctl deploy --config "fly-coordinator-${region}.toml" --app "${APP_NAME}-coordinator-${region}"
        }
        
        success "Coordinator deployed to $region"
    done
}

# Deploy nodes (multiple per region)
deploy_nodes() {
    log "Deploying nodes across regions..."
    
    for region in "${REGIONS[@]}"; do
        log "Deploying nodes to $region..."
        
        # Deploy 2 nodes per region for testing
        for node_num in 1 2; do
            cat > "fly-node-${region}-${node_num}.toml" << EOF
app = "${APP_NAME}-node-${region}-${node_num}"
primary_region = "$region"

[build]
  dockerfile = "Dockerfile.fly"

[env]
  DISTSQLITE_ROLE = "node"
  FLY_REGION = "$region"
  FLY_PRIMARY_REGION = "$PRIMARY_REGION"
  LEASE_SERVER_URL = "http://${APP_NAME}-leaseserver.internal:8080"
  COORDINATOR_ENDPOINT = "${APP_NAME}-coordinator-${region}.internal:5555"

[[services]]
  internal_port = 8080
  protocol = "tcp"
  
  [[services.ports]]
    handlers = ["http"]
    port = 80

[checks]
  [checks.node_health]
    grace_period = "20s"
    interval = "45s"
    method = "get"
    path = "/health"
    port = 8080
    timeout = "15s"
    type = "http"
EOF

            flyctl deploy --config "fly-node-${region}-${node_num}.toml" --app "${APP_NAME}-node-${region}-${node_num}" || {
                warn "Creating new node app for ${region}-${node_num}..."
                flyctl apps create "${APP_NAME}-node-${region}-${node_num}" --org personal
                flyctl deploy --config "fly-node-${region}-${node_num}.toml" --app "${APP_NAME}-node-${region}-${node_num}"
            }
        done
        
        success "Nodes deployed to $region"
    done
}

# Deploy test runner
deploy_test_runner() {
    log "Deploying test runner..."
    
    cat > fly-test-runner.toml << EOF
app = "${APP_NAME}-tests"
primary_region = "$PRIMARY_REGION"

[build]
  dockerfile = "Dockerfile.fly"

[env]
  DISTSQLITE_ROLE = "test"
  FLY_PRIMARY_REGION = "$PRIMARY_REGION"
  LEASE_SERVER_URL = "http://${APP_NAME}-leaseserver.internal:8080"

# Test runner doesn't need external services
[[services]]
  internal_port = 8080
  protocol = "tcp"
EOF

    flyctl deploy --config fly-test-runner.toml --app "${APP_NAME}-tests" || {
        warn "Creating new test runner app..."
        flyctl apps create "${APP_NAME}-tests" --org personal
        flyctl deploy --config fly-test-runner.toml --app "${APP_NAME}-tests"
    }
    
    success "Test runner deployed"
}

# Wait for all services to be healthy
wait_for_services() {
    log "Waiting for all services to be healthy..."
    
    # Wait for lease server
    log "Waiting for lease server..."
    flyctl status --app "${APP_NAME}-leaseserver" || error "Lease server failed to start"
    
    # Wait for coordinators
    for region in "${REGIONS[@]}"; do
        log "Waiting for coordinator in $region..."
        flyctl status --app "${APP_NAME}-coordinator-${region}" || error "Coordinator in $region failed to start"
    done
    
    # Wait for nodes (basic check)
    for region in "${REGIONS[@]}"; do
        for node_num in 1 2; do
            log "Checking node ${region}-${node_num}..."
            flyctl status --app "${APP_NAME}-node-${region}-${node_num}" || warn "Node ${region}-${node_num} may have issues"
        done
    done
    
    success "Services are starting up"
    log "Giving services 30s to fully initialize..."
    sleep 30
}

# Run distributed tests
run_tests() {
    log "Running distributed tests..."
    
    log "Starting test execution..."
    flyctl ssh console --app "${APP_NAME}-tests" --command "/app/test/run_distributed_tests.sh" || {
        warn "Test execution completed (check logs for details)"
    }
    
    # Get test results
    log "Fetching test results..."
    flyctl ssh console --app "${APP_NAME}-tests" --command "cat /app/logs/test-results/test-report.md" || {
        warn "Could not fetch test report"
    }
    
    success "Test execution completed"
}

# Show deployment status
show_status() {
    log "📊 Deployment Status Summary"
    
    echo ""
    echo "🔧 Lease Server:"
    flyctl status --app "${APP_NAME}-leaseserver" | head -10
    
    echo ""
    echo "🎯 Coordinators:"
    for region in "${REGIONS[@]}"; do
        echo "  $region:"
        flyctl status --app "${APP_NAME}-coordinator-${region}" | head -3
    done
    
    echo ""
    echo "🌐 Access URLs:"
    echo "  Lease Server: https://${APP_NAME}-leaseserver.fly.dev"
    for region in "${REGIONS[@]}"; do
        echo "  Coordinator ($region): https://${APP_NAME}-coordinator-${region}.fly.dev"
    done
    
    echo ""
    echo "📝 To view logs:"
    echo "  flyctl logs --app ${APP_NAME}-leaseserver"
    echo "  flyctl logs --app ${APP_NAME}-coordinator-ord"
    echo "  flyctl logs --app ${APP_NAME}-tests"
    
    echo ""
    echo "🧪 To run tests again:"
    echo "  flyctl ssh console --app ${APP_NAME}-tests --command '/app/test/run_distributed_tests.sh'"
}

# Cleanup deployment
cleanup() {
    log "🧹 Cleaning up deployment..."
    
    read -p "Are you sure you want to destroy all apps? (y/N) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        # Destroy all apps
        flyctl apps destroy "${APP_NAME}-leaseserver" --yes || true
        flyctl apps destroy "${APP_NAME}-tests" --yes || true
        
        for region in "${REGIONS[@]}"; do
            flyctl apps destroy "${APP_NAME}-coordinator-${region}" --yes || true
            for node_num in 1 2; do
                flyctl apps destroy "${APP_NAME}-node-${region}-${node_num}" --yes || true
            done
        done
        
        # Clean up config files
        rm -f fly-*.toml
        
        success "Cleanup completed"
    else
        log "Cleanup cancelled"
    fi
}

# Main deployment function
main() {
    case "${1:-deploy}" in
        "deploy")
            check_prereqs
            build_app
            deploy_leaseserver
            deploy_coordinators
            deploy_nodes
            deploy_test_runner
            wait_for_services
            show_status
            ;;
        "test")
            run_tests
            ;;
        "status")
            show_status
            ;;
        "cleanup")
            cleanup
            ;;
        *)
            echo "Usage: $0 [deploy|test|status|cleanup]"
            echo ""
            echo "Commands:"
            echo "  deploy   - Full deployment of DistSQLite across regions"
            echo "  test     - Run distributed tests"
            echo "  status   - Show deployment status"
            echo "  cleanup  - Destroy all deployed resources"
            exit 1
            ;;
    esac
}

main "$@"