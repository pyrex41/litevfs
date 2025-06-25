package ipc

import (
	"fmt"
	"log"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Constants matching the C side
const (
	SHM_KEY           = 0x44495354 // "DIST" in hex
	MAX_WRITE_EVENTS  = 1000
	MAX_PATH_LEN      = 512
	MAX_DATA_LEN      = 8192
)

// WriteEvent represents a database write event from the VFS layer
type WriteEvent struct {
	Path        string
	Offset      int64
	Length      int32
	IsWAL       bool
	TimestampNs int64
	Data        []byte
}

// Special offset values for control signals
const (
	OFFSET_FLUSH    = -1 // File close/flush
	OFFSET_BATCH    = -2 // Batch completion
	OFFSET_TRUNCATE = -3 // File truncation
	OFFSET_SYNC     = -4 // File sync
)

// SharedMemoryReader reads write events from shared memory
type SharedMemoryReader struct {
	shmID     int
	shmAddr   uintptr
	shmSize   uintptr
	
	// Callbacks for different event types
	writeCallback    func(WriteEvent) error
	batchCallback    func() error
	syncCallback     func(string) error
	truncateCallback func(string, int64) error
	
	// Shutdown coordination
	shutdown chan struct{}
	wg       sync.WaitGroup
}

// C-compatible structure layout (must match vfs.c)
type writeEventC struct {
	path        [MAX_PATH_LEN]byte
	offset      int64
	length      int32
	isWAL       int32
	timestampNs int64
	data        [MAX_DATA_LEN]byte
}

type sharedMemoryC struct {
	// Semaphores (we'll skip these in Go - use atomic operations)
	mutex        [32]byte // sem_t is typically 32 bytes
	producerSem  [32]byte
	consumerSem  [32]byte
	head         int32
	tail         int32
	shutdown     int32
	_padding     int32 // Alignment
	events       [MAX_WRITE_EVENTS]writeEventC
}

// NewSharedMemoryReader creates a new shared memory reader
func NewSharedMemoryReader() *SharedMemoryReader {
	return &SharedMemoryReader{
		shutdown: make(chan struct{}),
	}
}

// SetWriteCallback sets the callback for regular write events
func (r *SharedMemoryReader) SetWriteCallback(cb func(WriteEvent) error) {
	r.writeCallback = cb
}

// SetBatchCallback sets the callback for batch completion events
func (r *SharedMemoryReader) SetBatchCallback(cb func() error) {
	r.batchCallback = cb
}

// SetSyncCallback sets the callback for sync events
func (r *SharedMemoryReader) SetSyncCallback(cb func(string) error) {
	r.syncCallback = cb
}

// SetTruncateCallback sets the callback for truncate events
func (r *SharedMemoryReader) SetTruncateCallback(cb func(string, int64) error) {
	r.truncateCallback = cb
}

// Start begins reading events from shared memory
func (r *SharedMemoryReader) Start() error {
	// Get shared memory segment
	shmID, _, err := syscall.Syscall(syscall.SYS_SHMGET, SHM_KEY, unsafe.Sizeof(sharedMemoryC{}), 0)
	if int(shmID) == -1 {
		return fmt.Errorf("failed to get shared memory segment: %v", err)
	}
	r.shmID = int(shmID)
	
	// Attach to shared memory
	shmAddr, _, err := syscall.Syscall(syscall.SYS_SHMAT, shmID, 0, 0)
	if int(shmAddr) == -1 {
		return fmt.Errorf("failed to attach to shared memory: %v", err)
	}
	r.shmAddr = shmAddr
	r.shmSize = unsafe.Sizeof(sharedMemoryC{})
	
	log.Printf("[DistSQLite IPC] Connected to shared memory (ID: %d, Addr: 0x%x)", r.shmID, r.shmAddr)
	
	// Start background reader
	r.wg.Add(1)
	go r.readerLoop()
	
	return nil
}

// Stop stops reading from shared memory
func (r *SharedMemoryReader) Stop() error {
	close(r.shutdown)
	r.wg.Wait()
	
	// Detach from shared memory
	if r.shmAddr != 0 {
		_, _, _ = syscall.Syscall(syscall.SYS_SHMDT, r.shmAddr, 0, 0)
		r.shmAddr = 0
	}
	
	log.Printf("[DistSQLite IPC] Disconnected from shared memory")
	return nil
}

// readerLoop continuously reads events from shared memory
func (r *SharedMemoryReader) readerLoop() {
	defer r.wg.Done()
	
	ticker := time.NewTicker(1 * time.Millisecond) // Check every 1ms
	defer ticker.Stop()
	
	for {
		select {
		case <-r.shutdown:
			return
		case <-ticker.C:
			r.processEvents()
		}
	}
}

// processEvents processes all available events in the shared memory buffer
func (r *SharedMemoryReader) processEvents() {
	if r.shmAddr == 0 {
		return
	}
	
	shm := (*sharedMemoryC)(unsafe.Pointer(r.shmAddr))
	
	// Read head and tail atomically
	head := atomicLoadInt32(&shm.head)
	tail := atomicLoadInt32(&shm.tail)
	
	// Process all available events
	for head != tail {
		event := &shm.events[head]
		
		// Convert C event to Go event
		goEvent := r.convertEvent(event)
		
		// Dispatch event based on type
		if err := r.dispatchEvent(goEvent); err != nil {
			log.Printf("[DistSQLite IPC] Error processing event: %v", err)
		}
		
		// Advance head pointer
		head = (head + 1) % MAX_WRITE_EVENTS
		atomicStoreInt32(&shm.head, head)
		
		// Update local head for next iteration
		head = atomicLoadInt32(&shm.head)
		tail = atomicLoadInt32(&shm.tail)
	}
}

// convertEvent converts a C event structure to a Go event
func (r *SharedMemoryReader) convertEvent(event *writeEventC) WriteEvent {
	// Convert path from C string
	pathBytes := (*[MAX_PATH_LEN]byte)(unsafe.Pointer(&event.path[0]))
	pathLen := 0
	for i, b := range pathBytes {
		if b == 0 {
			pathLen = i
			break
		}
	}
	path := string(pathBytes[:pathLen])
	
	// Convert data
	var data []byte
	if event.length > 0 && event.length <= MAX_DATA_LEN {
		dataBytes := (*[MAX_DATA_LEN]byte)(unsafe.Pointer(&event.data[0]))
		data = make([]byte, event.length)
		copy(data, dataBytes[:event.length])
	}
	
	return WriteEvent{
		Path:        path,
		Offset:      event.offset,
		Length:      event.length,
		IsWAL:       event.isWAL != 0,
		TimestampNs: event.timestampNs,
		Data:        data,
	}
}

// dispatchEvent routes an event to the appropriate callback
func (r *SharedMemoryReader) dispatchEvent(event WriteEvent) error {
	switch event.Offset {
	case OFFSET_FLUSH:
		// File flush/close event
		log.Printf("[DistSQLite IPC] File flush: %s", event.Path)
		return nil
		
	case OFFSET_BATCH:
		// Batch completion event
		log.Printf("[DistSQLite IPC] Write batch completed")
		if r.batchCallback != nil {
			return r.batchCallback()
		}
		
	case OFFSET_TRUNCATE:
		// File truncation event
		if len(event.Data) >= 8 {
			size := *(*int64)(unsafe.Pointer(&event.Data[0]))
			log.Printf("[DistSQLite IPC] File truncated: %s to %d bytes", event.Path, size)
			if r.truncateCallback != nil {
				return r.truncateCallback(event.Path, size)
			}
		}
		
	case OFFSET_SYNC:
		// File sync event
		log.Printf("[DistSQLite IPC] File synced: %s", event.Path)
		if r.syncCallback != nil {
			return r.syncCallback(event.Path)
		}
		
	default:
		// Regular write event
		if event.Offset >= 0 {
			log.Printf("[DistSQLite IPC] Write: %s offset=%d len=%d wal=%t", 
				event.Path, event.Offset, event.Length, event.IsWAL)
			if r.writeCallback != nil {
				return r.writeCallback(event)
			}
		}
	}
	
	return nil
}

// Atomic operations for int32 (since Go's sync/atomic doesn't work with C structs directly)
func atomicLoadInt32(addr *int32) int32 {
	return *addr // On most platforms, 32-bit reads are atomic
}

func atomicStoreInt32(addr *int32, val int32) {
	*addr = val // On most platforms, 32-bit writes are atomic
}

// GetStats returns statistics about the shared memory reader
func (r *SharedMemoryReader) GetStats() map[string]interface{} {
	stats := map[string]interface{}{
		"shm_id":   r.shmID,
		"shm_addr": fmt.Sprintf("0x%x", r.shmAddr),
		"shm_size": r.shmSize,
		"active":   r.shmAddr != 0,
	}
	
	if r.shmAddr != 0 {
		shm := (*sharedMemoryC)(unsafe.Pointer(r.shmAddr))
		head := atomicLoadInt32(&shm.head)
		tail := atomicLoadInt32(&shm.tail)
		
		queueSize := tail - head
		if queueSize < 0 {
			queueSize += MAX_WRITE_EVENTS
		}
		
		stats["queue_head"] = head
		stats["queue_tail"] = tail
		stats["queue_size"] = queueSize
		stats["queue_capacity"] = MAX_WRITE_EVENTS
	}
	
	return stats
}