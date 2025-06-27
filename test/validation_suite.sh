#!/bin/bash
set -e

# DistSQLite Validation Suite
# Comprehensive local testing before Fly.io deployment

echo "🧪 DistSQLite Architecture Validation Suite"
echo "=============================================="

# Configuration
TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$TEST_DIR")"
LOG_DIR="$TEST_DIR/validation-logs"
RESULTS_FILE="$LOG_DIR/validation-results.txt"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Create log directory
mkdir -p "$LOG_DIR"

# Test results tracking
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

log() {
    echo -e "${BLUE}[$(date '+%H:%M:%S')]${NC} $1" | tee -a "$RESULTS_FILE"
}

success() {
    echo -e "${GREEN}✅ $1${NC}" | tee -a "$RESULTS_FILE"
    PASSED_TESTS=$((PASSED_TESTS + 1))
}

fail() {
    echo -e "${RED}❌ $1${NC}" | tee -a "$RESULTS_FILE"
    FAILED_TESTS=$((FAILED_TESTS + 1))
}

warn() {
    echo -e "${YELLOW}⚠️  $1${NC}" | tee -a "$RESULTS_FILE"
}

run_test() {
    local test_name="$1"
    local test_command="$2"
    local test_dir="${3:-$TEST_DIR}"
    
    TOTAL_TESTS=$((TOTAL_TESTS + 1))
    log "Running: $test_name"
    
    local start_time=$(date +%s)
    
    if (cd "$test_dir" && eval "$test_command") >"$LOG_DIR/${test_name// /_}.log" 2>&1; then
        local end_time=$(date +%s)
        local duration=$((end_time - start_time))
        success "$test_name (${duration}s)"
        return 0
    else
        local end_time=$(date +%s)
        local duration=$((end_time - start_time))
        fail "$test_name (${duration}s) - Check $LOG_DIR/${test_name// /_}.log"
        return 1
    fi
}

check_dependencies() {
    log "Checking dependencies..."
    
    local deps=("go" "make" "curl" "nc")
    local missing_deps=()
    
    for dep in "${deps[@]}"; do
        if ! command -v "$dep" &> /dev/null; then
            missing_deps+=("$dep")
        fi
    done
    
    if [ ${#missing_deps[@]} -ne 0 ]; then
        fail "Missing dependencies: ${missing_deps[*]}"
        echo "Please install missing dependencies:"
        echo "  Ubuntu/Debian: sudo apt-get install golang-go build-essential curl netcat-openbsd"
        echo "  macOS: brew install go make curl netcat"
        exit 1
    fi
    
    success "All dependencies found"
}

check_build() {
    log "Checking build system..."
    
    cd "$PROJECT_ROOT"
    
    # Check if Makefile exists
    if [ ! -f "Makefile" ]; then
        fail "Makefile not found in project root"
        return 1
    fi
    
    # Check if Go modules are set up
    if [ ! -f "go.mod" ]; then
        fail "go.mod not found - Go modules not initialized"
        return 1
    fi
    
    # Try to build
    if make build >/dev/null 2>&1; then
        success "Build system working"
    else
        fail "Build failed - check Go dependencies"
        return 1
    fi
}

validate_zmq_setup() {
    log "Validating ZMQ setup..."
    
    # Check if ZMQ Go bindings are available
    cd "$PROJECT_ROOT"
    if go list -m github.com/pebbe/zmq4 >/dev/null 2>&1; then
        success "ZMQ Go bindings available"
    else
        warn "ZMQ Go bindings not found - installing..."
        if go get github.com/pebbe/zmq4; then
            success "ZMQ Go bindings installed"
        else
            fail "Failed to install ZMQ Go bindings"
            echo "You may need to install libzmq3-dev:"
            echo "  Ubuntu/Debian: sudo apt-get install libzmq3-dev"
            echo "  macOS: brew install zmq"
            return 1
        fi
    fi
    
    # Check if msgpack is available
    if go list -m github.com/vmihailenco/msgpack/v5 >/dev/null 2>&1; then
        success "Msgpack bindings available"
    else
        warn "Msgpack bindings not found - installing..."
        if go get github.com/vmihailenco/msgpack/v5; then
            success "Msgpack bindings installed"
        else
            fail "Failed to install msgpack bindings"
            return 1
        fi
    fi
}

run_unit_tests() {
    log "Running unit tests..."
    
    # ZMQ transport tests
    run_test "ZMQ Transport Tests" "go run test/integration/zmq_transport_test.go" "$PROJECT_ROOT"
    
    # Fallback mechanism tests
    run_test "ZMQ-HTTP Fallback Tests" "go run test/integration/fallback_test.go" "$PROJECT_ROOT"
    
    # Existing unit tests if they exist
    if [ -d "$PROJECT_ROOT/test/unit" ]; then
        run_test "Unit Test Suite" "go test ./test/unit/..." "$PROJECT_ROOT"
    fi
}

run_integration_tests() {
    log "Running integration tests..."
    
    # Check if integration tests exist
    if [ -d "$PROJECT_ROOT/test/integration" ]; then
        # Run existing integration tests
        run_test "Integration Test Suite" "go test ./test/integration/..." "$PROJECT_ROOT"
    fi
    
    # Test distributed coordination
    run_test "Split Brain Prevention" "timeout 30s go test -run TestSplitBrain ./test/integration/..." "$PROJECT_ROOT"
    
    # Test concurrent operations
    run_test "Concurrent Operations" "timeout 30s go test -run TestConcurrent ./test/integration/..." "$PROJECT_ROOT"
}

run_performance_tests() {
    log "Running performance benchmarks..."
    
    # Load tests
    run_test "Distributed Load Tests" "go run test/benchmarks/distributed_load_test.go" "$PROJECT_ROOT"
    
    # Benchmark specific operations
    run_test "ZMQ vs HTTP Benchmark" "timeout 120s go test -bench=. ./test/benchmarks/..." "$PROJECT_ROOT"
    
    # Memory and CPU profiling
    if command -v pprof &> /dev/null; then
        run_test "Performance Profiling" "go test -cpuprofile cpu.prof -memprofile mem.prof -bench=. ./test/benchmarks/..." "$PROJECT_ROOT"
    else
        warn "pprof not available - skipping profiling tests"
    fi
}

validate_configuration() {
    log "Validating configuration files..."
    
    local config_files=(
        "deploy/fly/fly-coordinator.yml"
        "deploy/fly/fly-node.yml"
        "deploy/fly/fly-leaseserver.yml"
        "fly.toml"
        "Dockerfile.fly"
    )
    
    for config_file in "${config_files[@]}"; do
        if [ -f "$PROJECT_ROOT/$config_file" ]; then
            success "Configuration found: $config_file"
        else
            fail "Missing configuration: $config_file"
        fi
    done
    
    # Validate YAML syntax if yq is available
    if command -v yq &> /dev/null; then
        for config_file in "${config_files[@]}"; do
            if [[ "$config_file" == *.yml ]] && [ -f "$PROJECT_ROOT/$config_file" ]; then
                if yq eval . "$PROJECT_ROOT/$config_file" >/dev/null 2>&1; then
                    success "Valid YAML: $config_file"
                else
                    fail "Invalid YAML: $config_file"
                fi
            fi
        done
    else
        warn "yq not available - skipping YAML validation"
    fi
    
    # Check for environment variable usage in configs
    log "Checking configuration security..."
    
    local security_issues=()
    
    # Check for hardcoded sensitive values
    if grep -r "password\|secret\|token" "$PROJECT_ROOT/deploy/fly" | grep -v "token_" | grep -v "#" | grep -v "\${" >/dev/null 2>&1; then
        security_issues+=("Potential hardcoded secrets in configuration")
    fi
    
    # Check ZMQ security is properly configured
    if grep -q "security:" "$PROJECT_ROOT/deploy/fly/fly-coordinator.yml"; then
        success "ZMQ security configuration present"
    else
        warn "ZMQ security configuration missing (acceptable for development)"
    fi
    
    if [ ${#security_issues[@]} -eq 0 ]; then
        success "Configuration security check passed"
    else
        for issue in "${security_issues[@]}"; do
            warn "Security concern: $issue"
        done
    fi
}

validate_docker_setup() {
    log "Validating Docker setup..."
    
    if command -v docker &> /dev/null; then
        # Test Docker build
        cd "$PROJECT_ROOT"
        if docker build -f Dockerfile.fly -t distsqlite-test . >/dev/null 2>&1; then
            success "Docker build successful"
            # Cleanup
            docker rmi distsqlite-test >/dev/null 2>&1 || true
        else
            fail "Docker build failed"
        fi
    else
        warn "Docker not available - skipping Docker validation"
    fi
}

simulate_network_conditions() {
    log "Simulating network conditions..."
    
    # Test with artificial delays (if tc is available)
    if command -v tc &> /dev/null; then
        warn "Network shaping available but requires root - skipping"
    else
        # Simple delay simulation in tests
        run_test "Network Latency Simulation" "timeout 60s go run test/integration/zmq_transport_test.go" "$PROJECT_ROOT"
    fi
}

check_security() {
    log "Running security checks..."
    
    # Check for common security issues
    local security_issues=()
    
    # Check for hardcoded credentials
    if grep -r "password\|secret\|key" "$PROJECT_ROOT/deploy" | grep -v "key:" | grep -v "#" >/dev/null 2>&1; then
        security_issues+=("Potential hardcoded credentials found")
    fi
    
    # Check for insecure configurations
    if grep -r "disable.*auth\|auth.*false" "$PROJECT_ROOT/deploy" >/dev/null 2>&1; then
        security_issues+=("Authentication disabled in configuration")
    fi
    
    if [ ${#security_issues[@]} -eq 0 ]; then
        success "No obvious security issues found"
    else
        for issue in "${security_issues[@]}"; do
            warn "Security concern: $issue"
        done
    fi
}

validate_deployment_readiness() {
    log "Validating deployment readiness..."
    
    # Check if flyctl is available
    if command -v flyctl &> /dev/null; then
        if flyctl auth whoami >/dev/null 2>&1; then
            success "Fly.io CLI authenticated"
        else
            warn "Fly.io CLI not authenticated - run 'flyctl auth login'"
        fi
    else
        warn "Fly.io CLI not installed - install from https://fly.io/docs/hands-on/install-flyctl/"
    fi
    
    # Validate deployment script
    if [ -f "$PROJECT_ROOT/deploy/fly-deploy.sh" ]; then
        if [ -x "$PROJECT_ROOT/deploy/fly-deploy.sh" ]; then
            success "Deployment script is executable"
        else
            warn "Deployment script not executable - run 'chmod +x deploy/fly-deploy.sh'"
        fi
    else
        fail "Deployment script not found"
    fi
}

generate_report() {
    log "Generating validation report..."
    
    local total_duration=$(( $(date +%s) - start_time ))
    local success_rate=$(( PASSED_TESTS * 100 / TOTAL_TESTS ))
    
    cat > "$LOG_DIR/summary.md" << EOF
# DistSQLite Validation Report

**Date:** $(date)  
**Duration:** ${total_duration}s  
**Total Tests:** $TOTAL_TESTS  
**Passed:** $PASSED_TESTS  
**Failed:** $FAILED_TESTS  
**Success Rate:** ${success_rate}%  

## Test Results

$(cat "$RESULTS_FILE")

## Recommendations

EOF

    if [ $FAILED_TESTS -eq 0 ]; then
        echo "✅ **All tests passed! Ready for Fly.io deployment.**" >> "$LOG_DIR/summary.md"
        echo "" >> "$LOG_DIR/summary.md"
        echo "Next steps:" >> "$LOG_DIR/summary.md"
        echo "1. Run \`./deploy/fly-deploy.sh deploy\`" >> "$LOG_DIR/summary.md"
        echo "2. Monitor deployment with \`./deploy/fly-deploy.sh status\`" >> "$LOG_DIR/summary.md"
        echo "3. Run distributed tests with \`./deploy/fly-deploy.sh test\`" >> "$LOG_DIR/summary.md"
    else
        echo "❌ **Some tests failed. Please fix issues before deployment.**" >> "$LOG_DIR/summary.md"
        echo "" >> "$LOG_DIR/summary.md"
        echo "Check the following log files for details:" >> "$LOG_DIR/summary.md"
        for log_file in "$LOG_DIR"/*.log; do
            if [ -f "$log_file" ]; then
                echo "- $(basename "$log_file")" >> "$LOG_DIR/summary.md"
            fi
        done
    fi

    echo "📄 Validation report saved to: $LOG_DIR/summary.md"
    echo "📋 Full results in: $RESULTS_FILE"
}

main() {
    local start_time=$(date +%s)
    
    # Clear previous results
    echo "🧪 DistSQLite Architecture Validation Suite" > "$RESULTS_FILE"
    echo "Started: $(date)" >> "$RESULTS_FILE"
    echo "===========================================" >> "$RESULTS_FILE"
    
    # Run validation steps
    check_dependencies
    check_build
    validate_zmq_setup
    validate_configuration
    validate_docker_setup
    check_security
    
    # Run test suites
    run_unit_tests
    run_integration_tests
    run_performance_tests
    
    # Final checks
    simulate_network_conditions
    validate_deployment_readiness
    
    # Generate report
    generate_report
    
    echo ""
    echo "🎯 Validation Summary:"
    echo "  Total Tests: $TOTAL_TESTS"
    echo "  Passed: $PASSED_TESTS"
    echo "  Failed: $FAILED_TESTS"
    echo "  Success Rate: $(( PASSED_TESTS * 100 / TOTAL_TESTS ))%"
    
    if [ $FAILED_TESTS -eq 0 ]; then
        echo ""
        success "All validations passed! 🚀"
        echo "Ready for Fly.io deployment:"
        echo "  ./deploy/fly-deploy.sh deploy"
        exit 0
    else
        echo ""
        fail "Some validations failed! 🔧"
        echo "Please fix issues before deployment."
        echo "Check logs in: $LOG_DIR/"
        exit 1
    fi
}

# Handle command line arguments
case "${1:-validate}" in
    "validate")
        main
        ;;
    "quick")
        log "Running quick validation (dependencies + build only)..."
        check_dependencies
        check_build
        validate_zmq_setup
        success "Quick validation passed!"
        ;;
    "performance")
        log "Running performance tests only..."
        check_dependencies
        validate_zmq_setup
        run_performance_tests
        ;;
    "clean")
        log "Cleaning validation logs..."
        rm -rf "$LOG_DIR"
        success "Validation logs cleaned"
        ;;
    *)
        echo "Usage: $0 [validate|quick|performance|clean]"
        echo ""
        echo "Commands:"
        echo "  validate     - Full validation suite (default)"
        echo "  quick        - Quick dependency and build check"
        echo "  performance  - Performance tests only"
        echo "  clean        - Clean validation logs"
        exit 1
        ;;
esac