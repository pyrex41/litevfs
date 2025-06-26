# DistSQLite Test Suite

This directory contains a comprehensive test suite for DistSQLite, covering unit tests, integration tests, performance benchmarks, and error injection scenarios.

## Test Structure

```
test/
├── unit/                    # Unit tests for individual components
│   ├── vfs_test.c          # VFS layer unit tests (C)
│   ├── lease_test.go       # Lease management tests
│   └── ipc_test.go         # IPC/shared memory tests
├── integration/            # Integration tests for full system
│   ├── split_brain_test.go # Split-brain prevention tests
│   ├── concurrent_test.go  # Concurrent access scenarios
│   ├── error_injection_test.go # Network/corruption failures
│   └── simple_demo.go      # Basic integration demo
├── benchmarks/             # Performance benchmarks
│   └── performance_test.go # Bulk operation benchmarks
├── config.yaml            # Test configuration and thresholds
├── run_tests.sh           # Comprehensive test runner
└── README.md              # This file
```

## Running Tests

### All Tests
```bash
make test                   # Run complete test suite
./test/run_tests.sh        # Alternative direct execution
```

### Specific Test Categories
```bash
make test-unit             # Unit tests only
make test-integration      # Integration tests only
make test-benchmarks       # Performance benchmarks only
make test-vfs             # VFS C unit tests only
```

### Advanced Testing
```bash
make test-coverage         # Generate coverage report
make test-race            # Run with race detection
make test-stress          # Stress test with multiple iterations
```

## Test Categories

### 1. Unit Tests

#### VFS Layer Tests (`vfs_test.c`)
- VFS registration and method implementation
- Basic database operations (create, read, write)
- WAL mode functionality
- File locking mechanisms
- Large file operations
- Concurrent read scenarios

#### Lease Management Tests (`lease_test.go`)
- Basic lease acquisition and release
- Multi-node competition for leases
- Rapid failover scenarios (sub-second)
- Concurrent lease attempts
- File corruption handling
- Coordinator integration

#### IPC Tests (`ipc_test.go`)
- Shared memory structure layout
- Circular buffer operations
- Concurrent producer/consumer scenarios
- Performance benchmarks (>100K events/sec)
- Memory alignment verification
- Cleanup and shutdown procedures

### 2. Integration Tests

#### Split-Brain Prevention (`split_brain_test.go`)
- **TestSplitBrainPrevention**: Ensures only one node can write
- **TestFenceTokenValidation**: Validates fence token increments
- **TestPartitionRecovery**: Tests recovery from network partitions
- **TestConcurrentLeaseAttempts**: Many nodes competing simultaneously

#### Concurrent Access (`concurrent_test.go`)
- **TestConcurrentWriters**: Multiple writers with proper serialization
- **TestReadWriteInteraction**: Readers don't block writers in WAL mode
- **TestTransactionIsolation**: ACID properties under concurrent access

#### Error Injection (`error_injection_test.go`)
- **TestDatabaseCorruption**: Handling corrupted database files
- **TestNetworkFailures**: HTTP rsync under network conditions
- **TestDiskSpaceExhaustion**: Behavior when disk is full
- **TestConcurrentFailures**: Multiple simultaneous failures

### 3. Performance Benchmarks (`performance_test.go`)

#### Bulk Operations
- Small datasets (1K rows, 100B each)
- Medium datasets (10K rows, 1KB each)
- Large datasets (100K rows, 100B each)
- Huge datasets (1M rows, 100B each)

#### Comparison Metrics
- **Baseline SQLite**: Raw file copy performance
- **HTTP Rsync**: sqlite3-rsync algorithm over HTTP
- **SSH Rsync**: Traditional SSH transport
- **Hybrid Replication**: Strategy selection validation

#### Concurrency Benchmarks
- 1, 2, 4, 8, 16 concurrent writers
- Transaction throughput measurement
- Lock contention analysis

## Performance Targets

Based on the "10-100x faster than Turso" claim:

| Dataset Size | Expected Speedup | Target Use Case |
|-------------|------------------|-----------------|
| Small (100KB) | 10x | Small database sync |
| Medium (10MB) | 50x | Moderate bulk operations |
| Large (100MB+) | 100x | Large dataset replication |

## Test Configuration

The `config.yaml` file contains:
- Performance thresholds and targets
- Test timeouts and retry limits
- Database configurations (WAL, DELETE, MEMORY modes)
- Network simulation parameters
- Error injection settings

## Expected Test Results

### Unit Tests
- All VFS operations should work correctly
- Lease failover should complete in <1 second
- IPC should handle >100K events/second

### Integration Tests
- Zero split-brain scenarios detected
- All concurrent operations maintain ACID properties
- System gracefully handles corruption and network failures

### Performance Benchmarks
- HTTP rsync should significantly outperform baseline for large datasets
- Concurrent writers should scale linearly up to hardware limits
- Memory overhead should remain under 50MB

## Troubleshooting

### Common Issues

#### SQLite Headers Missing
```bash
# Ubuntu/Debian
sudo apt-get install libsqlite3-dev

# macOS
brew install sqlite3
```

#### Shared Memory Cleanup
```bash
make ipc-clean               # Clean up shared memory segments
make ipc-status             # Check current IPC status
```

#### Permission Issues
```bash
# Ensure test files are executable
chmod +x test/run_tests.sh
```

### Environment Dependencies

1. **Go 1.21+** - Required for all Go tests
2. **GCC** - Required for VFS C tests
3. **SQLite Dev Headers** - Required for C compilation
4. **Make** - Required for build targets

### Test Data Cleanup

Tests create temporary databases and files. These are automatically cleaned up, but manual cleanup can be done:

```bash
rm -rf /tmp/distsqlite-test-*
make clean
```

## Adding New Tests

### Unit Tests
1. Add test functions to appropriate `*_test.go` files
2. Follow existing naming conventions (`TestFunctionName`)
3. Include both positive and negative test cases

### Integration Tests
1. Create new test files in `test/integration/`
2. Use `package main` for integration tests
3. Include realistic failure scenarios

### Benchmarks
1. Add benchmark functions to `test/benchmarks/performance_test.go`
2. Use `func BenchmarkName(b *testing.B)` signature
3. Include memory and allocation metrics

## Contributing

When adding tests:
1. Ensure tests are deterministic and repeatable
2. Include appropriate timeouts for long-running tests
3. Clean up resources (files, connections) in defer statements
4. Document expected behavior and failure conditions
5. Update this README with new test descriptions