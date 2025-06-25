package replication

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"distsqlite/pkg/rsync"
)

const (
	// Threshold for switching to bulk replication (1MB)
	BulkThreshold = 1 * 1024 * 1024
	
	// Maximum time to wait before forcing replication
	MaxReplicationDelay = 5 * time.Second
	
	// Size of the transaction log buffer
	TxLogBufferSize = 100
	
	// Maximum retries for failed operations
	MaxRetries = 3
	
	// Fence token increment
	FenceTokenIncrement = 1
)

// Change represents a database change (kept for compatibility)
type Change struct {
	Database  string    `json:"database"`
	Timestamp time.Time `json:"timestamp"`
	Size      int64     `json:"size"`
	Type      string    `json:"type"` // "write", "truncate", "delete"
	Data      []byte    `json:"data,omitempty"`
	Offset    int64     `json:"offset,omitempty"`
	Flags     int       `json:"flags,omitempty"`
}

// WriteTracker tracks write patterns to inform replication strategy
type WriteTracker struct {
	mu                sync.RWMutex
	databases         map[string]*DatabaseStats
	replicationWindow time.Duration
}

// DatabaseStats tracks statistics for a specific database
type DatabaseStats struct {
	TotalWrites    int64
	BytesWritten   int64
	LastWrite      time.Time
	PendingChanges []Change
	LastSync       time.Time
	VectorClock    VectorClock
	FenceToken     uint64
	ConflictCount  int64
	LastConflict   time.Time
}

// ReplicationMode represents the replication strategy
type ReplicationMode int

const (
	ReplicationModeAuto ReplicationMode = iota
	ReplicationModeTxLog
	ReplicationModeBulk
	ReplicationModeDisabled
)

// SafetyConfig contains distributed safety configuration
type SafetyConfig struct {
	EnableFenceTokens      bool
	EnableVectorClocks     bool
	EnableConflictDetection bool
	ConflictResolutionStrategy string // "last-write-wins", "vector-clock", "manual"
	ReadYourWritesTimeout  time.Duration
	MaxClockSkew          time.Duration
}

// HybridReplicator manages the hybrid replication strategy
type HybridReplicator struct {
	mu           sync.RWMutex
	nodeID       string
	writeTracker *WriteTracker
	rsync        *rsync.Wrapper
	txLogShipper *TransactionLogShipper
	
	// Configuration
	bulkThreshold int64
	maxDelay      time.Duration
	mode          ReplicationMode
	safetyConfig  SafetyConfig
	
	// Replication targets (other nodes)
	targets []string
	
	// Distributed safety
	globalFenceToken  uint64 // Atomic counter
	nodeVectorClock   VectorClock
	clockMutex        sync.RWMutex
	
	// Circuit breaker state
	circuitBreakerOpen   bool
	circuitBreakerUntil  time.Time
	consecutiveFailures  int64
	circuitBreakerMutex  sync.RWMutex
	
	// Background workers
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	
	// Metrics
	operationsProcessed uint64
	replicationErrors   uint64
	conflictsDetected   uint64
	avgReplicationLatency time.Duration
}

// NewHybridReplicator creates a new hybrid replicator
func NewHybridReplicator(nodeID string, targets []string) *HybridReplicator {
	ctx, cancel := context.WithCancel(context.Background())
	
	replicator := &HybridReplicator{
		nodeID: nodeID,
		writeTracker: &WriteTracker{
			databases:         make(map[string]*DatabaseStats),
			replicationWindow: MaxReplicationDelay,
		},
		rsync:         rsync.NewWrapper(),
		bulkThreshold: BulkThreshold,
		maxDelay:      MaxReplicationDelay,
		mode:          ReplicationModeAuto,
		targets:       targets,
		nodeVectorClock: make(VectorClock),
		safetyConfig: SafetyConfig{
			EnableFenceTokens:             true,
			EnableVectorClocks:            true,
			EnableConflictDetection:       true,
			ConflictResolutionStrategy:    "vector-clock",
			ReadYourWritesTimeout:         5 * time.Second,
			MaxClockSkew:                 10 * time.Second,
		},
		ctx:           ctx,
		cancel:        cancel,
	}
	
	// Initialize transaction log shipper
	replicator.txLogShipper = NewTransactionLogShipper(nodeID, targets)
	
	// Initialize node vector clock
	replicator.nodeVectorClock[nodeID] = 0
	
	return replicator
}

// Start begins the background replication workers
func (h *HybridReplicator) Start() error {
	log.Printf("[DistSQLite] Starting hybrid replicator with %d targets (node: %s)", len(h.targets), h.nodeID)
	
	// Start transaction log shipper
	if err := h.txLogShipper.Start(); err != nil {
		return fmt.Errorf("failed to start transaction log shipper: %w", err)
	}
	
	// Start background workers
	h.wg.Add(3)
	go h.replicationWorker()
	go h.healthMonitorWorker()
	go h.conflictDetectionWorker()
	
	return nil
}

// Stop shuts down the replicator
func (h *HybridReplicator) Stop() error {
	log.Printf("[DistSQLite] Stopping hybrid replicator")
	
	h.cancel()
	h.wg.Wait()
	
	// Stop transaction log shipper
	if err := h.txLogShipper.Stop(); err != nil {
		log.Printf("[DistSQLite] Error stopping transaction log shipper: %v", err)
	}
	
	return nil
}

// RecordChange records a database change for potential replication
func (h *HybridReplicator) RecordChange(dbPath string, changeType string, size int64, offset int64, flags int, data []byte) error {
	start := time.Now()
	defer func() {
		atomic.AddUint64(&h.operationsProcessed, 1)
		// Update average latency (simplified)
		latency := time.Since(start)
		h.mu.Lock()
		h.avgReplicationLatency = (h.avgReplicationLatency + latency) / 2
		h.mu.Unlock()
	}()
	
	// Check circuit breaker
	if h.isCircuitBreakerOpen() {
		return fmt.Errorf("circuit breaker is open, rejecting operation")
	}
	
	h.writeTracker.mu.Lock()
	
	stats, exists := h.writeTracker.databases[dbPath]
	if !exists {
		stats = &DatabaseStats{
			PendingChanges: make([]Change, 0, TxLogBufferSize),
			VectorClock:    make(VectorClock),
			FenceToken:     h.getNextFenceToken(),
		}
		// Initialize vector clock for this database
		stats.VectorClock[h.nodeID] = 0
		h.writeTracker.databases[dbPath] = stats
	}
	
	// Update vector clock
	h.clockMutex.Lock()
	h.nodeVectorClock.Update(h.nodeID)
	stats.VectorClock.Update(h.nodeID)
	vectorClockCopy := stats.VectorClock.Clone()
	h.clockMutex.Unlock()
	
	change := Change{
		Database:  dbPath,
		Timestamp: time.Now(),
		Size:      size,
		Type:      changeType,
		Data:      data,
		Offset:    offset,
		Flags:     flags,
	}
	
	stats.TotalWrites++
	stats.BytesWritten += size
	stats.LastWrite = time.Now()
	stats.PendingChanges = append(stats.PendingChanges, change)
	
	// Determine replication strategy
	shouldReplicateImmediately := h.shouldReplicateImmediately(stats)
	h.writeTracker.mu.Unlock()
	
	// Add to transaction log for small changes
	if !shouldReplicateImmediately || size < h.bulkThreshold {
		var logEntryType LogEntryType
		switch changeType {
		case "write":
			logEntryType = LogEntryTypeWrite
		case "sync":
			logEntryType = LogEntryTypeSync
		case "truncate":
			logEntryType = LogEntryTypeTruncate
		default:
			logEntryType = LogEntryTypeWrite
		}
		
		// Add entry to transaction log shipper
		if err := h.txLogShipper.AddEntry(logEntryType, dbPath, offset, int(size), flags, data); err != nil {
			log.Printf("[DistSQLite] Failed to add entry to transaction log: %v", err)
			atomic.AddUint64(&h.replicationErrors, 1)
		}
	}
	
	// Trigger immediate replication if threshold exceeded
	if shouldReplicateImmediately {
		go h.replicateDatabase(dbPath, stats)
	}
	
	return nil
}

// getNextFenceToken atomically increments and returns the next fence token
func (h *HybridReplicator) getNextFenceToken() uint64 {
	return atomic.AddUint64(&h.globalFenceToken, FenceTokenIncrement)
}

// shouldReplicateImmediately determines if we should replicate immediately
func (h *HybridReplicator) shouldReplicateImmediately(stats *DatabaseStats) bool {
	// Check if we've accumulated enough changes for bulk replication
	totalPendingSize := int64(0)
	for _, change := range stats.PendingChanges {
		totalPendingSize += change.Size
	}
	
	if totalPendingSize >= h.bulkThreshold {
		return true
	}
	
	// Check if it's been too long since last sync
	if time.Since(stats.LastSync) >= h.maxDelay {
		return true
	}
	
	// Check if we have too many pending changes
	if len(stats.PendingChanges) >= TxLogBufferSize {
		return true
	}
	
	return false
}

// replicationWorker runs in the background to handle periodic replication
func (h *HybridReplicator) replicationWorker() {
	defer h.wg.Done()
	
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			h.checkPendingReplications()
		}
	}
}

// healthMonitorWorker monitors replication health and manages circuit breaker
func (h *HybridReplicator) healthMonitorWorker() {
	defer h.wg.Done()
	
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			h.monitorReplicationHealth()
		}
	}
}

// conflictDetectionWorker detects and resolves conflicts
func (h *HybridReplicator) conflictDetectionWorker() {
	defer h.wg.Done()
	
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			if h.safetyConfig.EnableConflictDetection {
				h.detectAndResolveConflicts()
			}
		}
	}
}

// monitorReplicationHealth monitors the health of replication and manages circuit breaker
func (h *HybridReplicator) monitorReplicationHealth() {
	h.circuitBreakerMutex.Lock()
	defer h.circuitBreakerMutex.Unlock()
	
	// Check if circuit breaker should be closed
	if h.circuitBreakerOpen && time.Now().After(h.circuitBreakerUntil) {
		h.circuitBreakerOpen = false
		h.consecutiveFailures = 0
		log.Printf("[DistSQLite] Circuit breaker closed")
	}
	
	// Check error rate
	totalOps := atomic.LoadUint64(&h.operationsProcessed)
	totalErrors := atomic.LoadUint64(&h.replicationErrors)
	
	if totalOps > 100 { // Only check after some operations
		errorRate := float64(totalErrors) / float64(totalOps)
		if errorRate > 0.5 && !h.circuitBreakerOpen { // 50% error rate threshold
			h.openCircuitBreaker()
		}
	}
}

// openCircuitBreaker opens the circuit breaker
func (h *HybridReplicator) openCircuitBreaker() {
	h.circuitBreakerOpen = true
	h.circuitBreakerUntil = time.Now().Add(60 * time.Second) // Open for 1 minute
	log.Printf("[DistSQLite] Circuit breaker opened due to high error rate")
}

// isCircuitBreakerOpen checks if the circuit breaker is open
func (h *HybridReplicator) isCircuitBreakerOpen() bool {
	h.circuitBreakerMutex.RLock()
	defer h.circuitBreakerMutex.RUnlock()
	return h.circuitBreakerOpen && time.Now().Before(h.circuitBreakerUntil)
}

// detectAndResolveConflicts detects and resolves replication conflicts
func (h *HybridReplicator) detectAndResolveConflicts() {
	h.writeTracker.mu.RLock()
	defer h.writeTracker.mu.RUnlock()
	
	for dbPath, stats := range h.writeTracker.databases {
		// Check for clock skew conflicts
		h.clockMutex.RLock()
		for nodeID, timestamp := range stats.VectorClock {
			if nodeID != h.nodeID {
				if myTimestamp, exists := h.nodeVectorClock[nodeID]; exists {
					skew := int64(timestamp) - int64(myTimestamp)
					if time.Duration(skew)*time.Nanosecond > h.safetyConfig.MaxClockSkew {
						log.Printf("[DistSQLite] Clock skew detected for %s: %v", dbPath, time.Duration(skew)*time.Nanosecond)
						atomic.AddUint64(&h.conflictsDetected, 1)
						stats.ConflictCount++
						stats.LastConflict = time.Now()
					}
				}
			}
		}
		h.clockMutex.RUnlock()
	}
}

// checkPendingReplications checks for databases that need replication
func (h *HybridReplicator) checkPendingReplications() {
	h.writeTracker.mu.Lock()
	defer h.writeTracker.mu.Unlock()
	
	for dbPath, stats := range h.writeTracker.databases {
		if len(stats.PendingChanges) > 0 && h.shouldReplicateImmediately(stats) {
			go h.replicateDatabase(dbPath, stats)
		}
	}
}

// replicateDatabase replicates a specific database using the optimal strategy
func (h *HybridReplicator) replicateDatabase(dbPath string, stats *DatabaseStats) {
	h.mu.Lock()
	defer h.mu.Unlock()
	
	if len(stats.PendingChanges) == 0 {
		return
	}
	
	totalSize := int64(0)
	for _, change := range stats.PendingChanges {
		totalSize += change.Size
	}
	
	log.Printf("[DistSQLite] Replicating %s (%d changes, %d bytes)", 
		dbPath, len(stats.PendingChanges), totalSize)
	
	var err error
	if totalSize >= h.bulkThreshold || h.mode == ReplicationModeBulk {
		// Large changes - use sqlite3_rsync
		err = h.replicateViaBulkSync(dbPath, stats.PendingChanges)
		log.Printf("[DistSQLite] Used bulk sync (rsync) for %s", dbPath)
	} else if h.mode != ReplicationModeDisabled {
		// Small changes - transaction log is handled by txLogShipper
		log.Printf("[DistSQLite] Changes handled by transaction log for %s", dbPath)
		err = nil // Transaction log shipping is handled separately
	}
	
	if err != nil {
		atomic.AddUint64(&h.replicationErrors, 1)
		h.circuitBreakerMutex.Lock()
		h.consecutiveFailures++
		if h.consecutiveFailures >= 5 {
			h.openCircuitBreaker()
		}
		h.circuitBreakerMutex.Unlock()
		
		log.Printf("[DistSQLite] Replication failed for %s: %v", dbPath, err)
		return
	}
	
	// Reset consecutive failures on success
	h.circuitBreakerMutex.Lock()
	h.consecutiveFailures = 0
	h.circuitBreakerMutex.Unlock()
	
	// Clear pending changes on successful replication
	stats.PendingChanges = stats.PendingChanges[:0]
	stats.LastSync = time.Now()
}

// replicateViaBulkSync uses sqlite3_rsync for large changes
func (h *HybridReplicator) replicateViaBulkSync(dbPath string, changes []Change) error {
	if len(h.targets) == 0 {
		return fmt.Errorf("no replication targets configured")
	}
	
	// Ensure the database file exists
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return fmt.Errorf("database file does not exist: %s", dbPath)
	}
	
	// Replicate to all targets with retry logic
	var lastError error
	for _, target := range h.targets {
		targetPath := h.getTargetPath(target, dbPath)
		
		// Retry logic for bulk sync
		for retry := 0; retry < MaxRetries; retry++ {
			log.Printf("[DistSQLite] Bulk syncing %s to %s (attempt %d/%d)", dbPath, targetPath, retry+1, MaxRetries)
			
			if err := h.rsync.Sync(dbPath, targetPath); err != nil {
				lastError = err
				log.Printf("[DistSQLite] Bulk sync attempt %d failed to %s: %v", retry+1, target, err)
				
				if retry < MaxRetries-1 {
					time.Sleep(time.Duration(retry+1) * time.Second) // Exponential backoff
				}
				continue
			}
			
			log.Printf("[DistSQLite] Bulk sync successful to %s", target)
			lastError = nil
			break
		}
		
		if lastError != nil {
			return fmt.Errorf("bulk sync failed to %s after %d retries: %w", target, MaxRetries, lastError)
		}
	}
	
	return nil
}

// getTargetPath constructs the target path for a database on a remote node
func (h *HybridReplicator) getTargetPath(target, dbPath string) string {
	// For now, assume rsync-style target format
	// In production, this would be more sophisticated
	filename := filepath.Base(dbPath)
	return fmt.Sprintf("%s:/data/%s", target, filename)
}

// GetStats returns replication statistics
func (h *HybridReplicator) GetStats() map[string]*DatabaseStats {
	h.writeTracker.mu.RLock()
	defer h.writeTracker.mu.RUnlock()
	
	stats := make(map[string]*DatabaseStats)
	for dbPath, dbStats := range h.writeTracker.databases {
		// Create a copy to avoid race conditions
		statsCopy := &DatabaseStats{
			TotalWrites:    dbStats.TotalWrites,
			BytesWritten:   dbStats.BytesWritten,
			LastWrite:      dbStats.LastWrite,
			LastSync:       dbStats.LastSync,
			VectorClock:    dbStats.VectorClock.Clone(),
			FenceToken:     dbStats.FenceToken,
			ConflictCount:  dbStats.ConflictCount,
			LastConflict:   dbStats.LastConflict,
			PendingChanges: make([]Change, len(dbStats.PendingChanges)),
		}
		copy(statsCopy.PendingChanges, dbStats.PendingChanges)
		stats[dbPath] = statsCopy
	}
	
	return stats
}

// GetDetailedStats returns detailed statistics including transaction log metrics
func (h *HybridReplicator) GetDetailedStats() map[string]interface{} {
	dbStats := h.GetStats()
	txLogStats := h.txLogShipper.GetStats()
	
	return map[string]interface{}{
		"node_id":                    h.nodeID,
		"operations_processed":       atomic.LoadUint64(&h.operationsProcessed),
		"replication_errors":         atomic.LoadUint64(&h.replicationErrors),
		"conflicts_detected":         atomic.LoadUint64(&h.conflictsDetected),
		"avg_replication_latency_ms": h.avgReplicationLatency.Milliseconds(),
		"circuit_breaker_open":       h.isCircuitBreakerOpen(),
		"consecutive_failures":       atomic.LoadInt64(&h.consecutiveFailures),
		"global_fence_token":         atomic.LoadUint64(&h.globalFenceToken),
		"vector_clock":               h.nodeVectorClock.Clone(),
		"database_stats":             dbStats,
		"transaction_log_stats":      txLogStats,
		"targets":                    h.targets,
		"replication_mode":           h.mode,
		"safety_config":              h.safetyConfig,
	}
}

// IsHealthy returns true if the replicator is functioning properly
func (h *HybridReplicator) IsHealthy() bool {
	// Check circuit breaker
	if h.isCircuitBreakerOpen() {
		return false
	}
	
	// Check if any databases have been failing to replicate for too long
	h.writeTracker.mu.RLock()
	defer h.writeTracker.mu.RUnlock()
	
	for _, stats := range h.writeTracker.databases {
		if len(stats.PendingChanges) > 0 {
			oldestChange := stats.PendingChanges[0]
			if time.Since(oldestChange.Timestamp) > 5*h.maxDelay {
				return false // Replication is falling behind
			}
		}
		
		// Check conflict rate
		if stats.ConflictCount > 0 && time.Since(stats.LastConflict) < time.Minute {
			return false // Recent conflicts indicate issues
		}
	}
	
	// Check transaction log shipper health
	txLogStats := h.txLogShipper.GetStats()
	if pendingEntries, ok := txLogStats["pending_entries"].(int); ok && pendingEntries > TxLogBufferSize/2 {
		return false // Transaction log is backing up
	}
	
	return true
}

// SetReplicationMode sets the replication mode
func (h *HybridReplicator) SetReplicationMode(mode ReplicationMode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mode = mode
}

// UpdateSafetyConfig updates the safety configuration
func (h *HybridReplicator) UpdateSafetyConfig(config SafetyConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.safetyConfig = config
}