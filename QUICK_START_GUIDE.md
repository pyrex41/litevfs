# DistSQLite Quick Start Guide

## 🚀 Ready to Use!

The DistSQLite distributed SQLite system has been successfully built and tested. Here's how to use it:

## 📦 Built Components

All binaries are located in the `build/` directory:

```bash
build/
├── distsqlite                 # Main sidecar (21MB)
├── distsqlite-coordinator     # HTTP coordinator (15MB) 
├── distsqlite-leaseserver     # Lease server (8.7MB)
├── distsqlite-inject.so       # LD_PRELOAD library (29KB)
└── libdistcore.so            # OCaml performance core (34KB)
```

## 🎯 Option 1: Run Demo (Quickest)

Start the working demonstration:

```bash
# Run the simple 2-node demo
./simple_demo.sh

# Or run the comprehensive 3-node test
./test_multinode.sh
```

## 🎯 Option 2: Manual Setup

### Step 1: Start the Coordinator

```bash
LD_LIBRARY_PATH=./build ./build/distsqlite-coordinator --addr :9090
```

### Step 2: Start Nodes

**Node 1 (Primary):**
```bash
mkdir -p data1
LD_LIBRARY_PATH=./build ./build/distsqlite \
    --data-dir "./data1" \
    --http-addr ":8080" \
    --coordination "http" \
    --coordinator-url "http://localhost:9090" \
    --hostname "node1" \
    --targets "localhost:8081"
```

**Node 2 (Secondary):**
```bash
mkdir -p data2  
LD_LIBRARY_PATH=./build ./build/distsqlite \
    --data-dir "./data2" \
    --http-addr ":8081" \
    --coordination "http" \
    --coordinator-url "http://localhost:9090" \
    --hostname "node2" \
    --targets "localhost:8080"
```

### Step 3: Test with an Application

Create a simple test application:

```go
// test_app.go
package main

import (
    "database/sql"
    "fmt"
    "log"
    _ "github.com/mattn/go-sqlite3"
)

func main() {
    db, err := sql.Open("sqlite3", "./test.db")
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()
    
    _, err = db.Exec("CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, name TEXT)")
    if err != nil {
        log.Fatal(err)
    }
    
    _, err = db.Exec("INSERT INTO users (name) VALUES (?)", "Alice")
    if err != nil {
        log.Fatal(err)
    }
    
    fmt.Println("✅ Data inserted successfully via DistSQLite!")
}
```

Run with LD_PRELOAD:

```bash
# Set up Go module
go mod init testapp
go get github.com/mattn/go-sqlite3

# Run with DistSQLite injection
LD_PRELOAD=./build/distsqlite-inject.so \
LD_LIBRARY_PATH=./build \
go run test_app.go
```

## 🔍 Monitoring

Check cluster status:

```bash
# Coordinator health
curl http://localhost:9090/health

# Node status  
curl http://localhost:8080/health
curl http://localhost:8081/health

# Node statistics
curl http://localhost:8080/stats
curl http://localhost:8081/stats
```

## 🎮 Available Demos

### 1. Simple Demo
```bash
./simple_demo.sh
```
- Starts 2-node cluster
- Shows cluster formation
- Demonstrates health endpoints
- Runs continuously until Ctrl+C

### 2. Multi-Node Test
```bash
./test_multinode.sh
```
- Starts 3-node cluster
- Runs comprehensive tests
- Shows lease coordination
- Tests LD_PRELOAD functionality

### 3. Integration Demo
```bash
cd test/integration
LD_PRELOAD=../../build/distsqlite-inject.so \
LD_LIBRARY_PATH=../../build \
go run simple_demo.go
```

## 🛠️ Configuration Options

### Coordination Backends
- **HTTP**: `--coordination http --coordinator-url http://localhost:9090`
- **Consul**: `--coordination consul --consul-url http://localhost:8500`

### Key Flags
- `--data-dir`: Directory for SQLite databases
- `--http-addr`: HTTP API listen address  
- `--hostname`: Node identifier
- `--targets`: Comma-separated list of replication targets
- `--advertise-url`: URL for other nodes to reach this node

## 🏗️ Architecture

```
┌─────────────────┐    ┌─────────────────┐
│   Application   │    │   Application   │
│                 │    │                 │
├─────────────────┤    ├─────────────────┤
│  LD_PRELOAD     │    │  LD_PRELOAD     │
│  (distsqlite-   │    │  (distsqlite-   │
│   inject.so)    │    │   inject.so)    │
├─────────────────┤    ├─────────────────┤
│   Node 1        │◄──►│   Node 2        │
│   (Primary)     │    │   (Secondary)   │
│   :8080         │    │   :8081         │
└─────────┬───────┘    └─────────┬───────┘
          │                      │
          └──────────┬───────────┘
                     │
          ┌─────────────────┐
          │  HTTP           │
          │  Coordinator    │
          │  :9090          │
          └─────────────────┘
```

## ✅ What Works

- ✅ Multi-node cluster formation
- ✅ Leader election via HTTP coordination  
- ✅ Health monitoring and status endpoints
- ✅ Lease management with fence tokens
- ✅ LD_PRELOAD injection for zero-code-change integration
- ✅ Automatic lease renewal
- ✅ Graceful shutdown and cleanup

## 🎉 Success!

Your DistSQLite distributed SQLite system is ready for use! Applications can now use distributed SQLite with zero code changes by simply using the LD_PRELOAD technique.