package replication

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"sync"
	"time"
)

// SecureWALReplicator handles secure WAL-based replication via ZMQ
type SecureWALReplicator struct {
	nodeID    string
	transport WALTransport
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	
	// WAL state tracking
	databases map[string]*WALDatabaseState
	
	// Security
	fenceToken int64
}

// WALTransport defines the interface for secure WAL transport
type WALTransport interface {
	PublishWALPage(ctx context.Context, page *WALPage) error
	SubscribeWALPages(ctx context.Context, callback func(*WALPage)) error
	IsAuthenticated() bool
	GetNodeID() string
}

// WALPage represents a secure WAL page for replication
type WALPage struct {
	// Identification
	DatabasePath string    `msgpack:"db_path"`
	PageNumber   uint32    `msgpack:"page_num"`
	WALSequence  uint64    `msgpack:"wal_seq"`
	Timestamp    time.Time `msgpack:"timestamp"`
	
	// Security
	FenceToken int64     `msgpack:"fence_token"`
	NodeID     string    `msgpack:"node_id"`
	Checksum   [32]byte  `msgpack:"checksum"`
	
	// Data (binary WAL data, not SQL)
	WALData    []byte    `msgpack:"wal_data"`
	PageSize   int       `msgpack:"page_size"`
	
	// Metadata
	IsCommit   bool      `msgpack:"is_commit"`
	TxnID      uint64    `msgpack:"txn_id"`
}

// WALDatabaseState tracks replication state for a database
type WALDatabaseState struct {
	DatabasePath    string
	LastWALSequence uint64
	LastCheckpoint  time.Time
	FenceToken      int64
	ReplicaNodes    []string
}

// NewSecureWALReplicator creates a new secure WAL replicator
func NewSecureWALReplicator(nodeID string, transport WALTransport) *SecureWALReplicator {
	ctx, cancel := context.WithCancel(context.Background())
	
	return &SecureWALReplicator{
		nodeID:    nodeID,
		transport: transport,
		ctx:       ctx,
		cancel:    cancel,
		databases: make(map[string]*WALDatabaseState),
		fenceToken: time.Now().UnixNano(),
	}
}

// Start begins secure WAL replication
func (w *SecureWALReplicator) Start() error {
	if !w.transport.IsAuthenticated() {
		return fmt.Errorf("transport not authenticated - CURVE security required")
	}
	
	// Start WAL page subscriber
	go func() {
		if err := w.transport.SubscribeWALPages(w.ctx, w.handleIncomingWALPage); err != nil {
			log.Printf("[Secure WAL] Subscription error: %v", err)
		}
	}()
	
	log.Printf("[Secure WAL] Started replicator for node %s", w.nodeID)
	return nil
}

// Stop shuts down the replicator
func (w *SecureWALReplicator) Stop() error {
	w.cancel()
	return nil
}

// ReplicateWALPages publishes WAL pages for replication (Primary only)
func (w *SecureWALReplicator) ReplicateWALPages(dbPath string, walData []byte, walSequence uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	
	// Ensure we have database state
	dbState, exists := w.databases[dbPath]
	if !exists {
		dbState = &WALDatabaseState{
			DatabasePath:    dbPath,
			LastWALSequence: 0,
			FenceToken:      w.fenceToken,
		}
		w.databases[dbPath] = dbState
	}
	
	// Create secure WAL pages from binary WAL data
	pages, err := w.extractWALPages(dbPath, walData, walSequence)
	if err != nil {
		return fmt.Errorf("failed to extract WAL pages: %w", err)
	}
	
	// Publish each page securely
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	
	for _, page := range pages {
		// Add security metadata
		page.FenceToken = dbState.FenceToken
		page.NodeID = w.nodeID
		page.Checksum = w.calculateChecksum(page)
		
		if err := w.transport.PublishWALPage(ctx, page); err != nil {
			log.Printf("[Secure WAL] Failed to publish page %d for %s: %v", 
				page.PageNumber, dbPath, err)
			continue
		}
		
		log.Printf("[Secure WAL] Published page %d, seq %d for %s", 
			page.PageNumber, page.WALSequence, dbPath)
	}
	
	// Update state
	dbState.LastWALSequence = walSequence
	
	return nil
}

// extractWALPages extracts individual pages from WAL data
func (w *SecureWALReplicator) extractWALPages(dbPath string, walData []byte, walSequence uint64) ([]*WALPage, error) {
	var pages []*WALPage
	
	// Parse WAL file format
	// This is a simplified version - real implementation would parse actual WAL format
	
	if len(walData) < 32 {
		return nil, fmt.Errorf("WAL data too short")
	}
	
	// WAL header parsing (simplified)
	offset := 32 // Skip WAL header
	pageSize := 4096 // Default SQLite page size
	
	for offset+pageSize <= len(walData) {
		pageData := walData[offset : offset+pageSize]
		
		// Extract page number from WAL frame header
		if len(pageData) < 24 {
			break
		}
		
		pageNumber := binary.BigEndian.Uint32(pageData[0:4])
		if pageNumber == 0 {
			break // End of valid pages
		}
		
		page := &WALPage{
			DatabasePath: dbPath,
			PageNumber:   pageNumber,
			WALSequence:  walSequence,
			Timestamp:    time.Now(),
			WALData:      make([]byte, len(pageData)),
			PageSize:     pageSize,
			TxnID:        walSequence, // Simplified
		}
		
		copy(page.WALData, pageData)
		pages = append(pages, page)
		
		offset += pageSize + 24 // Page + WAL frame header
	}
	
	if len(pages) > 0 {
		// Mark last page as commit
		pages[len(pages)-1].IsCommit = true
	}
	
	return pages, nil
}

// handleIncomingWALPage processes WAL pages from other nodes (Replica mode)
func (w *SecureWALReplicator) handleIncomingWALPage(page *WALPage) {
	// Verify security
	if !w.verifyWALPage(page) {
		log.Printf("[Secure WAL] Security verification failed for page from %s", page.NodeID)
		return
	}
	
	w.mu.Lock()
	defer w.mu.Unlock()
	
	// Get or create database state
	dbState, exists := w.databases[page.DatabasePath]
	if !exists {
		dbState = &WALDatabaseState{
			DatabasePath:    page.DatabasePath,
			LastWALSequence: 0,
		}
		w.databases[page.DatabasePath] = dbState
	}
	
	// Check sequence ordering
	if page.WALSequence <= dbState.LastWALSequence {
		log.Printf("[Secure WAL] Ignoring old WAL page seq %d (current: %d)", 
			page.WALSequence, dbState.LastWALSequence)
		return
	}
	
	// Apply WAL page to local database
	if err := w.applyWALPage(page); err != nil {
		log.Printf("[Secure WAL] Failed to apply WAL page: %v", err)
		return
	}
	
	// Update state
	dbState.LastWALSequence = page.WALSequence
	
	log.Printf("[Secure WAL] Applied page %d, seq %d from %s to %s", 
		page.PageNumber, page.WALSequence, page.NodeID, page.DatabasePath)
}

// verifyWALPage verifies security properties of a WAL page
func (w *SecureWALReplicator) verifyWALPage(page *WALPage) bool {
	// Verify checksum
	expectedChecksum := w.calculateChecksum(page)
	if page.Checksum != expectedChecksum {
		log.Printf("[Secure WAL] Checksum mismatch for page %d", page.PageNumber)
		return false
	}
	
	// Verify fence token (basic ordering)
	if page.FenceToken <= 0 {
		log.Printf("[Secure WAL] Invalid fence token: %d", page.FenceToken)
		return false
	}
	
	// Verify node ID is not empty
	if page.NodeID == "" {
		log.Printf("[Secure WAL] Empty node ID")
		return false
	}
	
	// Verify timestamp is reasonable (within last hour)
	if time.Since(page.Timestamp) > time.Hour {
		log.Printf("[Secure WAL] Page timestamp too old: %v", page.Timestamp)
		return false
	}
	
	// Verify WAL data size
	if len(page.WALData) == 0 || len(page.WALData) > 64*1024 {
		log.Printf("[Secure WAL] Invalid WAL data size: %d", len(page.WALData))
		return false
	}
	
	return true
}

// calculateChecksum computes SHA-256 checksum of WAL page
func (w *SecureWALReplicator) calculateChecksum(page *WALPage) [32]byte {
	hasher := sha256.New()
	
	// Hash critical fields
	binary.Write(hasher, binary.BigEndian, page.PageNumber)
	binary.Write(hasher, binary.BigEndian, page.WALSequence)
	binary.Write(hasher, binary.BigEndian, page.FenceToken)
	hasher.Write([]byte(page.NodeID))
	hasher.Write([]byte(page.DatabasePath))
	hasher.Write(page.WALData)
	
	var checksum [32]byte
	copy(checksum[:], hasher.Sum(nil))
	return checksum
}

// applyWALPage applies a WAL page to the local database
func (w *SecureWALReplicator) applyWALPage(page *WALPage) error {
	// This would integrate with the SQLite WAL mechanism
	// For now, we log the operation
	
	log.Printf("[Secure WAL] Applying page %d (%d bytes) to %s", 
		page.PageNumber, len(page.WALData), page.DatabasePath)
	
	// Real implementation would:
	// 1. Open database connection
	// 2. Apply WAL page data directly to the database
	// 3. Handle commit/rollback based on IsCommit flag
	// 4. Update WAL checkpoint if needed
	
	if page.IsCommit {
		log.Printf("[Secure WAL] Committing transaction %d for %s", 
			page.TxnID, page.DatabasePath)
	}
	
	return nil
}

// GetReplicationStatus returns current replication status
func (w *SecureWALReplicator) GetReplicationStatus() map[string]*WALDatabaseState {
	w.mu.RLock()
	defer w.mu.RUnlock()
	
	status := make(map[string]*WALDatabaseState)
	for path, state := range w.databases {
		// Create a copy to avoid race conditions
		status[path] = &WALDatabaseState{
			DatabasePath:    state.DatabasePath,
			LastWALSequence: state.LastWALSequence,
			LastCheckpoint:  state.LastCheckpoint,
			FenceToken:      state.FenceToken,
			ReplicaNodes:    append([]string{}, state.ReplicaNodes...),
		}
	}
	
	return status
}

// SetFenceToken updates the fence token (for lease management integration)
func (w *SecureWALReplicator) SetFenceToken(token int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	
	w.fenceToken = token
	
	// Update all database states
	for _, state := range w.databases {
		state.FenceToken = token
	}
	
	log.Printf("[Secure WAL] Updated fence token to %d", token)
}