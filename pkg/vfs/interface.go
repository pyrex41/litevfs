package vfs

/*
#cgo LDFLAGS: -lpthread -ldl -lrt
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <sys/mman.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>

// Forward declarations matching the C VFS
typedef void (*OnWriteCallback)(const char* dbPath, const void* data, int size, long long offset);
typedef void (*OnSyncCallback)(const char* dbPath, int flags);
typedef void (*OnTruncateCallback)(const char* dbPath, long long size);

// External C functions
extern int distVFSInit(void);
extern int distVFSShutdown(void);
extern int distVFSRegisterCallbacks(OnWriteCallback onWrite, OnSyncCallback onSync, OnTruncateCallback onTruncate);

// Shared memory structure (must match C definition)
typedef struct SharedMemory {
    pthread_mutex_t mutex;
    pthread_cond_t cond;
    volatile int pendingOps;
    volatile int maxOps;
    char operations[1024][512]; // Ring buffer for operations
    volatile int head;
    volatile int tail;
} SharedMemory;

// Callback implementations that will call Go functions
void goOnWrite(const char* dbPath, const void* data, int size, long long offset);
void goOnSync(const char* dbPath, int flags);
void goOnTruncate(const char* dbPath, long long size);

static inline SharedMemory* getSharedMemory() {
    int fd = shm_open("/distsqlite_ipc", O_RDWR, 0666);
    if (fd < 0) return NULL;
    
    SharedMemory* mem = mmap(NULL, sizeof(SharedMemory), PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    close(fd);
    
    if (mem == MAP_FAILED) return NULL;
    return mem;
}

static inline void releaseSharedMemory(SharedMemory* mem) {
    if (mem) {
        munmap(mem, sizeof(SharedMemory));
    }
}

static inline int consumeOperation(SharedMemory* mem, char* operation) {
    if (!mem) return 0;
    
    pthread_mutex_lock(&mem->mutex);
    
    if (mem->pendingOps == 0) {
        pthread_mutex_unlock(&mem->mutex);
        return 0;
    }
    
    // Copy operation from ring buffer
    strncpy(operation, mem->operations[mem->tail], 511);
    operation[511] = '\0';
    
    mem->tail = (mem->tail + 1) % mem->maxOps;
    mem->pendingOps--;
    
    pthread_mutex_unlock(&mem->mutex);
    return 1;
}
*/
import "C"
import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// Operation represents a database operation intercepted by the VFS
type Operation struct {
	Type     string    // "WRITE", "SYNC", "TRUNCATE"
	Database string    // Database file path
	Offset   int64     // For WRITE operations
	Size     int       // For WRITE operations
	Flags    int       // For SYNC operations
	Time     time.Time // When operation occurred
	Data     []byte    // Write data (if available)
}

// WriteHandler handles write operations
type WriteHandler func(dbPath string, data []byte, size int, offset int64)

// SyncHandler handles sync operations
type SyncHandler func(dbPath string, flags int)

// TruncateHandler handles truncate operations
type TruncateHandler func(dbPath string, size int64)

// Interface provides the Go interface to the DistSQLite VFS
type Interface struct {
	mu              sync.RWMutex
	initialized     bool
	sharedMem       *C.SharedMemory
	
	// Event handlers
	writeHandler    WriteHandler
	syncHandler     SyncHandler
	truncateHandler TruncateHandler
	
	// Background processing
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	
	// Operation channel for processing
	operations      chan Operation
}

// Global instance for callbacks
var globalInterface *Interface

// NewInterface creates a new VFS interface
func NewInterface() *Interface {
	ctx, cancel := context.WithCancel(context.Background())
	
	return &Interface{
		ctx:        ctx,
		cancel:     cancel,
		operations: make(chan Operation, 1000), // Buffered channel for operations
	}
}

// Initialize sets up the VFS interface and starts background processing
func (iface *Interface) Initialize() error {
	iface.mu.Lock()
	defer iface.mu.Unlock()
	
	if iface.initialized {
		return nil
	}
	
	// Set global instance for callbacks
	globalInterface = iface
	
	// Initialize the C VFS
	if result := C.distVFSInit(); result != 0 {
		return fmt.Errorf("failed to initialize DistSQLite VFS: %d", result)
	}
	
	// Register C callbacks
	if result := C.distVFSRegisterCallbacks(
		C.OnWriteCallback(C.goOnWrite),
		C.OnSyncCallback(C.goOnSync),
		C.OnTruncateCallback(C.goOnTruncate),
	); result != 0 {
		C.distVFSShutdown()
		return fmt.Errorf("failed to register VFS callbacks: %d", result)
	}
	
	// Get shared memory handle
	iface.sharedMem = C.getSharedMemory()
	if iface.sharedMem == nil {
		C.distVFSShutdown()
		return fmt.Errorf("failed to access shared memory")
	}
	
	// Start background workers
	iface.wg.Add(2)
	go iface.sharedMemoryWorker()
	go iface.operationProcessor()
	
	iface.initialized = true
	log.Printf("[DistSQLite VFS] Interface initialized successfully")
	
	return nil
}

// Shutdown cleans up the VFS interface
func (iface *Interface) Shutdown() error {
	iface.mu.Lock()
	defer iface.mu.Unlock()
	
	if !iface.initialized {
		return nil
	}
	
	// Cancel background workers
	iface.cancel()
	iface.wg.Wait()
	
	// Release shared memory
	if iface.sharedMem != nil {
		C.releaseSharedMemory(iface.sharedMem)
		iface.sharedMem = nil
	}
	
	// Shutdown C VFS
	C.distVFSShutdown()
	
	// Close operations channel
	close(iface.operations)
	
	iface.initialized = false
	globalInterface = nil
	
	log.Printf("[DistSQLite VFS] Interface shutdown complete")
	return nil
}

// SetWriteHandler sets the handler for write operations
func (iface *Interface) SetWriteHandler(handler WriteHandler) {
	iface.mu.Lock()
	defer iface.mu.Unlock()
	iface.writeHandler = handler
}

// SetSyncHandler sets the handler for sync operations
func (iface *Interface) SetSyncHandler(handler SyncHandler) {
	iface.mu.Lock()
	defer iface.mu.Unlock()
	iface.syncHandler = handler
}

// SetTruncateHandler sets the handler for truncate operations
func (iface *Interface) SetTruncateHandler(handler TruncateHandler) {
	iface.mu.Lock()
	defer iface.mu.Unlock()
	iface.truncateHandler = handler
}

// GetOperations returns a channel of database operations
func (iface *Interface) GetOperations() <-chan Operation {
	return iface.operations
}

// sharedMemoryWorker processes operations from shared memory
func (iface *Interface) sharedMemoryWorker() {
	defer iface.wg.Done()
	
	ticker := time.NewTicker(10 * time.Millisecond) // Check every 10ms
	defer ticker.Stop()
	
	for {
		select {
		case <-iface.ctx.Done():
			return
		case <-ticker.C:
			iface.processSharedMemoryOperations()
		}
	}
}

// processSharedMemoryOperations reads operations from shared memory
func (iface *Interface) processSharedMemoryOperations() {
	if iface.sharedMem == nil {
		return
	}
	
	operationStr := make([]byte, 512)
	
	for {
		// Try to consume an operation
		if result := C.consumeOperation(iface.sharedMem, (*C.char)(unsafe.Pointer(&operationStr[0]))); result == 0 {
			break // No more operations
		}
		
		// Parse operation
		op := iface.parseOperation(C.GoString((*C.char)(unsafe.Pointer(&operationStr[0]))))
		if op != nil {
			select {
			case iface.operations <- *op:
			case <-iface.ctx.Done():
				return
			default:
				// Channel full, drop operation (should not happen with proper sizing)
				log.Printf("[DistSQLite VFS] Dropped operation: channel full")
			}
		}
	}
}

// parseOperation parses a string operation into an Operation struct
func (iface *Interface) parseOperation(opStr string) *Operation {
	parts := strings.Split(opStr, "|")
	if len(parts) < 2 {
		return nil
	}
	
	op := &Operation{
		Type:     parts[0],
		Database: parts[1],
		Time:     time.Now(),
	}
	
	switch op.Type {
	case "WRITE":
		if len(parts) >= 4 {
			if offset, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				op.Offset = offset
			}
			if size, err := strconv.Atoi(parts[3]); err == nil {
				op.Size = size
			}
		}
	case "SYNC":
		if len(parts) >= 3 {
			if flags, err := strconv.Atoi(parts[2]); err == nil {
				op.Flags = flags
			}
		}
	case "TRUNCATE":
		if len(parts) >= 3 {
			if size, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				op.Offset = size // Reuse offset field for size
			}
		}
	}
	
	return op
}

// operationProcessor processes operations and calls handlers
func (iface *Interface) operationProcessor() {
	defer iface.wg.Done()
	
	for {
		select {
		case <-iface.ctx.Done():
			return
		case op, ok := <-iface.operations:
			if !ok {
				return
			}
			iface.handleOperation(op)
		}
	}
}

// handleOperation processes a single operation
func (iface *Interface) handleOperation(op Operation) {
	iface.mu.RLock()
	defer iface.mu.RUnlock()
	
	switch op.Type {
	case "WRITE":
		if iface.writeHandler != nil {
			iface.writeHandler(op.Database, op.Data, op.Size, op.Offset)
		}
	case "SYNC":
		if iface.syncHandler != nil {
			iface.syncHandler(op.Database, op.Flags)
		}
	case "TRUNCATE":
		if iface.truncateHandler != nil {
			iface.truncateHandler(op.Database, op.Offset) // Offset contains size for truncate
		}
	}
}

// IsInitialized returns true if the VFS interface is initialized
func (iface *Interface) IsInitialized() bool {
	iface.mu.RLock()
	defer iface.mu.RUnlock()
	return iface.initialized
}

// C callback functions (called from C code)

//export goOnWrite
func goOnWrite(dbPath *C.char, data unsafe.Pointer, size C.int, offset C.longlong) {
	if globalInterface == nil {
		return
	}
	
	// Convert C parameters to Go
	goDbPath := C.GoString(dbPath)
	goSize := int(size)
	goOffset := int64(offset)
	
	// Copy data (if we want to store it)
	var goData []byte
	if data != nil && size > 0 {
		goData = C.GoBytes(data, size)
	}
	
	// Call Go handler
	globalInterface.mu.RLock()
	handler := globalInterface.writeHandler
	globalInterface.mu.RUnlock()
	
	if handler != nil {
		handler(goDbPath, goData, goSize, goOffset)
	}
}

//export goOnSync  
func goOnSync(dbPath *C.char, flags C.int) {
	if globalInterface == nil {
		return
	}
	
	goDbPath := C.GoString(dbPath)
	goFlags := int(flags)
	
	globalInterface.mu.RLock()
	handler := globalInterface.syncHandler
	globalInterface.mu.RUnlock()
	
	if handler != nil {
		handler(goDbPath, goFlags)
	}
}

//export goOnTruncate
func goOnTruncate(dbPath *C.char, size C.longlong) {
	if globalInterface == nil {
		return
	}
	
	goDbPath := C.GoString(dbPath)
	goSize := int64(size)
	
	globalInterface.mu.RLock()
	handler := globalInterface.truncateHandler
	globalInterface.mu.RUnlock()
	
	if handler != nil {
		handler(goDbPath, goSize)
	}
}