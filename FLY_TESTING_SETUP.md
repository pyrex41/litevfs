# DistSQLite Fly.io Multi-Region Testing Setup

This document describes the comprehensive testing setup for DistSQLite using Fly.io's global infrastructure and private networking with ZMQ transport.

## 🏗️ Architecture Overview

### Components

1. **Lease Server** (`distsqlite-leaseserver`)
   - Single instance in primary region (Chicago/ORD)
   - **ZMQ-first lease coordination** with HTTP fallback
   - REQ/REP pattern on port 5558 for ultra-low latency
   - Persistent storage for lease state
   - Fast failover capabilities

2. **Coordinators** (`distsqlite-coordinator`)
   - One per region (ORD, IAD, LAX, FRA)
   - ZMQ transport on ports 5555-5557
   - Connects to lease server for coordination
   - Manages local nodes and cross-region replication

3. **Nodes** (`distsqlite`)
   - 2 nodes per region (8 total)
   - Connect to regional coordinator via ZMQ
   - Participate in distributed replication
   - Handle SQLite operations

4. **Test Runner** (`distsqlite-tests`)
   - Orchestrates distributed testing
   - Measures performance across regions
   - Validates consistency and failover

### Network Architecture

```
                    ┌─────────────────┐
                    │   Lease Server  │
                    │ ZMQ:5558 HTTP:80│
                    │    (ORD)        │
                    └─────────────────┘
                            │ ZMQ REQ/REP
                ┌───────────┼───────────┐
                │           │           │
        ┌───────────┐ ┌───────────┐ ┌───────────┐
        │Coordinator│ │Coordinator│ │Coordinator│
        │   (ORD)   │ │   (IAD)   │ │   (LAX)   │
        │ ZMQ:5555-8│ │ ZMQ:5555-8│ │ ZMQ:5555-8│
        └───────────┘ └───────────┘ └───────────┘
         │         │   │         │   │         │
    ┌─────┐   ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐
    │Node │   │Node │ │Node │ │Node │ │Node │ │Node │
    │ 1   │   │ 2   │ │ 1   │ │ 2   │ │ 1   │ │ 2   │
    └─────┘   └─────┘ └─────┘ └─────┘ └─────┘ └─────┘
       ORD       ORD     IAD     IAD     LAX     LAX
```

## 🚀 Quick Start

### Prerequisites

```bash
# Install Fly.io CLI
curl -L https://fly.io/install.sh | sh

# Login to Fly.io
flyctl auth login

# Clone and build DistSQLite
git clone <your-repo>
cd litevfs
make build
```

### Deploy Everything

```bash
# Deploy entire multi-region setup
./deploy/fly-deploy.sh deploy

# Run distributed tests
./deploy/fly-deploy.sh test

# Check status
./deploy/fly-deploy.sh status

# Clean up when done
./deploy/fly-deploy.sh cleanup
```

## 📊 What Gets Tested

### Performance Tests

1. **ZMQ vs HTTP Transport**
   - Measures message latency and throughput
   - Validates ZMQ's performance advantage
   - Tests 1000 operations across transports

2. **Bulk Operation Performance**
   - Small datasets: 1K rows × 100B
   - Medium datasets: 10K rows × 1KB  
   - Large datasets: 100K rows × 100B
   - Measures ops/second and total time

3. **Cross-Region Replication**
   - Inserts data in one region
   - Measures replication latency to other regions
   - Validates data consistency

### Reliability Tests

1. **Split-Brain Prevention**
   - Simulates network partitions
   - Verifies only one node can hold primary lease
   - Tests automatic failover

2. **Concurrent Operations**
   - Multiple writers across regions
   - ACID compliance validation
   - Lock contention handling

3. **Failure Recovery**
   - Node crashes and restarts
   - Network partition recovery
   - Lease expiration handling

## 🔧 Configuration Details

### ZMQ Transport Optimization

```yaml
# Fly.io private network leveraging
zmq:
  use-private-network: true
  flyio-app-name: "distsqlite-test"
  request-timeout: "3s"
  high-water-mark: 10000
  
  # Port allocation
  lease-port: 5555      # REP socket
  diff-port: 5556       # PUB socket  
  router-port: 5557     # ROUTER socket
```

### ZMQ-First Lease Configuration

```yaml
# ZMQ-first lease coordination with HTTP fallback
lease:
  # Primary ZMQ lease coordination
  zmq-endpoints:
    - "tcp://leaseserver.internal:5558"  # Ultra-fast REQ/REP
    - "tcp://coordinator.internal:5558"  # Self-coordination fallback
  
  # HTTP fallback endpoints
  http-endpoints:
    - "http://leaseserver.internal:8080"
  
  # Transport preference
  transport-preference: "zmq"
  connection-timeout: "2s"
  retry-zmq-attempts: 3
```

## 📈 Performance Expectations

Based on DistSQLite's "10-100x faster than Turso" claim:

| Test Scenario | Expected Result |
|---------------|-----------------|
| ZMQ vs HTTP Transport | 3-5x faster message delivery |
| ZMQ vs HTTP Leases | 5-10x faster lease operations |
| Small datasets | 10x faster than baseline |
| Medium datasets | 50x faster than baseline |
| Large datasets | 100x faster than baseline |
| Cross-region latency | <100ms for replication |
| Lease failover | <500ms with ZMQ coordination |

## 🌍 Regional Deployment

### Regions Used

- **ORD** (Chicago) - Primary region, lease server
- **IAD** (N. Virginia) - East coast testing
- **LAX** (Los Angeles) - West coast testing  
- **FRA** (Frankfurt) - European testing

### Deployment Pattern

```bash
# Each region gets:
1x Coordinator (ZMQ coordination)
2x Nodes (SQLite operations)

# Plus global services:
1x Lease Server (primary region)
1x Test Runner (primary region)
```

## 🔍 Monitoring and Debugging

### Health Checks

```bash
# Check lease server
curl https://distsqlite-test-leaseserver.fly.dev/health

# Check coordinator
curl https://distsqlite-test-coordinator-ord.fly.dev/health/zmq

# Check node
curl https://distsqlite-test-node-ord-1.fly.dev/health
```

### Logs

```bash
# View lease server logs
flyctl logs --app distsqlite-test-leaseserver

# View coordinator logs
flyctl logs --app distsqlite-test-coordinator-ord

# View test results
flyctl logs --app distsqlite-test-tests
```

### SSH Access

```bash
# Connect to any service
flyctl ssh console --app distsqlite-test-leaseserver

# Run manual tests
flyctl ssh console --app distsqlite-test-tests
```

## 🧪 Running Custom Tests

### Manual Test Execution

```bash
# SSH into test runner
flyctl ssh console --app distsqlite-test-tests

# Run specific test
cd /app
./test/integration/split_brain_test.go

# Run performance benchmarks
go test -bench=. ./test/benchmarks/
```

### Adding New Tests

1. Add test to `test/run_distributed_tests.sh`
2. Update configurations if needed
3. Redeploy: `./deploy/fly-deploy.sh deploy`

## 🛠️ Troubleshooting

### Common Issues

1. **ZMQ Connection Failures**
   ```bash
   # Check ZMQ connectivity
   flyctl ssh console --app distsqlite-test-coordinator-ord
   /app/bin/zmq-test --endpoint coordinator.internal:5555
   ```

2. **Lease Server Unreachable**
   ```bash
   # Verify lease server health
   curl https://distsqlite-test-leaseserver.fly.dev/health
   ```

3. **Cross-Region Latency**
   ```bash
   # Check Fly.io network status
   flyctl status --app distsqlite-test-coordinator-ord
   ```

### Debug Mode

```bash
# Deploy with debug logging
export LOG_LEVEL=debug
./deploy/fly-deploy.sh deploy
```

## 💡 Key Advantages

### Over HTTP-based Testing

1. **ZMQ Performance**: 3-5x faster than HTTP for messaging
2. **ZMQ Lease Performance**: 5-10x faster lease operations  
3. **Connection Efficiency**: Persistent connections vs HTTP overhead
4. **Binary Protocol**: Msgpack serialization vs JSON
5. **Multiplexing**: Multiple message types on same connection
6. **Intelligent Fallback**: ZMQ-first with HTTP backup

### Over Single-Region Testing

1. **Real Network Latency**: Tests actual cross-region performance
2. **Failure Scenarios**: Network partitions, regional outages
3. **Consistency Validation**: Distributed consensus testing
4. **Scalability**: Multiple regions, multiple nodes per region

### Fly.io Specific Benefits

1. **Private Network**: Fast, secure inter-service communication
2. **Global Regions**: Realistic geographic distribution
3. **Managed Infrastructure**: No server management overhead
4. **Built-in Monitoring**: Health checks, logs, metrics

## 📋 Test Results Format

Tests generate structured reports:

```markdown
# DistSQLite Distributed Test Report

**Transport Comparison:**
- ZMQ: 45ms (1000 requests)
- HTTP: 180ms (1000 requests)  
- Speedup: 4x faster with ZMQ

**Lease Performance:**
- ZMQ Leases: 850ms (100 acquire/release cycles)
- HTTP Leases: 4200ms (100 acquire/release cycles)
- Speedup: 5x faster with ZMQ leases

**Performance Results:**
- SmallData: 15,000 ops/sec
- MediumData: 8,500 ops/sec
- LargeData: 2,100 ops/sec

**Reliability:**
- ✅ Split-brain Prevention
- ✅ Cross-region Replication
- ✅ Lease Failover (<500ms)
```

This setup provides comprehensive testing of DistSQLite's distributed capabilities using Fly.io's infrastructure with efficient ZMQ-first transport and ZMQ-first lease coordination with intelligent HTTP fallback.