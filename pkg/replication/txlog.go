package replication

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// LogEntryType represents the type of transaction log entry
type LogEntryType uint8

const (
	LogEntryTypeWrite LogEntryType = iota + 1
	LogEntryTypeSync
	LogEntryTypeTruncate
	LogEntryTypeCheckpoint
	LogEntryTypeBeginTx
	LogEntryTypeCommitTx
	LogEntryTypeRollbackTx
)

// VectorClock represents a vector clock for causality tracking
type VectorClock map[string]uint64

// Clone creates a copy of the vector clock
func (vc VectorClock) Clone() VectorClock {
	clone := make(VectorClock)
	for k, v := range vc {
		clone[k] = v
	}
	return clone
}

// Update increments the clock for the given node
func (vc VectorClock) Update(nodeID string) {
	vc[nodeID]++
}

// Merge merges another vector clock into this one
func (vc VectorClock) Merge(other VectorClock) {
	for nodeID, timestamp := range other {
		if vc[nodeID] < timestamp {
			vc[nodeID] = timestamp
		}
	}
}

// HappensBefore returns true if this clock happens before the other
func (vc VectorClock) HappensBefore(other VectorClock) bool {
	for nodeID, timestamp := range vc {
		if otherTimestamp, exists := other[nodeID]; !exists || timestamp > otherTimestamp {
			return false
		}
	}
	
	// Check if they are equal (not happens-before)
	equal := true
	for nodeID, timestamp := range other {
		if vcTimestamp, exists := vc[nodeID]; !exists || vcTimestamp != timestamp {
			equal = false
			break
		}
	}
	
	return !equal
}

// LogEntry represents a single entry in the transaction log
type LogEntry struct {
	// Header
	Version     uint8         `json:"version"`
	Type        LogEntryType  `json:"type"`
	NodeID      string        `json:"node_id"`
	Database    string        `json:"database"`
	Timestamp   time.Time     `json:"timestamp"`
	VectorClock VectorClock   `json:"vector_clock"`
	Checksum    [32]byte      `json:"checksum"`
	
	// Payload
	Offset      int64         `json:"offset,omitempty"`
	Size        int           `json:"size,omitempty"`
	Flags       int           `json:"flags,omitempty"`
	Data        []byte        `json:"data,omitempty"`
	
	// Transaction context
	TransactionID string      `json:"transaction_id,omitempty"`
	
	// Internal
	compressed  bool
	rawSize     int
}

// TransactionLogShipper manages transaction log shipping
type TransactionLogShipper struct {
	mu          sync.RWMutex
	nodeID      string
	vectorClock VectorClock
	
	// Configuration
	compression       CompressionType
	maxBatchSize      int
	maxBatchDelay     time.Duration
	retryDelay        time.Duration
	maxRetries        int
	
	// Network connections
	connections map[string]*ConnectionPool
	
	// Batching
	pendingEntries []LogEntry
	lastFlush      time.Time
	batchMutex     sync.Mutex
	
	// Background workers
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	
	// Metrics
	entriesSent     uint64
	bytesCompressed uint64
	compressionRatio float64
}

// CompressionType represents the compression algorithm
type CompressionType int

const (
	CompressionNone CompressionType = iota
	CompressionGzip
	CompressionZstd
)

// ConnectionPool manages connections to a target node
type ConnectionPool struct {
	target     string
	maxConns   int
	connChan   chan net.Conn
	connCount  int
	mu         sync.Mutex
}

// NewTransactionLogShipper creates a new transaction log shipper
func NewTransactionLogShipper(nodeID string, targets []string) *TransactionLogShipper {
	ctx, cancel := context.WithCancel(context.Background())
	
	shipper := &TransactionLogShipper{
		nodeID:      nodeID,
		vectorClock: make(VectorClock),
		compression: CompressionZstd,
		maxBatchSize: 100,
		maxBatchDelay: 50 * time.Millisecond,
		retryDelay:   time.Second,
		maxRetries:   3,
		connections:  make(map[string]*ConnectionPool),
		ctx:          ctx,
		cancel:       cancel,
	}
	
	// Initialize vector clock
	shipper.vectorClock[nodeID] = 0
	
	// Create connection pools for targets
	for _, target := range targets {
		shipper.connections[target] = &ConnectionPool{
			target:   target,
			maxConns: 5,
			connChan: make(chan net.Conn, 5),
		}
	}
	
	return shipper
}

// Start begins the transaction log shipping
func (s *TransactionLogShipper) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	// Start batch flush worker
	s.wg.Add(1)
	go s.batchFlushWorker()
	
	log.Printf("[DistSQLite TxLog] Transaction log shipper started for node %s", s.nodeID)
	return nil
}

// Stop shuts down the transaction log shipper
func (s *TransactionLogShipper) Stop() error {
	s.cancel()
	s.wg.Wait()
	
	// Close all connections
	for _, pool := range s.connections {
		s.closeConnectionPool(pool)
	}
	
	log.Printf("[DistSQLite TxLog] Transaction log shipper stopped")
	return nil
}

// AddEntry adds a new log entry for shipping
func (s *TransactionLogShipper) AddEntry(entryType LogEntryType, database string, offset int64, size int, flags int, data []byte) error {
	// Update vector clock
	s.mu.Lock()
	s.vectorClock.Update(s.nodeID)
	vectorClockCopy := s.vectorClock.Clone()
	s.mu.Unlock()
	
	// Create log entry
	entry := LogEntry{
		Version:     1,
		Type:        entryType,
		NodeID:      s.nodeID,
		Database:    database,
		Timestamp:   time.Now(),
		VectorClock: vectorClockCopy,
		Offset:      offset,
		Size:        size,
		Flags:       flags,
		Data:        data,
	}
	
	// Calculate checksum
	entry.Checksum = s.calculateChecksum(entry)
	
	// Add to batch
	s.batchMutex.Lock()
	s.pendingEntries = append(s.pendingEntries, entry)
	shouldFlush := len(s.pendingEntries) >= s.maxBatchSize
	s.batchMutex.Unlock()
	
	// Flush immediately if batch is full
	if shouldFlush {
		return s.flushBatch()
	}
	
	return nil
}

// calculateChecksum calculates the SHA256 checksum of the entry
func (s *TransactionLogShipper) calculateChecksum(entry LogEntry) [32]byte {
	hasher := sha256.New()
	
	// Include key fields in checksum
	binary.Write(hasher, binary.LittleEndian, entry.Version)
	binary.Write(hasher, binary.LittleEndian, entry.Type)
	hasher.Write([]byte(entry.NodeID))
	hasher.Write([]byte(entry.Database))
	binary.Write(hasher, binary.LittleEndian, entry.Timestamp.UnixNano())
	binary.Write(hasher, binary.LittleEndian, entry.Offset)
	binary.Write(hasher, binary.LittleEndian, int64(entry.Size))
	binary.Write(hasher, binary.LittleEndian, int64(entry.Flags))
	if entry.Data != nil {
		hasher.Write(entry.Data)
	}
	
	var result [32]byte
	copy(result[:], hasher.Sum(nil))
	return result
}

// batchFlushWorker flushes batches periodically
func (s *TransactionLogShipper) batchFlushWorker() {
	defer s.wg.Done()
	
	ticker := time.NewTicker(s.maxBatchDelay / 2) // Check more frequently than the delay
	defer ticker.Stop()
	
	for {
		select {
		case <-s.ctx.Done():
			// Final flush before shutdown
			s.flushBatch()
			return
		case <-ticker.C:
			s.batchMutex.Lock()
			shouldFlush := len(s.pendingEntries) > 0 && time.Since(s.lastFlush) >= s.maxBatchDelay
			s.batchMutex.Unlock()
			
			if shouldFlush {
				s.flushBatch()
			}
		}
	}
}

// flushBatch sends the current batch to all targets
func (s *TransactionLogShipper) flushBatch() error {
	s.batchMutex.Lock()
	if len(s.pendingEntries) == 0 {
		s.batchMutex.Unlock()
		return nil
	}
	
	// Take snapshot of pending entries
	entries := make([]LogEntry, len(s.pendingEntries))
	copy(entries, s.pendingEntries)
	s.pendingEntries = s.pendingEntries[:0] // Clear slice
	s.lastFlush = time.Now()
	s.batchMutex.Unlock()
	
	// Serialize and compress the batch
	batch, err := s.serializeBatch(entries)
	if err != nil {
		log.Printf("[DistSQLite TxLog] Failed to serialize batch: %v", err)
		return err
	}
	
	// Send to all targets
	var sendErrors []error
	for target, pool := range s.connections {
		if err := s.sendBatchToTarget(target, pool, batch); err != nil {
			log.Printf("[DistSQLite TxLog] Failed to send batch to %s: %v", target, err)
			sendErrors = append(sendErrors, err)
		}
	}
	
	// Update metrics
	s.mu.Lock()
	s.entriesSent += uint64(len(entries))
	s.mu.Unlock()
	
	if len(sendErrors) > 0 {
		return fmt.Errorf("failed to send to %d targets", len(sendErrors))
	}
	
	return nil
}

// serializeBatch serializes and compresses a batch of log entries
func (s *TransactionLogShipper) serializeBatch(entries []LogEntry) ([]byte, error) {
	// Serialize to JSON
	jsonData, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal entries: %w", err)
	}
	
	// Compress based on configuration
	var compressed []byte
	switch s.compression {
	case CompressionNone:
		compressed = jsonData
	case CompressionGzip:
		compressed, err = s.compressGzip(jsonData)
	case CompressionZstd:
		compressed, err = s.compressZstd(jsonData)
	default:
		compressed = jsonData
	}
	
	if err != nil {
		return nil, fmt.Errorf("compression failed: %w", err)
	}
	
	// Update compression metrics
	s.mu.Lock()
	s.bytesCompressed += uint64(len(jsonData))
	if len(jsonData) > 0 {
		s.compressionRatio = float64(len(compressed)) / float64(len(jsonData))
	}
	s.mu.Unlock()
	
	// Create batch header
	header := struct {
		Version     uint8           `json:"version"`
		Compression CompressionType `json:"compression"`
		EntryCount  int             `json:"entry_count"`
		RawSize     int             `json:"raw_size"`
		Timestamp   time.Time       `json:"timestamp"`
	}{
		Version:     1,
		Compression: s.compression,
		EntryCount:  len(entries),
		RawSize:     len(jsonData),
		Timestamp:   time.Now(),
	}
	
	headerData, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal header: %w", err)
	}
	
	// Combine header and payload
	var result bytes.Buffer
	binary.Write(&result, binary.LittleEndian, uint32(len(headerData)))
	result.Write(headerData)
	binary.Write(&result, binary.LittleEndian, uint32(len(compressed)))
	result.Write(compressed)
	
	return result.Bytes(), nil
}

// compressGzip compresses data using gzip
func (s *TransactionLogShipper) compressGzip(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	
	if _, err := writer.Write(data); err != nil {
		writer.Close()
		return nil, err
	}
	
	if err := writer.Close(); err != nil {
		return nil, err
	}
	
	return buf.Bytes(), nil
}

// compressZstd compresses data using zstd
func (s *TransactionLogShipper) compressZstd(data []byte) ([]byte, error) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer encoder.Close()
	
	return encoder.EncodeAll(data, nil), nil
}

// sendBatchToTarget sends a batch to a specific target
func (s *TransactionLogShipper) sendBatchToTarget(target string, pool *ConnectionPool, batch []byte) error {
	conn, err := s.getConnection(pool)
	if err != nil {
		return fmt.Errorf("failed to get connection: %w", err)
	}
	defer s.releaseConnection(pool, conn)
	
	// Set write timeout
	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	
	// Send batch
	if _, err := conn.Write(batch); err != nil {
		return fmt.Errorf("failed to write batch: %w", err)
	}
	
	// Read acknowledgment
	ackBuf := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(ackBuf); err != nil {
		return fmt.Errorf("failed to read acknowledgment: %w", err)
	}
	
	if ackBuf[0] != 0x01 { // 0x01 = ACK
		return fmt.Errorf("received negative acknowledgment")
	}
	
	return nil
}

// getConnection gets a connection from the pool
func (s *TransactionLogShipper) getConnection(pool *ConnectionPool) (net.Conn, error) {
	select {
	case conn := <-pool.connChan:
		return conn, nil
	default:
		// No available connection, create new one
		pool.mu.Lock()
		if pool.connCount >= pool.maxConns {
			pool.mu.Unlock()
			// Wait for available connection
			return <-pool.connChan, nil
		}
		pool.connCount++
		pool.mu.Unlock()
		
		// Create new connection
		conn, err := net.DialTimeout("tcp", pool.target, 10*time.Second)
		if err != nil {
			pool.mu.Lock()
			pool.connCount--
			pool.mu.Unlock()
			return nil, err
		}
		
		return conn, nil
	}
}

// releaseConnection returns a connection to the pool
func (s *TransactionLogShipper) releaseConnection(pool *ConnectionPool, conn net.Conn) {
	select {
	case pool.connChan <- conn:
		// Connection returned to pool
	default:
		// Pool is full, close connection
		conn.Close()
		pool.mu.Lock()
		pool.connCount--
		pool.mu.Unlock()
	}
}

// closeConnectionPool closes all connections in a pool
func (s *TransactionLogShipper) closeConnectionPool(pool *ConnectionPool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	
	close(pool.connChan)
	for conn := range pool.connChan {
		conn.Close()
	}
	pool.connCount = 0
}

// GetStats returns statistics about the transaction log shipper
func (s *TransactionLogShipper) GetStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	s.batchMutex.Lock()
	pendingCount := len(s.pendingEntries)
	s.batchMutex.Unlock()
	
	return map[string]interface{}{
		"node_id":           s.nodeID,
		"entries_sent":      s.entriesSent,
		"bytes_compressed":  s.bytesCompressed,
		"compression_ratio": s.compressionRatio,
		"pending_entries":   pendingCount,
		"vector_clock":      s.vectorClock,
		"targets":           len(s.connections),
	}
}