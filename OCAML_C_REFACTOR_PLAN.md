# OCaml/C Integration Refactoring Plan

## Executive Summary

This document outlines the comprehensive refactoring needed to address the critical OCaml/C/Go integration issues identified in the code review. The current implementation has several fundamental problems that make it fragile and unmaintainable.

## Critical Issues Identified

### 🚨 Critical Priority

1. **Fragile Build System**
   - Current: Manual `bash` commands in `dune` files
   - Problem: Non-standard, brittle, will break with OCaml version changes
   - Impact: Unreliable builds, difficult maintenance

2. **Manual OCaml Runtime Symbol Definitions**
   - Current: Defining `caml_apply2`, `caml_exn_Failure`, etc. in C
   - Problem: Indicates serious linking issues with OCaml runtime
   - Impact: Will break with minor OCaml version changes

3. **Overly Complex C Stubs**
   - Current: 1365+ lines of complex logic in `distcore_stubs.c`
   - Problem: Complex business logic in FFI layer
   - Impact: Hard to debug, security risks, maintenance nightmare

### 🤷‍♂️ High Priority

4. **Unsafe IPC Mechanism**
   - Current: Raw syscalls and `unsafe.Pointer` for shared memory
   - Problem: Linux-only, memory corruption risks
   - Impact: Portability and stability issues

## Refactoring Strategy

### Phase 1: Standardize Build System (Critical)

**Goal**: Replace manual build scripts with proper dune configuration

**Current State**:
```ocaml
(rule
 (targets libdistcore.so)
 (deps distcore_stubs.c distcore_main.c distcore.cma distcore.cmxa)
 (action
  (bash "gcc -fPIC -O3 ... ocamlmklib ...")))
```

**Target State**:
```ocaml
(library
 (public_name distsqlite-core)
 (name distcore)
 (modules distcore lock_free_queue binary_diff replication lease_state_machine protocol_constants)
 (libraries threads unix bigarray-compat)
 (foreign_stubs
  (language c)
  (names distcore_stubs_minimal)
  (flags -O3 -fPIC -DCAML_NAME_SPACE))
 (foreign_archives sqlite3))
```

### Phase 2: Minimize C Stubs (Critical)

**Goal**: Move all business logic to OCaml, keep C stubs for data conversion only

**Current**: 1365 lines of C with VFS logic, queue management, etc.
**Target**: ~200 lines of pure data conversion

**Architecture**:
```
Go Application
    ↓ (Unix Domain Sockets)
OCaml Core Engine
    ↓ (Minimal C Stubs - data conversion only)
SQLite VFS Layer
```

### Phase 3: Replace Unsafe IPC (High Priority)

**Goal**: Replace shared memory with safer, portable mechanism

**Options**:
1. **Unix Domain Sockets** (Recommended)
2. **Named Pipes**  
3. **ZeroMQ inproc transport**

### Phase 4: Consolidate FFI Boundaries

**Goal**: Single, consistent approach to OCaml/C integration

## Implementation Plan

### Step 1: Create New Minimal C Stubs

Create `src/distcore_stubs_minimal.c` with only essential FFI functions:

```c
// ONLY data conversion - no business logic
extern value distcore_write_event_of_c(write_event_c* event);
extern void distcore_write_event_to_c(value ml_event, write_event_c* event);
extern value distcore_create_bigarray_from_ptr(void* ptr, size_t len);
```

### Step 2: Move VFS Logic to OCaml

Create `src/vfs_intercept.ml` to handle all VFS operations:

```ocaml
module VFS = struct
  type file_handle = {
    path: string;
    fd: Unix.file_descr;
    page_size: int;
    mutable intercept_enabled: bool;
  }
  
  let register_vfs () = ...
  let intercept_write file offset data = ...
  let intercept_read file offset amount = ...
end
```

### Step 3: Replace IPC with Unix Domain Sockets

Create `pkg/ipc/socket.go`:

```go
type SocketIPC struct {
    conn   net.Conn
    encoder *json.Encoder
    decoder *json.Decoder
}

func (s *SocketIPC) SendWriteEvent(event WriteEvent) error {
    return s.encoder.Encode(event)
}
```

### Step 4: Standardize Build Configuration

Update `src/dune`:

```ocaml
(library
 (public_name distsqlite-core)
 (name distcore)
 (modules distcore vfs_intercept lock_free_queue binary_diff replication lease_state_machine protocol_constants)
 (libraries threads unix bigarray-compat)
 (foreign_stubs
  (language c)
  (names distcore_stubs_minimal vfs_sqlite_bridge)
  (flags -O3 -fPIC -DCAML_NAME_SPACE -I/usr/include/sqlite3))
 (foreign_archives sqlite3)
 (c_library_flags -lsqlite3 -lpthread -lm -ldl))

(rule
 (target libdistcore.so)
 (deps (glob_files *.{cma,cmxa,a}) distcore_stubs_minimal.o vfs_sqlite_bridge.o)
 (action
  (run ocamlmklib -o distcore_shared -linkall
    distcore.cma distcore_stubs_minimal.o vfs_sqlite_bridge.o
    -cclib -lsqlite3 -cclib -lpthread -cclib -lm -cclib -ldl)))
```

## File Structure After Refactoring

```
src/
├── distcore.ml                 # Main OCaml API
├── vfs_intercept.ml            # VFS logic (moved from C)
├── ipc_socket.ml               # Socket-based IPC
├── distcore_stubs_minimal.c    # Minimal C stubs (~200 lines)
├── vfs_sqlite_bridge.c         # Thin SQLite VFS bridge
├── lock_free_queue.ml          # Pure OCaml queue
├── binary_diff.ml              # Pure OCaml diff
├── replication.ml              # Pure OCaml replication
└── dune                        # Standard dune config

pkg/ipc/
├── socket.go                   # Unix domain socket IPC
└── shm.go.disabled             # Old shared memory (disabled)
```

## Migration Steps

### Phase 1: Build System (Week 1)
1. Create `distcore_stubs_minimal.c` with essential functions only
2. Update `src/dune` to use `foreign_stubs` properly
3. Test that OCaml library builds without manual bash commands
4. Verify all OCaml runtime symbols are linked correctly

### Phase 2: Move Logic to OCaml (Week 2)
1. Create `vfs_intercept.ml` with VFS logic
2. Create `ipc_socket.ml` for socket communication
3. Update `distcore.ml` to use new modules
4. Test that functionality is preserved

### Phase 3: Replace IPC (Week 3)
1. Implement `pkg/ipc/socket.go`
2. Create socket server in OCaml (`ipc_socket.ml`)
3. Test Go ↔ OCaml communication via sockets
4. Performance test vs. current shared memory

### Phase 4: Integration & Testing (Week 4)
1. Update Go code to use socket IPC
2. Run full integration test suite
3. Performance benchmarking
4. Clean up old files

## Success Criteria

- [ ] Build uses standard dune configuration (no bash commands)
- [ ] No manual OCaml runtime symbol definitions
- [ ] C stubs under 300 lines total
- [ ] IPC works on macOS and Linux
- [ ] Performance within 10% of current implementation
- [ ] All existing tests pass
- [ ] Memory-safe (no `unsafe.Pointer` in Go)

## Risk Mitigation

1. **Performance Regression**: Benchmark each phase
2. **Compatibility Issues**: Test on multiple OCaml versions (4.14, 5.0, 5.1)
3. **Integration Complexity**: Maintain parallel implementation during migration
4. **Socket vs SharedMem Performance**: Benchmark thoroughly before switching

## Files to Modify

### Critical Priority
- [ ] `src/dune` - Standardize build system
- [ ] `src/distcore_stubs.c` - Replace with minimal version
- [ ] `src/distcore.ml` - Update to use new architecture

### High Priority  
- [ ] `pkg/ipc/shm.go` - Replace with socket implementation
- [ ] `pkg/zmq/ocaml_stubs.c` - Consolidate with main stubs

### Medium Priority
- [ ] `dune-project` - Update dependencies
- [ ] Build scripts - Update to use new system

This refactoring will transform the codebase from a fragile polyglot system to a robust, maintainable architecture that preserves the performance benefits while eliminating the stability risks.