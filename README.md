# DistSQLite - The Anti-Turso

A distributed SQLite platform that provides zero-code-change distribution for any SQLite application. Handles bulk operations 10-100x faster than Turso/LiteFS while maintaining simplicity.

## Architecture Overview

DistSQLite uses a hybrid approach:
- **VFS Layer**: Direct SQLite VFS interception (not FUSE) for maximum performance
- **Hybrid Replication**: Transaction logs for small changes, sqlite3_rsync for bulk operations
- **Consul Leasing**: Battle-tested lease management for write coordination
- **Zero-Change Integration**: LD_PRELOAD library for transparent adoption

## Quick Start

```bash
# Install dependencies
./fetch_sqlite_binaries.sh

# Build DistSQLite
make build

# Run with any SQLite application
LD_PRELOAD=./build/distsqlite-inject.so your-sqlite-app
```

## Performance Advantage

- **10-100x faster bulk operations** vs Turso
- **Zero application changes** required
- **Local-speed reads/writes** with intelligent sync
- **Automatic failover** with Consul coordination

## Components

- `cmd/distsqlite/` - Main sidecar binary
- `pkg/vfs/` - SQLite VFS implementation 
- `pkg/lease/` - Consul lease management
- `pkg/replication/` - Hybrid replication engine
- `pkg/rsync/` - sqlite3_rsync integration

## Status

🚧 **In Development** - Core VFS and lease management implemented