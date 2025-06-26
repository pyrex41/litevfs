# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DistSQLite is a distributed SQLite platform that provides zero-code-change distribution for any SQLite application. It uses VFS interception instead of FUSE for maximum performance and handles bulk operations 10-100x faster than Turso while maintaining simplicity.

## Common Development Commands

### Building
```bash
make build          # Build both sidecar and injection library
make deps           # Install dependencies and fetch SQLite binaries
make clean          # Clean build artifacts
make install        # Install to system directories
```

### Testing
```bash
make test           # Run unit tests
make dev-test       # Run integration test with LD_PRELOAD
go test -v ./...    # Run Go tests only
cd test/integration && go test -v .  # Integration tests only
```

### Development
```bash
make lint           # Run linters (golangci-lint, clang-format)
make dev-consul     # Start Consul in dev mode for testing
make benchmark      # Run performance benchmarks against Turso
```

## Architecture Overview

### Core Components
- **SQLite VFS Layer** (`pkg/vfs/vfs.c`) - Direct SQLite VFS interception for transparent operation
- **Sidecar Process** (`cmd/distsqlite/`) - Go binary handling replication and coordination
- **Lease Management** (`pkg/lease/consul.go`) - Consul-based write coordination and failover
- **Hybrid Replication** (`pkg/replication/hybrid.go`) - Smart strategy selection (tx logs vs bulk sync)
- **sqlite3_rsync Wrapper** (`pkg/rsync/wrapper.go`) - Optimized bulk synchronization
- **LD_PRELOAD Injection** (`cmd/distsqlite-inject/inject.c`) - Zero-change integration

### Key Design Decisions
- **VFS instead of FUSE**: Direct SQLite VFS interception avoids FUSE overhead (90% performance penalty)
- **Hybrid Replication**: Small changes use transaction logs, large changes (≥1MB) use sqlite3_rsync
- **Zero-Change Integration**: LD_PRELOAD automatically intercepts all SQLite calls
- **Consul Leasing**: Battle-tested lease management borrowed from LiteFS patterns

## Dependencies and Setup

### Required Tools
- Go 1.21+
- gcc with SQLite development headers
- SQLite binaries (automatically fetched via `fetch_sqlite_binaries.sh`)
- Consul (for lease management)

### Key Dependencies
- `github.com/hashicorp/consul/api` - Consul integration
- `github.com/spf13/cobra` - CLI framework
- `go.uber.org/zap` - Structured logging

## Usage Patterns

### Zero-Change Integration
Any SQLite application can be made distributed without code changes:
```bash
LD_PRELOAD=./build/distsqlite-inject.so your-sqlite-app
```

### Configuration
Uses YAML configuration file (`distsqlite.yml`) for cluster setup:
- Hostname identification
- Consul URL for coordination
- Target nodes for replication

### Performance Optimization
- Bulk operations automatically trigger sqlite3_rsync for 10-100x speed improvement
- Local SQLite performance maintained with background replication
- Smart threshold detection (1MB default) for replication strategy selection

## Testing Strategy

Integration tests validate the complete flow:
- VFS interception working correctly
- Sidecar process auto-starting
- Basic database operations functioning
- Bulk insert optimization triggering

The test suite includes both unit tests for individual components and integration tests that validate the full LD_PRELOAD workflow.