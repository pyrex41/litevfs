// Package distcore provides Go bindings to the high-performance OCaml core
package distcore

/*
#cgo LDFLAGS: -L${SRCDIR}/../../src -ldistcore -lasmrun_shared -lunix -lm -ldl
#cgo CFLAGS: -I${SRCDIR}/../../src
#include "distcore.h"
#include <stdlib.h>
*/
import "C"
import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// Strategy represents replication strategies
type Strategy int

const (
	StrategyTxLog Strategy = iota
	StrategyBulkSync
	StrategyHybrid
	StrategyPageDiff
)

func (s Strategy) String() string {
	switch s {
	case StrategyTxLog:
		return "TxLog"
	case StrategyBulkSync:
		return "BulkSync"
	case StrategyHybrid:
		return "Hybrid"
	case StrategyPageDiff:
		return "PageDiff"
	default:
		return "Unknown"
	}
}

// WriteEvent represents a database write event
type WriteEvent struct {
	Path       string
	Offset     int64
	Data       []byte
	IsWAL      bool
	FenceToken int64
}

// OCamlEngine provides access to the high-performance OCaml core
type OCamlEngine struct {
	queue      unsafe.Pointer
	mu         sync.RWMutex
	initialized bool
}

var (
	globalEngine *OCamlEngine
	initOnce     sync.Once
)

// GetEngine returns the global OCaml engine instance
func GetEngine() (*OCamlEngine, error) {
	var err error
	initOnce.Do(func() {
		globalEngine, err = NewOCamlEngine()
	})
	return globalEngine, err
}

// NewOCamlEngine creates a new OCaml engine instance
func NewOCamlEngine() (*OCamlEngine, error) {
	// Initialize OCaml runtime
	C.distcore_init()
	
	// Create write queue with high capacity
	queue := C.distcore_queue_create(C.int(10000))
	if queue == nil {
		return nil, fmt.Errorf("failed to create OCaml write queue")
	}
	
	engine := &OCamlEngine{
		queue:       queue,
		initialized: true,
	}
	
	// Set finalizer to ensure cleanup
	runtime.SetFinalizer(engine, (*OCamlEngine).cleanup)
	
	return engine, nil
}

// EnqueueWrite adds a write event to the high-performance queue
func (e *OCamlEngine) EnqueueWrite(event WriteEvent) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	
	if !e.initialized {
		return false
	}
	
	// Convert Go WriteEvent to C struct
	cEvent := C.write_event_t{
		path:        C.CString(event.Path),
		offset:      C.int64_t(event.Offset),
		length:      C.int32_t(len(event.Data)),
		data:        (*C.char)(unsafe.Pointer(&event.Data[0])),
		is_wal:      C.int(0),
		fence_token: C.int64_t(event.FenceToken),
	}
	
	if event.IsWAL {
		cEvent.is_wal = C.int(1)
	}
	
	// Free the path string after the call
	defer C.free(unsafe.Pointer(cEvent.path))
	
	// Call OCaml function
	result := C.distcore_queue_enqueue(e.queue, &cEvent)
	return bool(result != 0)
}

// DequeueWriteBatch retrieves a batch of write events for processing
func (e *OCamlEngine) DequeueWriteBatch(maxEvents int) ([]WriteEvent, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	
	if !e.initialized {
		return nil, fmt.Errorf("engine not initialized")
	}
	
	// Allocate C array for events
	cEvents := make([]C.write_event_t, maxEvents)
	
	// Call OCaml dequeue function
	count := C.distcore_queue_dequeue_batch(e.queue, &cEvents[0], C.int(maxEvents))
	
	if count <= 0 {
		return []WriteEvent{}, nil
	}
	
	// Convert C events back to Go events
	events := make([]WriteEvent, int(count))
	for i := 0; i < int(count); i++ {
		cEvent := &cEvents[i]
		
		// Convert C strings and data
		path := C.GoString(cEvent.path)
		data := C.GoBytes(unsafe.Pointer(cEvent.data), cEvent.length)
		
		events[i] = WriteEvent{
			Path:       path,
			Offset:     int64(cEvent.offset),
			Data:       data,
			IsWAL:      cEvent.is_wal != 0,
			FenceToken: int64(cEvent.fence_token),
		}
		
		// Free allocated memory
		C.free(unsafe.Pointer(cEvent.path))
	}
	
	return events, nil
}

// PendingCount returns the number of events waiting in the queue
func (e *OCamlEngine) PendingCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	
	if !e.initialized {
		return 0
	}
	
	count := C.distcore_queue_pending_count(e.queue)
	return int(count)
}

// ComputeReplicationStrategy determines the optimal replication strategy
func (e *OCamlEngine) ComputeReplicationStrategy(events []WriteEvent) Strategy {
	if len(events) == 0 {
		return StrategyTxLog
	}
	
	// Convert Go events to C array
	cEvents := make([]C.write_event_t, len(events))
	for i, event := range events {
		cEvents[i] = C.write_event_t{
			path:        C.CString(event.Path),
			offset:      C.int64_t(event.Offset),
			length:      C.int32_t(len(event.Data)),
			data:        (*C.char)(unsafe.Pointer(&event.Data[0])),
			is_wal:      C.int(0),
			fence_token: C.int64_t(event.FenceToken),
		}
		
		if event.IsWAL {
			cEvents[i].is_wal = C.int(1)
		}
	}
	
	// Free allocated strings
	defer func() {
		for i := range cEvents {
			C.free(unsafe.Pointer(cEvents[i].path))
		}
	}()
	
	// Call OCaml function
	strategy := C.distcore_compute_strategy(&cEvents[0], C.int(len(events)))
	
	return Strategy(strategy)
}

// EncodeWriteBatch encodes a batch of writes into efficient binary format
func (e *OCamlEngine) EncodeWriteBatch(events []WriteEvent) ([]byte, error) {
	if len(events) == 0 {
		return []byte{}, nil
	}
	
	// Convert Go events to C array
	cEvents := make([]C.write_event_t, len(events))
	for i, event := range events {
		cEvents[i] = C.write_event_t{
			path:        C.CString(event.Path),
			offset:      C.int64_t(event.Offset),
			length:      C.int32_t(len(event.Data)),
			data:        (*C.char)(unsafe.Pointer(&event.Data[0])),
			is_wal:      C.int(0),
			fence_token: C.int64_t(event.FenceToken),
		}
		
		if event.IsWAL {
			cEvents[i].is_wal = C.int(1)
		}
	}
	
	// Free allocated strings
	defer func() {
		for i := range cEvents {
			C.free(unsafe.Pointer(cEvents[i].path))
		}
	}()
	
	// Estimate buffer size (conservative estimate)
	bufferSize := len(events) * 1024 // 1KB per event estimate
	outputBuffer := make([]byte, bufferSize)
	
	// Call OCaml encoding function
	encodedSize := C.distcore_encode_write_batch(
		&cEvents[0], 
		C.int(len(events)),
		(*C.char)(unsafe.Pointer(&outputBuffer[0])),
		C.int(bufferSize),
	)
	
	if encodedSize < 0 {
		return nil, fmt.Errorf("buffer too small for encoding")
	}
	
	// Return only the used portion of the buffer
	return outputBuffer[:int(encodedSize)], nil
}

// ComputeDatabaseDiff computes efficient binary diff between two databases
func (e *OCamlEngine) ComputeDatabaseDiff(oldDB, newDB []byte, pageSize int) ([]byte, error) {
	if len(oldDB) == 0 || len(newDB) == 0 {
		return []byte{}, nil
	}
	
	// Estimate output buffer size
	outputSize := (len(oldDB) + len(newDB)) / 4 // Conservative estimate
	outputBuffer := make([]byte, outputSize)
	
	// Call OCaml diff function
	diffSize := C.distcore_compute_diff(
		(*C.char)(unsafe.Pointer(&oldDB[0])),
		C.int(len(oldDB)),
		(*C.char)(unsafe.Pointer(&newDB[0])),
		C.int(len(newDB)),
		C.int(pageSize),
		(*C.char)(unsafe.Pointer(&outputBuffer[0])),
		C.int(outputSize),
	)
	
	if diffSize < 0 {
		return nil, fmt.Errorf("diff computation failed")
	}
	
	return outputBuffer[:int(diffSize)], nil
}

// Stats represents performance statistics
type Stats struct {
	QueueEnqueues         int64
	QueueDequeues         int64
	BytesProcessed        int64
	DiffOperations        int64
	ReplicationOperations int64
	StateTransitions      int64
}

// GetStats returns performance statistics from the OCaml engine
func (e *OCamlEngine) GetStats() (*Stats, error) {
	var cStats C.distcore_stats_t
	
	result := C.distcore_get_stats(&cStats)
	if result != 0 {
		return nil, fmt.Errorf("failed to get stats")
	}
	
	return &Stats{
		QueueEnqueues:         int64(cStats.queue_enqueues),
		QueueDequeues:         int64(cStats.queue_dequeues),
		BytesProcessed:        int64(cStats.bytes_processed),
		DiffOperations:        int64(cStats.diff_operations),
		ReplicationOperations: int64(cStats.replication_operations),
		StateTransitions:      int64(cStats.state_transitions),
	}, nil
}

// ResetStats resets performance counters
func (e *OCamlEngine) ResetStats() {
	C.distcore_reset_stats()
}

// IsInitialized returns true if the engine is properly initialized
func (e *OCamlEngine) IsInitialized() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.initialized
}

// cleanup is called by the finalizer to clean up resources
func (e *OCamlEngine) cleanup() {
	e.mu.Lock()
	defer e.mu.Unlock()
	
	if e.initialized {
		C.distcore_cleanup()
		e.initialized = false
	}
}

// ProcessWriteBatch is a high-level function that processes a batch of writes
// using the OCaml engine for optimal performance
func ProcessWriteBatch(events []WriteEvent) (Strategy, []byte, error) {
	engine, err := GetEngine()
	if err != nil {
		return StrategyTxLog, nil, fmt.Errorf("failed to get OCaml engine: %w", err)
	}
	
	// Compute optimal strategy
	strategy := engine.ComputeReplicationStrategy(events)
	
	// Encode events for efficient transmission
	encoded, err := engine.EncodeWriteBatch(events)
	if err != nil {
		return strategy, nil, fmt.Errorf("failed to encode write batch: %w", err)
	}
	
	return strategy, encoded, nil
}

// HighPerformanceQueue provides a queue interface optimized for performance
type HighPerformanceQueue struct {
	engine *OCamlEngine
}

// NewHighPerformanceQueue creates a new high-performance queue
func NewHighPerformanceQueue() (*HighPerformanceQueue, error) {
	engine, err := GetEngine()
	if err != nil {
		return nil, err
	}
	
	return &HighPerformanceQueue{
		engine: engine,
	}, nil
}

// Enqueue adds an event to the queue
func (q *HighPerformanceQueue) Enqueue(event WriteEvent) bool {
	return q.engine.EnqueueWrite(event)
}

// DequeueBatch retrieves multiple events efficiently
func (q *HighPerformanceQueue) DequeueBatch(maxEvents int) ([]WriteEvent, error) {
	return q.engine.DequeueWriteBatch(maxEvents)
}

// Pending returns the number of pending events
func (q *HighPerformanceQueue) Pending() int {
	return q.engine.PendingCount()
}

// BinaryDiffer provides high-performance binary diff operations
type BinaryDiffer struct {
	engine *OCamlEngine
}

// NewBinaryDiffer creates a new binary differ
func NewBinaryDiffer() (*BinaryDiffer, error) {
	engine, err := GetEngine()
	if err != nil {
		return nil, err
	}
	
	return &BinaryDiffer{
		engine: engine,
	}, nil
}

// ComputeDiff computes the binary diff between two databases
func (d *BinaryDiffer) ComputeDiff(oldDB, newDB []byte, pageSize int) ([]byte, error) {
	return d.engine.ComputeDatabaseDiff(oldDB, newDB, pageSize)
}