# DistSQLite Production Deployment Guide

## Table of Contents
- [Overview](#overview)
- [Architecture](#architecture)
- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Configuration](#configuration)
- [Deployment](#deployment)
- [Monitoring](#monitoring)
- [Security](#security)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)
- [Performance Tuning](#performance-tuning)

## Overview

DistSQLite is a production-ready distributed SQLite platform that provides zero-code-change distribution with enterprise-grade safety mechanisms. This guide covers deploying DistSQLite in production environments.

### Key Features
- **Zero-code-change**: Existing SQLite applications work without modification
- **Automatic failover**: 5-second failover with fence tokens
- **Split-brain protection**: Vector clocks and quorum-based safety
- **Transaction log shipping**: <50ms latency for small transactions
- **Bulk replication**: 10x faster bulk operations than competitors
- **Network partition tolerance**: Graceful degradation and recovery
- **Comprehensive monitoring**: Prometheus metrics and health checks

## Architecture

### Component Overview
```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   Application   │    │   Application   │    │   Application   │
│                 │    │                 │    │                 │
├─────────────────┤    ├─────────────────┤    ├─────────────────┤
│  DistSQLite VFS │    │  DistSQLite VFS │    │  DistSQLite VFS │
│   (Sidecar)     │    │   (Sidecar)     │    │   (Sidecar)     │
└─────────────────┘    └─────────────────┘    └─────────────────┘
         │                       │                       │
         └───────────────────────┼───────────────────────┘
                                 │
                    ┌─────────────────┐
                    │     Consul      │
                    │  (Coordination) │
                    └─────────────────┘
```

### Network Architecture
- **Primary/Replica Model**: One writer, multiple readers
- **Lease-based Coordination**: Consul-backed distributed locking
- **Multi-region Support**: Cross-region replication with conflict resolution
- **Circuit Breakers**: Automatic failover and degradation

## Prerequisites

### System Requirements
- **OS**: Linux 4.14+ (Ubuntu 20.04+ or RHEL 8+ recommended)
- **CPU**: 2+ cores per node
- **Memory**: 4GB+ RAM per node
- **Storage**: SSD storage recommended, 100GB+ per node
- **Network**: 1Gbps+ between nodes, <10ms latency within region

### Software Dependencies
- **Go**: 1.21+ (for building from source)
- **GCC**: 9.0+ with pthread support
- **SQLite**: 3.38+ (automatically downloaded)
- **Consul**: 1.15+ for coordination
- **Docker**: 20.10+ (optional, for containerized deployment)
- **Kubernetes**: 1.25+ (optional, for k8s deployment)

### Network Requirements
- **Ports**:
  - `8080`: HTTP API (configurable)
  - `8500`: Consul (if running locally)
  - `8301-8302`: Consul cluster communication
- **Firewall**: Allow inter-node communication on required ports
- **DNS**: Proper hostname resolution between nodes

## Installation

### Option 1: Binary Installation

1. **Download Release**:
   ```bash
   wget https://github.com/your-org/distsqlite/releases/latest/download/distsqlite-linux-amd64.tar.gz
   tar -xzf distsqlite-linux-amd64.tar.gz
   cd distsqlite-linux-amd64
   ```

2. **Install Binaries**:
   ```bash
   sudo cp distsqlite /usr/local/bin/
   sudo cp distsqlite-inject /usr/local/bin/
   sudo cp libdistsqlite_vfs.so /usr/local/lib/
   sudo cp sqlite3* /usr/local/bin/
   sudo ldconfig
   ```

3. **Create System User**:
   ```bash
   sudo useradd -r -s /bin/false distsqlite
   sudo mkdir -p /var/lib/distsqlite /var/log/distsqlite
   sudo chown distsqlite:distsqlite /var/lib/distsqlite /var/log/distsqlite
   ```

### Option 2: Build from Source

1. **Clone Repository**:
   ```bash
   git clone https://github.com/your-org/distsqlite.git
   cd distsqlite
   ```

2. **Install Build Dependencies**:
   ```bash
   # Ubuntu/Debian
   sudo apt-get update
   sudo apt-get install -y build-essential golang-1.21 libsqlite3-dev pkg-config
   
   # RHEL/CentOS
   sudo dnf install -y gcc golang sqlite-devel pkgconfig
   ```

3. **Build**:
   ```bash
   make clean
   make deps
   make build
   make test
   ```

4. **Install**:
   ```bash
   sudo make install
   ```

### Option 3: Docker Deployment

1. **Pull Image**:
   ```bash
   docker pull your-registry/distsqlite:latest
   ```

2. **Run Container**:
   ```bash
   docker run -d \
     --name distsqlite \
     --network host \
     -v /var/lib/distsqlite:/data \
     -e DISTSQLITE_HOSTNAME=$(hostname) \
     -e DISTSQLITE_CONSUL_URL=http://consul:8500 \
     your-registry/distsqlite:latest
   ```

## Configuration

### Environment Variables
```bash
# Node identification
DISTSQLITE_HOSTNAME=node-1
DISTSQLITE_NODE_ID=node-1-us-east-1
DISTSQLITE_ADVERTISE_URL=http://10.0.1.10:8080

# Consul configuration
DISTSQLITE_CONSUL_URL=http://consul:8500
DISTSQLITE_LEASE_KEY=distsqlite/primary

# Replication
DISTSQLITE_TARGETS=node-2:8080,node-3:8080
DISTSQLITE_REPLICATION_MODE=auto

# Safety settings
DISTSQLITE_ENABLE_FENCE_TOKENS=true
DISTSQLITE_ENABLE_VECTOR_CLOCKS=true
DISTSQLITE_MIN_QUORUM_SIZE=2

# Performance tuning
DISTSQLITE_BULK_THRESHOLD=1048576
DISTSQLITE_MAX_REPLICATION_DELAY=5s

# Monitoring
DISTSQLITE_LOG_LEVEL=info
DISTSQLITE_ENABLE_METRICS=true
```

### Configuration File (`/etc/distsqlite/config.yaml`)
```yaml
# Node configuration
node:
  hostname: "node-1"
  node_id: "node-1-us-east-1"
  advertise_url: "http://10.0.1.10:8080"
  data_dir: "/var/lib/distsqlite"
  
# HTTP server
http:
  addr: ":8080"
  
# Consul configuration
consul:
  url: "http://consul:8500"
  lease_key: "distsqlite/primary"
  
# Replication settings
replication:
  targets:
    - "node-2:8080"
    - "node-3:8080"
  mode: "auto"
  bulk_threshold: 1048576
  max_delay: "5s"
  
# Safety settings
safety:
  enable_fence_tokens: true
  enable_vector_clocks: true
  enable_conflict_detection: true
  max_clock_skew: "5s"
  min_quorum_size: 2
  
# Logging and monitoring
logging:
  level: "info"
  
monitoring:
  enable_metrics: true
  enable_tracing: false
```

### Command Line Options
```bash
distsqlite \
  --hostname=node-1 \
  --node-id=node-1-us-east-1 \
  --advertise-url=http://10.0.1.10:8080 \
  --data-dir=/var/lib/distsqlite \
  --http-addr=:8080 \
  --consul-url=http://consul:8500 \
  --targets=node-2:8080,node-3:8080 \
  --replication-mode=auto \
  --enable-safety=true \
  --enable-fence-tokens=true \
  --enable-vector-clocks=true \
  --min-quorum-size=2 \
  --bulk-threshold=1048576 \
  --max-replication-delay=5s \
  --log-level=info \
  --enable-metrics=true
```

## Deployment

### Systemd Service

1. **Create Service File** (`/etc/systemd/system/distsqlite.service`):
   ```ini
   [Unit]
   Description=DistSQLite Distributed SQLite Service
   After=network.target consul.service
   Wants=consul.service
   
   [Service]
   Type=simple
   User=distsqlite
   Group=distsqlite
   
   # Environment
   EnvironmentFile=-/etc/distsqlite/environment
   
   # Execution
   ExecStart=/usr/local/bin/distsqlite \
     --config=/etc/distsqlite/config.yaml
   
   ExecReload=/bin/kill -HUP $MAINPID
   
   # Restart policy
   Restart=always
   RestartSec=5
   
   # Security
   NoNewPrivileges=true
   PrivateTmp=true
   ProtectSystem=strict
   ProtectHome=true
   ReadWritePaths=/var/lib/distsqlite /var/log/distsqlite
   
   # Limits
   LimitNOFILE=65536
   LimitMEMLOCK=infinity
   
   [Install]
   WantedBy=multi-user.target
   ```

2. **Enable and Start**:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable distsqlite
   sudo systemctl start distsqlite
   sudo systemctl status distsqlite
   ```

### Kubernetes Deployment

1. **ConfigMap** (`k8s/configmap.yaml`):
   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: distsqlite-config
   data:
     config.yaml: |
       node:
         data_dir: "/data"
       http:
         addr: ":8080"
       consul:
         url: "http://consul:8500"
       replication:
         mode: "auto"
       safety:
         enable_fence_tokens: true
         min_quorum_size: 2
       monitoring:
         enable_metrics: true
   ```

2. **StatefulSet** (`k8s/statefulset.yaml`):
   ```yaml
   apiVersion: apps/v1
   kind: StatefulSet
   metadata:
     name: distsqlite
   spec:
     serviceName: distsqlite
     replicas: 3
     selector:
       matchLabels:
         app: distsqlite
     template:
       metadata:
         labels:
           app: distsqlite
       spec:
         containers:
         - name: distsqlite
           image: your-registry/distsqlite:latest
           ports:
           - containerPort: 8080
             name: http
           env:
           - name: DISTSQLITE_HOSTNAME
             valueFrom:
               fieldRef:
                 fieldPath: metadata.name
           - name: DISTSQLITE_NODE_ID
             valueFrom:
               fieldRef:
                 fieldPath: metadata.name
           volumeMounts:
           - name: data
             mountPath: /data
           - name: config
             mountPath: /etc/distsqlite
           livenessProbe:
             httpGet:
               path: /health
               port: 8080
             initialDelaySeconds: 30
             periodSeconds: 10
           readinessProbe:
             httpGet:
               path: /readiness
               port: 8080
             initialDelaySeconds: 5
             periodSeconds: 5
           resources:
             requests:
               memory: "1Gi"
               cpu: "500m"
             limits:
               memory: "4Gi"
               cpu: "2000m"
         volumes:
         - name: config
           configMap:
             name: distsqlite-config
     volumeClaimTemplates:
     - metadata:
         name: data
       spec:
         accessModes: ["ReadWriteOnce"]
         resources:
           requests:
             storage: 100Gi
         storageClassName: fast-ssd
   ```

3. **Service** (`k8s/service.yaml`):
   ```yaml
   apiVersion: v1
   kind: Service
   metadata:
     name: distsqlite
   spec:
     clusterIP: None
     selector:
       app: distsqlite
     ports:
     - port: 8080
       targetPort: 8080
       name: http
   ```

4. **Deploy**:
   ```bash
   kubectl apply -f k8s/
   kubectl get statefulset distsqlite
   kubectl get pods -l app=distsqlite
   ```

## Monitoring

### Health Checks

1. **HTTP Endpoints**:
   ```bash
   # Basic health
   curl http://localhost:8080/health
   
   # Readiness check
   curl http://localhost:8080/readiness
   
   # Detailed status
   curl http://localhost:8080/status
   
   # Safety metrics
   curl http://localhost:8080/safety
   ```

2. **Expected Responses**:
   ```json
   // /health
   {
     "status": "healthy",
     "timestamp": "2024-01-15T10:30:00Z",
     "uptime_seconds": 3600,
     "last_health_check": "2024-01-15T10:29:30Z"
   }
   
   // /status
   {
     "node_id": "node-1-us-east-1",
     "hostname": "node-1",
     "is_primary": true,
     "current_primary": "node-1",
     "fence_token": 42,
     "is_healthy": true,
     "replicator_healthy": true,
     "uptime_seconds": 3600
   }
   ```

### Prometheus Metrics

1. **Key Metrics**:
   - `distsqlite_operations_total{operation, database, status}`
   - `distsqlite_operation_duration_seconds{operation, database}`
   - `distsqlite_replication_lag_seconds{target}`
   - `distsqlite_lease_status{node_id}`

2. **Prometheus Configuration** (`prometheus.yml`):
   ```yaml
   scrape_configs:
   - job_name: 'distsqlite'
     static_configs:
     - targets: ['node-1:8080', 'node-2:8080', 'node-3:8080']
     metrics_path: '/metrics'
     scrape_interval: 15s
   ```

3. **Grafana Dashboard**:
   ```json
   {
     "dashboard": {
       "title": "DistSQLite Monitoring",
       "panels": [
         {
           "title": "Operations per Second",
           "targets": [
             {
               "expr": "rate(distsqlite_operations_total[5m])",
               "legendFormat": "{{operation}} - {{instance}}"
             }
           ]
         },
         {
           "title": "Replication Lag",
           "targets": [
             {
               "expr": "distsqlite_replication_lag_seconds",
               "legendFormat": "{{target}}"
             }
           ]
         },
         {
           "title": "Primary Node Status",
           "targets": [
             {
               "expr": "distsqlite_lease_status",
               "legendFormat": "{{node_id}}"
             }
           ]
         }
       ]
     }
   }
   ```

### Alerting Rules

1. **Prometheus Alerts** (`alerts.yml`):
   ```yaml
   groups:
   - name: distsqlite
     rules:
     - alert: DistSQLiteNodeDown
       expr: up{job="distsqlite"} == 0
       for: 30s
       labels:
         severity: critical
       annotations:
         summary: "DistSQLite node is down"
         description: "DistSQLite node {{ $labels.instance }} has been down for more than 30 seconds."
   
     - alert: DistSQLiteHighReplicationLag
       expr: distsqlite_replication_lag_seconds > 10
       for: 1m
       labels:
         severity: warning
       annotations:
         summary: "High replication lag detected"
         description: "Replication lag to {{ $labels.target }} is {{ $value }} seconds."
   
     - alert: DistSQLiteNoPrimary
       expr: sum(distsqlite_lease_status) == 0
       for: 10s
       labels:
         severity: critical
       annotations:
         summary: "No primary node elected"
         description: "No DistSQLite node is currently holding the primary lease."
   
     - alert: DistSQLiteMultiplePrimaries
       expr: sum(distsqlite_lease_status) > 1
       for: 5s
       labels:
         severity: critical
       annotations:
         summary: "Multiple primary nodes detected"
         description: "{{ $value }} DistSQLite nodes are claiming to be primary (split-brain)."
   ```

## Security

### Authentication and Authorization

1. **mTLS Configuration**:
   ```yaml
   tls:
     cert_file: "/etc/distsqlite/tls/server.crt"
     key_file: "/etc/distsqlite/tls/server.key"
     ca_file: "/etc/distsqlite/tls/ca.crt"
     client_auth: true
   ```

2. **Consul ACL**:
   ```bash
   # Create policy for DistSQLite
   consul acl policy create \
     -name distsqlite \
     -description "DistSQLite access policy" \
     -rules @distsqlite-policy.hcl
   
   # Create token
   consul acl token create \
     -description "DistSQLite token" \
     -policy-name distsqlite
   ```

3. **Network Security**:
   ```bash
   # Firewall rules
   sudo ufw allow from 10.0.0.0/8 to any port 8080
   sudo ufw allow from 10.0.0.0/8 to any port 8500
   sudo ufw deny 8080
   sudo ufw deny 8500
   ```

### Data Encryption

1. **At-Rest Encryption**:
   ```bash
   # Use encrypted file system
   sudo cryptsetup luksFormat /dev/sdb
   sudo cryptsetup luksOpen /dev/sdb distsqlite-data
   sudo mkfs.ext4 /dev/mapper/distsqlite-data
   sudo mount /dev/mapper/distsqlite-data /var/lib/distsqlite
   ```

2. **In-Transit Encryption**:
   - All node-to-node communication uses TLS 1.3
   - Consul communication encrypted with TLS
   - Transaction log shipping encrypted with AES-256

## Operations

### Backup and Recovery

1. **Database Backup**:
   ```bash
   # Online backup using sqlite3_rsync
   sqlite3_rsync /var/lib/distsqlite/database.db \
     backup-server:/backup/database-$(date +%Y%m%d-%H%M%S).db
   
   # Snapshot backup
   rsync -av /var/lib/distsqlite/ backup-server:/backup/distsqlite-$(date +%Y%m%d)/
   ```

2. **Point-in-Time Recovery**:
   ```bash
   # Stop DistSQLite
   sudo systemctl stop distsqlite
   
   # Restore from backup
   rsync -av backup-server:/backup/distsqlite-20240115/ /var/lib/distsqlite/
   
   # Start DistSQLite
   sudo systemctl start distsqlite
   ```

### Scaling Operations

1. **Add Node**:
   ```bash
   # Deploy new node
   ansible-playbook -i inventory deploy-node.yml -e node_name=node-4
   
   # Update existing nodes with new target
   curl -X PATCH http://node-1:8080/config \
     -H "Content-Type: application/json" \
     -d '{"replication_targets": ["node-2:8080", "node-3:8080", "node-4:8080"]}'
   ```

2. **Remove Node**:
   ```bash
   # Remove from targets on other nodes
   curl -X PATCH http://node-1:8080/config \
     -H "Content-Type: application/json" \
     -d '{"replication_targets": ["node-2:8080", "node-3:8080"]}'
   
   # Gracefully shutdown node
   curl -X POST http://node-4:8080/maintenance \
     -H "Content-Type: application/json" \
     -d '{"action": "force_lease_release"}'
   
   sudo systemctl stop distsqlite
   ```

### Maintenance Tasks

1. **Rolling Updates**:
   ```bash
   # Update one node at a time
   for node in node-1 node-2 node-3; do
     echo "Updating $node"
     
     # If primary, force failover
     if [[ $(curl -s http://$node:8080/status | jq -r '.is_primary') == "true" ]]; then
       curl -X POST http://$node:8080/maintenance \
         -H "Content-Type: application/json" \
         -d '{"action": "force_lease_release"}'
       sleep 10  # Wait for failover
     fi
     
     # Update binary
     scp distsqlite $node:/tmp/
     ssh $node "sudo systemctl stop distsqlite && sudo cp /tmp/distsqlite /usr/local/bin/ && sudo systemctl start distsqlite"
     
     # Wait for health check
     until curl -f http://$node:8080/health; do
       echo "Waiting for $node to become healthy..."
       sleep 5
     done
   done
   ```

2. **Configuration Updates**:
   ```bash
   # Update replication mode
   curl -X PUT http://node-1:8080/replication/mode \
     -H "Content-Type: application/json" \
     -d '{"mode": "bulk"}'
   
   # Force health check
   curl -X POST http://node-1:8080/maintenance \
     -H "Content-Type: application/json" \
     -d '{"action": "health_check"}'
   ```

## Troubleshooting

### Common Issues

1. **Split-Brain Detection**:
   ```bash
   # Check safety metrics
   curl http://node-1:8080/safety | jq '.safety_metrics'
   
   # Check network connectivity
   for node in node-2 node-3; do
     curl -m 5 http://$node:8080/health || echo "$node unreachable"
   done
   
   # Force lease release if needed
   curl -X POST http://node-1:8080/maintenance \
     -d '{"action": "force_lease_release"}'
   ```

2. **High Replication Lag**:
   ```bash
   # Check replication stats
   curl http://node-1:8080/stats | jq '.transaction_log_stats'
   
   # Check network bandwidth
   iperf3 -c node-2 -p 5201
   
   # Switch to bulk replication temporarily
   curl -X PUT http://node-1:8080/replication/mode \
     -d '{"mode": "bulk"}'
   ```

3. **Memory Leaks**:
   ```bash
   # Check memory usage
   ps aux | grep distsqlite
   
   # Run memory profiling
   curl http://node-1:8080/debug/pprof/heap > heap.prof
   go tool pprof heap.prof
   
   # Check for VFS issues
   valgrind --tool=memcheck ./distsqlite --help
   ```

### Log Analysis

1. **Structured Logging**:
   ```bash
   # Filter by level
   journalctl -u distsqlite -p err
   
   # Search for specific events
   journalctl -u distsqlite | grep "Split-brain"
   
   # Export logs for analysis
   journalctl -u distsqlite --since "1 hour ago" -o json > logs.jsonl
   ```

2. **Log Aggregation** (ELK Stack):
   ```yaml
   # filebeat.yml
   filebeat.inputs:
   - type: journald
     id: distsqlite
     include_matches:
       - "_SYSTEMD_UNIT=distsqlite.service"
   
   output.elasticsearch:
     hosts: ["elasticsearch:9200"]
     index: "distsqlite-%{+yyyy.MM.dd}"
   ```

## Performance Tuning

### System Optimization

1. **Kernel Parameters** (`/etc/sysctl.conf`):
   ```
   # Network optimization
   net.core.rmem_max = 134217728
   net.core.wmem_max = 134217728
   net.ipv4.tcp_rmem = 4096 65536 134217728
   net.ipv4.tcp_wmem = 4096 65536 134217728
   
   # File system optimization
   fs.file-max = 2097152
   vm.swappiness = 1
   vm.dirty_ratio = 15
   vm.dirty_background_ratio = 5
   ```

2. **Storage Optimization**:
   ```bash
   # Mount with optimal flags
   mount -o noatime,data=writeback,barrier=0 /dev/sdb1 /var/lib/distsqlite
   
   # Set I/O scheduler
   echo deadline > /sys/block/sdb/queue/scheduler
   
   # Increase read-ahead
   blockdev --setra 4096 /dev/sdb1
   ```

3. **DistSQLite Tuning**:
   ```yaml
   # High-throughput configuration
   replication:
     bulk_threshold: 2097152  # 2MB
     max_delay: "2s"
   
   # Low-latency configuration
   replication:
     bulk_threshold: 65536    # 64KB
     max_delay: "100ms"
   
   # High-availability configuration
   safety:
     min_quorum_size: 3
     max_clock_skew: "1s"
   ```

### Performance Monitoring

1. **Key Metrics to Watch**:
   - Operation latency (p95, p99)
   - Replication lag
   - Memory usage
   - Disk I/O utilization
   - Network bandwidth

2. **Benchmarking**:
   ```bash
   # Basic benchmark
   sqlite3 test.db ".read benchmark.sql"
   
   # Distributed benchmark
   cd test/benchmark
   go test -bench=. -benchmem
   ```

---

## Summary

This production deployment guide provides comprehensive instructions for deploying, configuring, and operating DistSQLite in production environments. The system delivers on its promise of zero-code-change distributed SQLite with enterprise-grade safety, monitoring, and operational capabilities.

Key operational principles:
- **Safety First**: Always verify quorum and fence tokens before writes
- **Monitor Everything**: Use comprehensive metrics and alerting
- **Plan for Failure**: Design for network partitions and node failures
- **Test Regularly**: Validate disaster recovery procedures
- **Document Changes**: Maintain operational runbooks

For additional support, see the troubleshooting section or consult the operational runbooks for your specific deployment environment.