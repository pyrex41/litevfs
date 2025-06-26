package replication

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
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
)

// Change represents a database change
type Change struct {
	Database  string    `json:"database"`
	Timestamp time.Time `json:"timestamp"`
	Size      int64     `json:"size"`
	Type      string    `json:"type"` // "write", "truncate", "delete"
	Data      []byte    `json:"data,omitempty"`
}

// ReplicationStrategy represents different replication methods
type ReplicationStrategy string

const (
	StrategyTxLog    ReplicationStrategy = "txlog"
	StrategyHTTPRsync ReplicationStrategy = "http-rsync"
	StrategySSHRsync  ReplicationStrategy = "ssh-rsync"
)

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
}

// HybridReplicator manages the hybrid replication strategy
type HybridReplicator struct {
	mu           sync.RWMutex
	writeTracker *WriteTracker
	rsync        *rsync.Wrapper
	httpRsync    *rsync.HTTPRsyncClient
	
	// Configuration
	bulkThreshold     int64
	maxDelay          time.Duration
	preferHTTPRsync   bool
	
	// Replication targets (other nodes)
	targets []string
	
	// Background workers
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewHybridReplicator creates a new hybrid replicator
func NewHybridReplicator(targets []string) *HybridReplicator {
	ctx, cancel := context.WithCancel(context.Background())
	
	return &HybridReplicator{
		writeTracker: &WriteTracker{
			databases:         make(map[string]*DatabaseStats),
			replicationWindow: MaxReplicationDelay,
		},
		rsync:           rsync.NewWrapper(),
		httpRsync:       rsync.NewHTTPRsyncClient(),
		bulkThreshold:   BulkThreshold,
		maxDelay:        MaxReplicationDelay,
		preferHTTPRsync: true, // Default to HTTP rsync
		targets:         targets,
		ctx:             ctx,
		cancel:          cancel,
	}
}

// Start begins the background replication workers
func (h *HybridReplicator) Start() error {
	log.Printf("[DistSQLite] Starting hybrid replicator with %d targets", len(h.targets))
	
	// Start background replication worker
	h.wg.Add(1)
	go h.replicationWorker()
	
	return nil
}

// Stop shuts down the replicator
func (h *HybridReplicator) Stop() error {
	log.Printf("[DistSQLite] Stopping hybrid replicator")
	
	h.cancel()
	h.wg.Wait()
	
	return nil
}

// RecordChange records a database change for potential replication
func (h *HybridReplicator) RecordChange(dbPath string, changeType string, size int64, data []byte) error {
	h.writeTracker.mu.Lock()
	defer h.writeTracker.mu.Unlock()
	
	stats, exists := h.writeTracker.databases[dbPath]
	if !exists {
		stats = &DatabaseStats{
			PendingChanges: make([]Change, 0, TxLogBufferSize),
		}
		h.writeTracker.databases[dbPath] = stats
	}
	
	change := Change{
		Database:  dbPath,
		Timestamp: time.Now(),
		Size:      size,
		Type:      changeType,
		Data:      data,
	}
	
	stats.TotalWrites++
	stats.BytesWritten += size
	stats.LastWrite = time.Now()
	stats.PendingChanges = append(stats.PendingChanges, change)
	
	// Trigger immediate replication if threshold exceeded
	if h.shouldReplicateImmediately(stats) {
		go h.replicateDatabase(dbPath, stats)
	}
	
	return nil
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
	var strategy ReplicationStrategy
	
	if totalSize >= h.bulkThreshold {
		// Large changes - choose rsync strategy
		if h.preferHTTPRsync {
			strategy = StrategyHTTPRsync
		} else {
			strategy = StrategySSHRsync
		}
		err = h.replicateViaBulkSync(dbPath, stats.PendingChanges, strategy)
		log.Printf("[DistSQLite] Used %s for %s", strategy, dbPath)
	} else {
		// Small changes - use transaction log
		strategy = StrategyTxLog
		err = h.replicateViaTxLog(dbPath, stats.PendingChanges)
		log.Printf("[DistSQLite] Used %s for %s", strategy, dbPath)
	}
	
	if err != nil {
		log.Printf("[DistSQLite] Replication failed for %s: %v", dbPath, err)
		return
	}
	
	// Clear pending changes on successful replication
	stats.PendingChanges = stats.PendingChanges[:0]
	stats.LastSync = time.Now()
}

// replicateViaBulkSync uses rsync (HTTP or SSH) for large changes
func (h *HybridReplicator) replicateViaBulkSync(dbPath string, changes []Change, strategy ReplicationStrategy) error {
	if len(h.targets) == 0 {
		return fmt.Errorf("no replication targets configured")
	}
	
	// Ensure the database file exists
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return fmt.Errorf("database file does not exist: %s", dbPath)
	}
	
	// Replicate to all targets using chosen strategy
	for _, target := range h.targets {
		var err error
		
		switch strategy {
		case StrategyHTTPRsync:
			err = h.replicateViaHTTPRsync(dbPath, target)
		case StrategySSHRsync:
			err = h.replicateViaSSHRsync(dbPath, target)
		default:
			err = fmt.Errorf("unknown replication strategy: %s", strategy)
		}
		
		if err != nil {
			log.Printf("[DistSQLite] %s replication failed to %s: %v", strategy, target, err)
			continue // Try other targets
		}
		
		log.Printf("[DistSQLite] %s replication successful to %s", strategy, target)
	}
	
	return nil
}

// replicateViaHTTPRsync replicates using HTTP transport
func (h *HybridReplicator) replicateViaHTTPRsync(dbPath, target string) error {
	// Construct HTTP URL for target
	targetURL := fmt.Sprintf("http://%s", target)
	
	// Use HTTP rsync client to sync database
	stats, err := h.httpRsync.HTTPRsyncSync(h.ctx, dbPath, targetURL)
	if err != nil {
		return fmt.Errorf("HTTP rsync failed: %w", err)
	}
	
	log.Printf("[DistSQLite] HTTP rsync stats: %d pages sent, %d bytes, %dms", 
		stats.PagesSent, stats.BytesSent, stats.ElapsedMs)
	
	return nil
}

// replicateViaSSHRsync replicates using SSH transport (fallback)
func (h *HybridReplicator) replicateViaSSHRsync(dbPath, target string) error {
	targetPath := h.getTargetPath(target, dbPath)
	
	log.Printf("[DistSQLite] SSH rsync syncing %s to %s", dbPath, targetPath)
	
	if err := h.rsync.Sync(dbPath, targetPath); err != nil {
		return fmt.Errorf("SSH rsync failed: %w", err)
	}
	
	return nil
}

// replicateViaTxLog uses transaction log shipping for small changes
func (h *HybridReplicator) replicateViaTxLog(dbPath string, changes []Change) error {
	// TODO: Implement transaction log shipping
	// For now, we'll fall back to bulk sync for small changes too
	// In a full implementation, this would:
	// 1. Package changes into a transaction log format
	// 2. Send to target nodes via HTTP/gRPC
	// 3. Apply changes on target nodes
	
	log.Printf("[DistSQLite] Transaction log replication not yet implemented, using bulk sync")
	strategy := StrategyHTTPRsync
	if !h.preferHTTPRsync {
		strategy = StrategySSHRsync
	}
	return h.replicateViaBulkSync(dbPath, changes, strategy)
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
			PendingChanges: make([]Change, len(dbStats.PendingChanges)),
		}
		copy(statsCopy.PendingChanges, dbStats.PendingChanges)
		stats[dbPath] = statsCopy
	}
	
	return stats
}

// IsHealthy returns true if the replicator is functioning properly
func (h *HybridReplicator) IsHealthy() bool {
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
	}
	
	return true
}