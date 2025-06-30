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
	
	// SQL replay thresholds
	SQLReplayMaxOps = 50        // Max operations for SQL replay
	SQLReplayMaxSize = 64 * 1024 // 64KB max for SQL replay
	
	// Default verification intervals
	DefaultVerificationInterval = 1 * time.Hour   // Configurable safety net
	DefaultQuickVerifyInterval  = 5 * time.Minute // For critical systems
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
	TotalWrites         int64
	BytesWritten        int64
	LastWrite           time.Time
	PendingChanges      []Change
	LastSync            time.Time
	LastVerification    time.Time
	VerificationsPassed int64
	VerificationsFailed int64
	SQLReplayAttempts   int64
	SQLReplayFailures   int64
	PageSyncFallbacks   int64
}

// HybridReplicator manages the hybrid replication strategy
type HybridReplicator struct {
	mu           sync.RWMutex
	writeTracker *WriteTracker
	rsync        *rsync.Wrapper
	httpRsync    *rsync.HTTPRsyncClient
	sqlReplay    *SQLReplayClient
	
	// Configuration
	bulkThreshold          int64
	maxDelay               time.Duration
	preferHTTPRsync        bool
	verificationInterval   time.Duration  // Configurable safety net frequency
	enableQuickVerify      bool           // Enable frequent verification for critical systems
	quickVerifyInterval    time.Duration  // Quick verification frequency
	
	// Replication targets (other nodes)
	targets []string
	
	// Background workers
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// HybridReplicatorConfig holds configuration for the hybrid replicator
type HybridReplicatorConfig struct {
	Targets                []string
	BulkThreshold          int64
	MaxDelay               time.Duration
	VerificationInterval   time.Duration
	EnableQuickVerify      bool
	QuickVerifyInterval    time.Duration
	PreferHTTPRsync        bool
}

// DefaultHybridConfig returns sensible defaults
func DefaultHybridConfig(targets []string) HybridReplicatorConfig {
	return HybridReplicatorConfig{
		Targets:              targets,
		BulkThreshold:        BulkThreshold,
		MaxDelay:             MaxReplicationDelay,
		VerificationInterval: DefaultVerificationInterval,  // 1 hour default
		EnableQuickVerify:    false,                       // Disabled by default
		QuickVerifyInterval:  DefaultQuickVerifyInterval,  // 5 minutes when enabled
		PreferHTTPRsync:      true,
	}
}

// NewHybridReplicator creates a new hybrid replicator
func NewHybridReplicator(targets []string) *HybridReplicator {
	return NewHybridReplicatorWithConfig(DefaultHybridConfig(targets))
}

// NewHybridReplicatorWithConfig creates a new hybrid replicator with custom configuration
func NewHybridReplicatorWithConfig(config HybridReplicatorConfig) *HybridReplicator {
	ctx, cancel := context.WithCancel(context.Background())
	
	return &HybridReplicator{
		writeTracker: &WriteTracker{
			databases:         make(map[string]*DatabaseStats),
			replicationWindow: config.MaxDelay,
		},
		rsync:                rsync.NewWrapper(),
		httpRsync:            rsync.NewHTTPRsyncClient(),
		sqlReplay:            NewSQLReplayClient(),
		bulkThreshold:        config.BulkThreshold,
		maxDelay:             config.MaxDelay,
		preferHTTPRsync:      config.PreferHTTPRsync,
		verificationInterval: config.VerificationInterval,
		enableQuickVerify:    config.EnableQuickVerify,
		quickVerifyInterval:  config.QuickVerifyInterval,
		targets:              config.Targets,
		ctx:                  ctx,
		cancel:               cancel,
	}
}

// Start begins the background replication workers
func (h *HybridReplicator) Start() error {
	log.Printf("[DistSQLite] Starting hybrid replicator with %d targets", len(h.targets))
	log.Printf("[DistSQLite] Verification interval: %v, Quick verify: %v", 
		h.verificationInterval, h.enableQuickVerify)
	
	// Start background replication worker
	h.wg.Add(1)
	go h.replicationWorker()
	
	// Start safety net verification worker
	h.wg.Add(1)
	go h.verificationWorker()
	
	// Start quick verification worker if enabled
	if h.enableQuickVerify {
		h.wg.Add(1)
		go h.quickVerificationWorker()
	}
	
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
	
	// Analyze the changes to determine optimal strategy
	strategy := h.chooseOptimalStrategy(dbPath, stats.PendingChanges, stats.LastSync)
	
	totalSize := int64(0)
	for _, change := range stats.PendingChanges {
		totalSize += change.Size
	}
	
	log.Printf("[DistSQLite] Replicating %s (%d changes, %d bytes) using %s", 
		dbPath, len(stats.PendingChanges), totalSize, strategy)
	
	var err error
	
	switch strategy {
	case StrategyTxLog:
		err = h.replicateViaTxLog(dbPath, stats.PendingChanges)
		
	case StrategyHTTPRsync:
		err = h.replicateViaBulkSync(dbPath, stats.PendingChanges, StrategyHTTPRsync)
		
	case StrategySSHRsync:
		err = h.replicateViaBulkSync(dbPath, stats.PendingChanges, StrategySSHRsync)
		
	default:
		// Fall back to HTTP rsync
		strategy = StrategyHTTPRsync
		err = h.replicateViaBulkSync(dbPath, stats.PendingChanges, StrategyHTTPRsync)
	}
	
	if err != nil {
		log.Printf("[DistSQLite] Replication failed for %s using %s: %v", dbPath, strategy, err)
		return
	}
	
	// Clear pending changes on successful replication
	stats.PendingChanges = stats.PendingChanges[:0]
	stats.LastSync = time.Now()
	
	log.Printf("[DistSQLite] Replication completed for %s using %s", dbPath, strategy)
}

// chooseOptimalStrategy implements the layered strategy selection
func (h *HybridReplicator) chooseOptimalStrategy(dbPath string, changes []Change, lastSync time.Time) ReplicationStrategy {
	changeCount := len(changes)
	totalSize := int64(0)
	for _, change := range changes {
		totalSize += change.Size
	}
	
	timeSinceSync := time.Since(lastSync)
	
	// Real-time scenario: Fast SQL replay for small, frequent changes
	if timeSinceSync < 30*time.Second && 
	   changeCount <= SQLReplayMaxOps && 
	   totalSize <= SQLReplayMaxSize {
		log.Printf("[DistSQLite] Choosing SQL replay: %d ops, %d bytes, %v since last sync", 
			changeCount, totalSize, timeSinceSync)
		return StrategyTxLog
	}
	
	// Medium frequency: Use page sync (potentially with checkpoint)
	if timeSinceSync < 10*time.Minute && totalSize < h.bulkThreshold {
		log.Printf("[DistSQLite] Choosing page sync: %d ops, %d bytes, %v since last sync", 
			changeCount, totalSize, timeSinceSync)
		
		// For medium-sized batches, trigger checkpoint first for better spatial locality
		if changeCount > 20 {
			h.triggerCheckpoint(dbPath)
		}
		
		if h.preferHTTPRsync {
			return StrategyHTTPRsync
		} else {
			return StrategySSHRsync
		}
	}
	
	// Large/infrequent: Always use page hashing (most reliable for catch-up)
	log.Printf("[DistSQLite] Choosing bulk sync: %d ops, %d bytes, %v since last sync", 
		changeCount, totalSize, timeSinceSync)
	
	if h.preferHTTPRsync {
		return StrategyHTTPRsync
	} else {
		return StrategySSHRsync
	}
}

// triggerCheckpoint triggers a WAL checkpoint for better spatial locality
func (h *HybridReplicator) triggerCheckpoint(dbPath string) {
	log.Printf("[DistSQLite] Triggering checkpoint for %s to improve sync efficiency", dbPath)
	// Implementation would go here - for now just log
	// In practice: PRAGMA wal_checkpoint(TRUNCATE)
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

// replicateViaTxLog uses SQL replay for small changes
func (h *HybridReplicator) replicateViaTxLog(dbPath string, changes []Change) error {
	h.writeTracker.mu.Lock()
	stats := h.writeTracker.databases[dbPath]
	if stats != nil {
		stats.SQLReplayAttempts++
	}
	h.writeTracker.mu.Unlock()
	
	// Extract SQL statements from changes
	statements, err := h.sqlReplay.ExtractSQLFromChanges(changes)
	if err != nil {
		log.Printf("[DistSQLite] Failed to extract SQL from changes: %v, falling back to page sync", err)
		return h.fallbackToPageSync(dbPath, changes)
	}
	
	if len(statements) == 0 {
		log.Printf("[DistSQLite] No SQL statements extracted, skipping replay")
		return nil
	}
	
	// Create batch
	batch := SQLReplayBatch{
		Database:   dbPath,
		Statements: statements,
		BatchID:    fmt.Sprintf("batch_%d_%s", time.Now().Unix(), filepath.Base(dbPath)),
		CreatedAt:  time.Now(),
		FenceToken: time.Now().Unix(), // Simple fence token
	}
	
	// Send to all targets
	var lastErr error
	successCount := 0
	
	for _, target := range h.targets {
		ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
		err := h.sqlReplay.SendSQLBatch(ctx, target, batch)
		cancel()
		
		if err != nil {
			log.Printf("[DistSQLite] SQL replay failed to %s: %v", target, err)
			lastErr = err
		} else {
			successCount++
			log.Printf("[DistSQLite] SQL replay successful to %s (%d statements)", 
				target, len(statements))
		}
	}
	
	// Update stats
	h.writeTracker.mu.Lock()
	if stats != nil {
		if successCount == 0 {
			stats.SQLReplayFailures++
		}
	}
	h.writeTracker.mu.Unlock()
	
	// If all targets failed, fall back to page sync
	if successCount == 0 && lastErr != nil {
		log.Printf("[DistSQLite] All SQL replay attempts failed, falling back to page sync")
		return h.fallbackToPageSync(dbPath, changes)
	}
	
	log.Printf("[DistSQLite] SQL replay completed: %d/%d targets successful", 
		successCount, len(h.targets))
	
	return nil
}

// fallbackToPageSync falls back to page-level sync when SQL replay fails
func (h *HybridReplicator) fallbackToPageSync(dbPath string, changes []Change) error {
	h.writeTracker.mu.Lock()
	stats := h.writeTracker.databases[dbPath]
	if stats != nil {
		stats.PageSyncFallbacks++
	}
	h.writeTracker.mu.Unlock()
	
	strategy := StrategyHTTPRsync
	if !h.preferHTTPRsync {
		strategy = StrategySSHRsync
	}
	
	log.Printf("[DistSQLite] Falling back to %s for %s", strategy, dbPath)
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

// verificationWorker runs periodic safety net verification using page hashing
func (h *HybridReplicator) verificationWorker() {
	defer h.wg.Done()
	
	ticker := time.NewTicker(h.verificationInterval)
	defer ticker.Stop()
	
	log.Printf("[DistSQLite] Safety net verification worker started (interval: %v)", 
		h.verificationInterval)
	
	for {
		select {
		case <-h.ctx.Done():
			log.Printf("[DistSQLite] Safety net verification worker shutting down")
			return
			
		case <-ticker.C:
			h.performSafetyNetVerification()
		}
	}
}

// quickVerificationWorker runs frequent verification for critical systems
func (h *HybridReplicator) quickVerificationWorker() {
	defer h.wg.Done()
	
	ticker := time.NewTicker(h.quickVerifyInterval)
	defer ticker.Stop()
	
	log.Printf("[DistSQLite] Quick verification worker started (interval: %v)", 
		h.quickVerifyInterval)
	
	for {
		select {
		case <-h.ctx.Done():
			log.Printf("[DistSQLite] Quick verification worker shutting down")
			return
			
		case <-ticker.C:
			h.performQuickVerification()
		}
	}
}

// performSafetyNetVerification runs comprehensive page-level verification
func (h *HybridReplicator) performSafetyNetVerification() {
	h.writeTracker.mu.RLock()
	databases := make(map[string]*DatabaseStats)
	for path, stats := range h.writeTracker.databases {
		databases[path] = stats
	}
	h.writeTracker.mu.RUnlock()
	
	log.Printf("[DistSQLite] Starting safety net verification for %d databases", len(databases))
	
	for dbPath, stats := range databases {
		// Skip if recently verified
		if time.Since(stats.LastVerification) < h.verificationInterval/2 {
			continue
		}
		
		if err := h.verifyDatabaseConsistency(dbPath, stats, true); err != nil {
			log.Printf("[DistSQLite] Safety net verification failed for %s: %v", dbPath, err)
		}
	}
}

// performQuickVerification runs lightweight verification checks
func (h *HybridReplicator) performQuickVerification() {
	h.writeTracker.mu.RLock()
	databases := make(map[string]*DatabaseStats)
	for path, stats := range h.writeTracker.databases {
		// Only verify recently active databases for quick checks
		if time.Since(stats.LastWrite) < h.quickVerifyInterval*2 {
			databases[path] = stats
		}
	}
	h.writeTracker.mu.RUnlock()
	
	if len(databases) == 0 {
		return
	}
	
	log.Printf("[DistSQLite] Starting quick verification for %d active databases", len(databases))
	
	for dbPath, stats := range databases {
		if err := h.verifyDatabaseConsistency(dbPath, stats, false); err != nil {
			log.Printf("[DistSQLite] Quick verification failed for %s: %v", dbPath, err)
		}
	}
}

// verifyDatabaseConsistency verifies database consistency using page hashing
func (h *HybridReplicator) verifyDatabaseConsistency(dbPath string, stats *DatabaseStats, fullVerification bool) error {
	if len(h.targets) == 0 {
		return nil // No targets to verify against
	}
	
	log.Printf("[DistSQLite] Verifying consistency for %s (full: %v)", dbPath, fullVerification)
	
	// Use page hashing as the source of truth
	strategy := StrategyHTTPRsync
	if !h.preferHTTPRsync {
		strategy = StrategySSHRsync
	}
	
	verificationStart := time.Now()
	
	// Run verification sync (dry-run first to check differences)
	var err error
	switch strategy {
	case StrategyHTTPRsync:
		err = h.verifyViaHTTPRsync(dbPath, fullVerification)
	case StrategySSHRsync:
		err = h.verifyViaSSHRsync(dbPath, fullVerification)
	}
	
	// Update stats
	h.writeTracker.mu.Lock()
	stats.LastVerification = time.Now()
	if err != nil {
		stats.VerificationsFailed++
		log.Printf("[DistSQLite] Verification FAILED for %s: %v (took %v)", 
			dbPath, err, time.Since(verificationStart))
	} else {
		stats.VerificationsPassed++
		log.Printf("[DistSQLite] Verification PASSED for %s (took %v)", 
			dbPath, time.Since(verificationStart))
	}
	h.writeTracker.mu.Unlock()
	
	return err
}

// verifyViaHTTPRsync verifies consistency using HTTP rsync
func (h *HybridReplicator) verifyViaHTTPRsync(dbPath string, fullVerification bool) error {
	// For verification, we do a dry-run first to check for differences
	// If differences are found, we can either:
	// 1. Log the inconsistency
	// 2. Automatically repair by running a real sync
	// 3. Alert operators
	
	for _, target := range h.targets {
		ctx, cancel := context.WithTimeout(h.ctx, 60*time.Second)
		
		// Use dry-run mode to detect differences without applying changes
		err := h.httpRsync.VerifyConsistency(ctx, dbPath, target)
		cancel()
		
		if err != nil {
			log.Printf("[DistSQLite] Consistency check failed for %s -> %s: %v", 
				dbPath, target, err)
			
			// If this is a full verification and we found inconsistencies,
			// automatically repair them
			if fullVerification {
				log.Printf("[DistSQLite] Auto-repairing inconsistency for %s -> %s", 
					dbPath, target)
				
				repairCtx, repairCancel := context.WithTimeout(h.ctx, 120*time.Second)
				repairErr := h.httpRsync.SyncDatabase(repairCtx, dbPath, target)
				repairCancel()
				
				if repairErr != nil {
					return fmt.Errorf("repair failed: %w", repairErr)
				}
				
				log.Printf("[DistSQLite] Auto-repair completed for %s -> %s", 
					dbPath, target)
			} else {
				return err
			}
		}
	}
	
	return nil
}

// verifyViaSSHRsync verifies consistency using SSH rsync  
func (h *HybridReplicator) verifyViaSSHRsync(dbPath string, fullVerification bool) error {
	// Similar to HTTP verification but using SSH rsync
	for _, target := range h.targets {
		ctx, cancel := context.WithTimeout(h.ctx, 60*time.Second)
		
		// Use rsync's dry-run mode
		err := h.rsync.VerifyFile(ctx, dbPath, target)
		cancel()
		
		if err != nil {
			log.Printf("[DistSQLite] SSH consistency check failed for %s -> %s: %v", 
				dbPath, target, err)
			
			if fullVerification {
				log.Printf("[DistSQLite] Auto-repairing inconsistency via SSH for %s -> %s", 
					dbPath, target)
				
				repairCtx, repairCancel := context.WithTimeout(h.ctx, 120*time.Second)
				repairErr := h.rsync.SyncFile(repairCtx, dbPath, target)
				repairCancel()
				
				if repairErr != nil {
					return fmt.Errorf("SSH repair failed: %w", repairErr)
				}
				
				log.Printf("[DistSQLite] SSH auto-repair completed for %s -> %s", 
					dbPath, target)
			} else {
				return err
			}
		}
	}
	
	return nil
}