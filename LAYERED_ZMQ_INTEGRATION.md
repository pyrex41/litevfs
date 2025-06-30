# 🔄 DistSQLite Layered Replication + ZMQ Integration

## 🎯 **Your Scenario Explained**

> *"over 5m i do a large number of writes -- but any one of them would be small over a 5-10s period. What determines the frequency and thus, the sync mechanism that will be used?"*

**Answer**: The **layered replication system** (that I just built) determines frequency and mechanism, while **ZMQ transport** provides the communication patterns. Here's exactly how it works:

---

## 🏗️ **Architecture Integration**

### **Two-Layer System**

```
┌─────────────────────────────────────────────────────────┐
│                LAYERED REPLICATION                      │
│  ┌─────────────┬─────────────┬─────────────────────┐   │
│  │ SQL Replay  │ Page Sync   │ Safety Net          │   │
│  │ (<30s)      │ (30s-10m)   │ (>10m + periodic)   │   │
│  └─────────────┴─────────────┴─────────────────────┘   │
└─────────────────────────────────────────────────────────┘
                            ↓
┌─────────────────────────────────────────────────────────┐
│                   ZMQ TRANSPORT                         │
│  ┌─────────────┬─────────────┬─────────────────────┐   │
│  │ PUB/SUB     │ REQ/REP     │ PUSH/PULL           │   │
│  │ (fast diffs)│ (small sync)│ (bulk operations)   │   │
│  └─────────────┴─────────────┴─────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### **Primary Election & Write Flow**

1. **📊 Lease System** (ZMQ REQ/REP): Determines who can write
2. **✍️ Write Capture**: Primary records changes via VFS interception  
3. **🎯 Strategy Decision**: Layered replicator chooses optimal sync method
4. **📡 Transport Execution**: ZMQ patterns execute the chosen strategy

---

## 🕐 **Your 5-Minute Scenario Breakdown**

### **Timeline Analysis**
```
Time: 0s ──────── 10s ──────── 20s ──────── 5m
      │           │           │           │
      ├─ 5 small  ├─ 3 small  ├─ 4 small  ├─ Analysis
      │  writes   │  writes   │  writes   │
      │           │           │           │
   SQL Replay  SQL Replay  SQL Replay  Page Sync
   (PUB/SUB)   (PUB/SUB)   (PUB/SUB)   (HTTP/rsync)
```

### **Decision Logic Flow**

```go
// Every 5-10 seconds: Small batch triggers SQL Replay
if timeSinceSync < 30*time.Second && 
   changeCount <= 50 && 
   totalSize <= 64*1024 {
    
    // ✅ Use ZMQ PUB/SUB for WAL diff broadcasting
    return StrategyTxLog  // 40-80x bandwidth advantage
}

// After 5 minutes: Accumulated changes trigger Page Sync  
if timeSinceSync > 30*time.Second && totalSize < bulkThreshold {
    if changeCount > 20 {
        triggerCheckpoint()  // Improve page locality
    }
    
    // ✅ Use HTTP rsync or PUSH/PULL for page-level sync
    return StrategyPageSync  // 5-15x bandwidth advantage
}
```

---

## 📡 **ZMQ Communication Patterns by Strategy**

### **1. 🚀 SQL Replay (Fast Path) - PUB/SUB**

**When**: Small writes every 5-10 seconds in your scenario

```
Primary Node                    Replica Nodes
     │                               │
     ├─ VFS captures write           │
     ├─ Extract SQL statement        │
     ├─ Publish via ZMQ PUB ────────→ ZMQ SUB receives
     │                               ├─ Apply SQL batch
     │                               └─ ACK (optional)
```

**ZMQ Pattern**: `PUB/SUB` 
- **Publisher**: Primary broadcasts SQL changes
- **Subscribers**: All replicas receive and apply
- **Topic**: Database path (e.g., `"/app/data/users.db"`)
- **Frequency**: Real-time (milliseconds after write)

### **2. 📄 Page Sync (Medium Path) - Multiple Patterns**

**When**: After 5 minutes of accumulated small writes

```
Primary Node                    Replica Nodes
     │                               │
     ├─ Checkpoint WAL               │
     ├─ Calculate page hashes        │
     ├─ HTTP rsync request ─────────→ Compare hashes
     │                               ├─ Return diff list
     ├─ Send changed pages ─────────→ Apply pages
     │                               └─ Verify integrity
```

**ZMQ Pattern**: `REQ/REP` or `HTTP` (hybrid approach)
- **Small page sync**: ZMQ REQ/REP for coordination
- **Bulk transfer**: HTTP rsync for actual data
- **Verification**: ZMQ DEALER/ROUTER for health checks

### **3. 🛡️ Safety Net (Verification) - ROUTER/DEALER**

**When**: Periodic verification (configurable, 1 hour default)

```
Coordinator                     All Nodes
     │                               │
     ├─ Health check request ───────→ Status report
     ├─ Consistency verification ───→ Hash comparison
     ├─ Auto-repair trigger ────────→ Force page sync
     │                               └─ Repair complete
```

**ZMQ Pattern**: `ROUTER/DEALER`
- **Router**: Coordinator manages cluster state
- **Dealers**: Nodes report status and receive commands
- **Purpose**: Catch anything the fast paths missed

---

## 🎛️ **What Determines Sync Frequency?**

### **Configuration-Driven Decision Matrix**

| **Factor** | **5-10s Intervals** | **5-Minute Mark** | **1-Hour Mark** |
|------------|---------------------|-------------------|-----------------|
| **Time Since Last Sync** | <30s | >30s | >1h |
| **Change Count** | 3-5 ops | 50+ ops accumulated | Any |
| **Data Size** | <64KB | <1MB total | Any |
| **Strategy Chosen** | **SQL Replay** | **Page Sync** | **Safety Net** |
| **ZMQ Pattern** | **PUB/SUB** | **REQ/REP + HTTP** | **ROUTER/DEALER** |
| **Bandwidth Advantage** | **40-80x** | **5-15x** | **2-5x** |

### **Your Scenario Specifically**

```go
// Configure for your workload
config := replication.HybridReplicatorConfig{
    // Frequent small writes → Optimize for SQL replay
    MaxDelay:             5 * time.Second,  // React quickly to small batches
    SQLReplayMaxOps:      20,               // Allow up to 20 ops for SQL replay
    SQLReplayMaxSize:     32 * 1024,        // 32KB threshold
    
    // Medium aggregation → Page sync after accumulation
    BulkThreshold:        512 * 1024,       // 512KB before bulk sync
    
    // Safety net → Catch anything missed
    VerificationInterval: 1 * time.Hour,    // Your requested 1-hour check
    EnableQuickVerify:    true,             // Enable for critical systems
    QuickVerifyInterval:  5 * time.Minute,  // Check active DBs every 5min
}
```

---

## 🔄 **Complete Flow for Your Scenario**

### **Phase 1: Real-time Replication (0-30s)**

```
Write #1 (5s) → VFS Capture → SQL Extract → ZMQ PUB → All Replicas Apply
Write #2 (10s) → VFS Capture → SQL Extract → ZMQ PUB → All Replicas Apply  
Write #3 (15s) → VFS Capture → SQL Extract → ZMQ PUB → All Replicas Apply
...
```

**Transport**: ZMQ `PUB/SUB` pattern
**Latency**: ~1-5ms per operation
**Bandwidth**: 40-80x advantage over full sync

### **Phase 2: Accumulated Sync (5-minute mark)**

```
Timer Trigger (5m) → Checkpoint WAL → Page Hash Calculation → 
  HTTP rsync negotiation → Send only changed pages → Verify consistency
```

**Transport**: HTTP rsync with ZMQ coordination
**Latency**: ~100-500ms total
**Bandwidth**: 5-15x advantage (only changed pages)

### **Phase 3: Safety Net (1-hour mark)**

```
Verification Timer → ZMQ ROUTER broadcast → All nodes report status →
  Hash comparison → Auto-repair if needed → Health monitoring update
```

**Transport**: ZMQ `ROUTER/DEALER` pattern
**Purpose**: Catch any missed inconsistencies
**Action**: Automatic repair using page sync

---

## 🚀 **ZMQ Patterns Deep Dive**

### **PUB/SUB for SQL Replay** 
```go
// Primary publishes WAL changes
pub.Send(topic, sqlBatch)  // topic = "/db/users.db"

// Replicas subscribe and apply  
sub.SetSubscribe("/db/users.db")
batch := sub.Receive()
applyBatchTransactionally(batch)
```

**Advantages**:
- ✅ **Fanout**: One publish reaches all replicas
- ✅ **Low latency**: ~1ms propagation
- ✅ **Bandwidth efficient**: Only SQL, not data pages
- ✅ **Fire-and-forget**: No coordination overhead

### **REQ/REP for Coordination**
```go
// Page sync coordination
req := PageSyncRequest{Database: dbPath, LastSequence: seq}
resp := client.Send(coordinator, req)  // REQ/REP

// Returns: which pages need sync
return resp.ChangedPages
```

**Advantages**:
- ✅ **Reliability**: Guaranteed request/response
- ✅ **Coordination**: Prevents sync conflicts
- ✅ **Error handling**: Explicit success/failure

### **ROUTER/DEALER for Health**
```go
// Coordinator health check
coordinator.Route(nodeID, HealthCheckRequest{})

// Node responds with status
dealer.Send(HealthCheckResponse{Status: "healthy", LastSync: timestamp})
```

**Advantages**:
- ✅ **Scalable**: Router handles many dealers
- ✅ **Addressed**: Direct node-to-node communication  
- ✅ **Async**: Non-blocking health monitoring

---

## 🎯 **Summary: Your 5-Minute Write Pattern**

**What happens in your scenario**:

1. **Every 5-10 seconds**: Small writes trigger **SQL Replay** via **ZMQ PUB/SUB**
   - 40-80x bandwidth advantage
   - ~1ms replication latency
   - All replicas receive and apply immediately

2. **At 5-minute mark**: Accumulated changes trigger **Page Sync** via **HTTP rsync**
   - 5-15x bandwidth advantage  
   - Only changed pages transferred
   - Checkpoint optimization improves efficiency

3. **Every hour**: **Safety Net Verification** via **ZMQ ROUTER/DEALER**
   - Comprehensive consistency check
   - Auto-repair any missed changes
   - Health monitoring and metrics

**The layered system gives you the best of all worlds**: fast real-time sync for small changes, efficient page-level sync for medium batches, and bulletproof safety net for long-term consistency! 🚀