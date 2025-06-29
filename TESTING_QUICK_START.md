# DistSQLite Testing Quick Start Guide

## 🚀 Get Started in 5 Minutes

### Option 1: Fly.io Production Testing (Recommended)

```bash
# 1. Validate everything locally first
./test/validation_suite.sh

# 2. Deploy to Fly.io (4 regions, 8+ services)
./deploy/fly-deploy.sh deploy

# 3. Run comprehensive distributed tests
./deploy/fly-deploy.sh test

# 4. Check status and monitor
./deploy/fly-deploy.sh status
flyctl logs --app distsqlite-test-tests

# 5. Clean up when done
./deploy/fly-deploy.sh cleanup
```

### Option 2: Local Multi-Node Testing

```bash
# 1. Set up 6-service local cluster
./setup_local_cluster.sh start

# 2. Run comprehensive tests
./test_local_cluster.sh

# 3. Test real applications
./test_application_integration.sh

# 4. Stop when done
./setup_local_cluster.sh stop
```

## 📊 What Gets Tested

### Fly.io Testing (Production-Like)
- **Multi-Region**: ORD, IAD, LAX, FRA regions
- **ZMQ vs HTTP Performance**: Validates 3-5x speedup
- **Cross-Region Replication**: <100ms latency
- **Split-Brain Prevention**: Network partition simulation
- **Lease Performance**: ZMQ 5-10x faster than HTTP
- **Scale Testing**: 8 nodes across 4 regions

### Local Testing (Development)
- **Multi-Node**: 6 services (1 lease server, 2 coordinators, 4 nodes)
- **Data Replication**: Between coordinator regions
- **Failover**: Coordinator crash recovery
- **Application Integration**: C, Go, Python, Node.js apps
- **Concurrent Operations**: Multi-writer validation

## 🎯 Success Criteria

### Performance Targets
- **ZMQ Transport**: 3-5x faster than HTTP
- **ZMQ Leases**: 5-10x faster than HTTP  
- **Overall**: 10-100x faster than Turso baseline
- **Replication**: <100ms cross-region
- **Failover**: <500ms with ZMQ coordination

### Reliability Targets
- ✅ Zero split-brain scenarios
- ✅ Successful coordinator failover
- ✅ Data consistency across regions
- ✅ Application compatibility (LD_PRELOAD)
- ✅ Concurrent write handling

## 🛠️ Troubleshooting

### Common Issues

**Build Failures:**
```bash
# Install dependencies
make deps
make build
```

**Fly Deployment Issues:**
```bash
# Check auth
flyctl auth whoami

# Check app status
flyctl status --app distsqlite-test-leaseserver
```

**Local Cluster Issues:**
```bash
# Check service status
./setup_local_cluster.sh status

# View logs
./setup_local_cluster.sh logs

# Restart cluster
./setup_local_cluster.sh stop
./setup_local_cluster.sh start
```

**ZMQ Connectivity Issues:**
```bash
# Check ZMQ libraries
pkg-config --exists libzmq

# Test ZMQ directly
./build/zmq-test --endpoint tcp://localhost:5555 --mode ping
```

## 📋 Architecture Overview

### Fly.io Deployment
```
Lease Server (ORD) ──┐
                     │
    ┌────────────────┼────────────────┐
    │                │                │
Coordinator      Coordinator    Coordinator
  (ORD)            (IAD)          (LAX)
    │                │                │
┌───┴───┐        ┌───┴───┐        ┌───┴───┐
│Node 1 │        │Node 3 │        │Node 5 │
│Node 2 │        │Node 4 │        │Node 6 │
└───────┘        └───────┘        └───────┘
```

### Local Deployment
```
Lease Server (8080/5558) ──┐
                           │
    └──────────────────────┼──────────────────────┘
    │                      │                      │
Coordinator 1          Coordinator 2         
(8081/5555)           (8082/5556)         
    │                      │                     
┌───┴───┐              ┌───┴───┐               
│Node 1 │              │Node 3 │               
│Node 2 │              │Node 4 │               
└───────┘              └───────┘               
```

## 🔧 Customization

### Testing Different Scenarios

**Load Testing:**
```bash
# Heavy load test
go test -bench=. ./test/benchmarks/ -benchtime=60s

# Concurrent connections
go test -run TestConcurrent ./test/integration/ -count=10
```

**Network Simulation:**
```bash
# Add latency (requires tc/netem)
sudo tc qdisc add dev lo root netem delay 100ms

# Test with latency
./test_local_cluster.sh
```

**Custom Applications:**
```bash
# Test your own app
LD_PRELOAD=./build/distsqlite-inject.so LD_LIBRARY_PATH=./build your_app

# Multiple instances
for i in {1..10}; do
    LD_PRELOAD=./build/distsqlite-inject.so your_app &
done
wait
```

## 📈 Performance Monitoring

### Real-Time Monitoring

**Fly.io:**
```bash
# Watch logs
flyctl logs --app distsqlite-test-coordinator-ord -f

# SSH into services
flyctl ssh console --app distsqlite-test-leaseserver

# Check metrics
curl https://distsqlite-test-coordinator-ord.fly.dev/metrics
```

**Local:**
```bash
# Monitor resources
htop

# Watch logs
tail -f ./local-cluster-logs/*.log

# Check connections
netstat -tulpn | grep -E "(5555|5556|5558|8080|8081|8082)"
```

## 🎉 Next Steps

After successful testing:

1. **Review Results**: Check test reports and performance metrics
2. **Optimize Configuration**: Tune based on test results
3. **Scale Testing**: Increase load and node count
4. **Production Deployment**: Deploy with your configuration
5. **Monitor**: Set up monitoring and alerting

Your DistSQLite system is now validated and ready for production use!