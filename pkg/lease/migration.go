package lease

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

// LeaseTransferManager handles graceful lease migration without disrupting active operations.
// It provides mechanisms for quiescing write operations, waiting for completion,
// and safely transferring lease ownership to another node.
//
// The manager uses atomic operations for thread-safe state management and prevents
// race conditions during the transfer process.
type LeaseTransferManager struct {
	leaseManager LeaseManager
	quiescing    int32 // atomic flag for quiescing mode
	pendingOps   int32 // count of pending operations
	
	// Configuration
	quiesceTimeout time.Duration
	maxPendingOps  int32 // prevent overflow
}

// LeaseManager defines the interface for lease management implementations.
// This interface abstracts different lease coordination mechanisms (HTTP, Consul, etc.)
// and provides consistent operations for lease validation and lifecycle management.
type LeaseManager interface {
	// IsLeaseHolder returns true if this node currently holds the lease
	IsLeaseHolder() bool
	
	// GetCurrentFenceToken returns the current fence token for the lease
	GetCurrentFenceToken() int64
	
	// ValidateWrite validates a write operation using the provided fence token
	ValidateWrite(fenceToken int64) error
	
	// ReleaseLease releases the current lease, allowing other nodes to acquire it
	ReleaseLease(ctx context.Context) error
}

// NewLeaseTransferManager creates a new transfer manager
func NewLeaseTransferManager(lm LeaseManager) *LeaseTransferManager {
	return &LeaseTransferManager{
		leaseManager:   lm,
		quiesceTimeout: 5 * time.Second,
		maxPendingOps:  10000, // prevent overflow
	}
}

// SetQuiesceTimeout configures the timeout for waiting for pending operations
func (ltm *LeaseTransferManager) SetQuiesceTimeout(timeout time.Duration) {
	ltm.quiesceTimeout = timeout
}

// BeginWrite checks if writes are allowed and increments pending operations
func (ltm *LeaseTransferManager) BeginWrite(fenceToken int64) error {
	// Validate fence token first
	if err := ltm.leaseManager.ValidateWrite(fenceToken); err != nil {
		return fmt.Errorf("fence token validation failed: %w", err)
	}
	
	// Atomically check quiescing state and increment pending ops
	for {
		current := atomic.LoadInt32(&ltm.pendingOps)
		
		// Check bounds to prevent overflow
		if current >= ltm.maxPendingOps {
			return fmt.Errorf("too many pending operations: %d", current)
		}
		
		// Check if we're in quiescing mode
		if atomic.LoadInt32(&ltm.quiescing) != 0 {
			return fmt.Errorf("node is quiescing - no new writes accepted")
		}
		
		// Try to increment atomically
		if atomic.CompareAndSwapInt32(&ltm.pendingOps, current, current+1) {
			// Double-check quiescing state after increment (prevent race)
			if atomic.LoadInt32(&ltm.quiescing) != 0 {
				// We incremented but now we're quiescing - undo and fail
				atomic.AddInt32(&ltm.pendingOps, -1)
				return fmt.Errorf("node is quiescing - no new writes accepted")
			}
			break
		}
		// CAS failed, retry
	}
	
	return nil
}

// EndWrite decrements pending operations count
func (ltm *LeaseTransferManager) EndWrite() {
	// Ensure we don't go below zero
	for {
		current := atomic.LoadInt32(&ltm.pendingOps)
		if current <= 0 {
			// Already at zero or below - this is unexpected but handle gracefully
			log.Printf("[Lease Migration] Warning: EndWrite called with no pending operations")
			return
		}
		
		if atomic.CompareAndSwapInt32(&ltm.pendingOps, current, current-1) {
			return
		}
		// CAS failed, retry
	}
}

// PrepareLeaseTransfer initiates graceful lease migration
func (ltm *LeaseTransferManager) PrepareLeaseTransfer(ctx context.Context) error {
	if ltm.leaseManager == nil {
		return fmt.Errorf("lease manager is nil")
	}
	
	if !ltm.leaseManager.IsLeaseHolder() {
		return fmt.Errorf("not the lease holder")
	}
	
	log.Printf("[Lease Migration] Starting graceful lease transfer")
	
	// Step 1: Enter quiescing mode (stop accepting new writes)
	atomic.StoreInt32(&ltm.quiescing, 1)
	log.Printf("[Lease Migration] Entered quiescing mode - no new writes accepted")
	
	// Step 2: Wait for pending writes to complete with context cancellation
	timeoutCtx, cancel := context.WithTimeout(ctx, ltm.quiesceTimeout)
	defer cancel()
	
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-timeoutCtx.Done():
			pending := atomic.LoadInt32(&ltm.pendingOps)
			// Reset quiescing mode on timeout
			atomic.StoreInt32(&ltm.quiescing, 0)
			if timeoutCtx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timeout waiting for %d pending writes to complete", pending)
			}
			return fmt.Errorf("context cancelled while waiting for pending writes: %w", timeoutCtx.Err())
			
		case <-ticker.C:
			pending := atomic.LoadInt32(&ltm.pendingOps)
			if pending == 0 {
				log.Printf("[Lease Migration] All pending writes completed")
				return nil
			}
			log.Printf("[Lease Migration] Waiting for %d pending writes to complete", pending)
		}
	}
}

// CompleteLeaseTransfer finalizes the lease migration
func (ltm *LeaseTransferManager) CompleteLeaseTransfer(ctx context.Context) error {
	// Release the lease first
	err := ltm.leaseManager.ReleaseLease(ctx)
	
	// Always reset quiescing mode, but preserve the original error
	atomic.StoreInt32(&ltm.quiescing, 0)
	
	if err != nil {
		return fmt.Errorf("failed to release lease: %w", err)
	}
	
	log.Printf("[Lease Migration] Lease transfer completed successfully")
	return nil
}

// IsQuiescing returns true if the node is in quiescing mode
func (ltm *LeaseTransferManager) IsQuiescing() bool {
	return atomic.LoadInt32(&ltm.quiescing) != 0
}

// GetPendingOperationsCount returns the number of pending operations
func (ltm *LeaseTransferManager) GetPendingOperationsCount() int32 {
	return atomic.LoadInt32(&ltm.pendingOps)
}

// SmartLeaseHolderSelection implements intelligent lease holder selection
// based on node metrics and performance characteristics. It considers factors
// like region preference, latency, load, and replication lag to choose the
// optimal node for lease ownership.
type SmartLeaseHolderSelection struct {
	nodes map[string]*NodeMetrics
}

// NodeMetrics tracks performance and operational metrics for lease holder selection.
// These metrics are used to score nodes and select the best candidate for lease ownership.
type NodeMetrics struct {
	NodeID            string        // Unique identifier for the node
	LastSeen          time.Time     // Last time this node was observed (for staleness)
	WriteLatency      time.Duration // Average write operation latency
	ReplicationLag    time.Duration // Current replication lag behind primary
	ActiveConnections int           // Number of active database connections
	Region            string        // Geographic or logical region
	LoadAverage       float64       // System load average
}

// SelectBestLeaseHolder chooses the optimal node for lease transfer
func (sls *SmartLeaseHolderSelection) SelectBestLeaseHolder(currentRegion string) (string, error) {
	if len(sls.nodes) == 0 {
		return "", fmt.Errorf("no available nodes")
	}
	
	var bestNode *NodeMetrics
	bestScore := float64(-1)
	
	for _, node := range sls.nodes {
		// Skip nodes that haven't been seen recently
		if time.Since(node.LastSeen) > 30*time.Second {
			continue
		}
		
		score := sls.calculateNodeScore(node, currentRegion)
		if score > bestScore {
			bestScore = score
			bestNode = node
		}
	}
	
	if bestNode == nil {
		return "", fmt.Errorf("no suitable lease holder found")
	}
	
	log.Printf("[Lease Selection] Selected %s as best lease holder (score: %.2f)", 
		bestNode.NodeID, bestScore)
	
	return bestNode.NodeID, nil
}

// calculateNodeScore calculates a score for lease holder selection
func (sls *SmartLeaseHolderSelection) calculateNodeScore(node *NodeMetrics, currentRegion string) float64 {
	score := 100.0 // Base score
	
	// Prefer nodes in the same region
	if node.Region == currentRegion {
		score += 50.0
	}
	
	// Penalize high latency
	if node.WriteLatency > 100*time.Millisecond {
		score -= 30.0
	}
	
	// Penalize high replication lag  
	if node.ReplicationLag > 1*time.Second {
		score -= 40.0
	}
	
	// Penalize high load
	if node.LoadAverage > 2.0 {
		score -= 20.0
	}
	
	// Prefer nodes with fewer active connections
	score -= float64(node.ActiveConnections) * 0.1
	
	return score
}

// Example usage integration with existing managers
type HTTPManagerWithTransfer struct {
	*HTTPManager
	transferManager *LeaseTransferManager
}

// NewHTTPManagerWithTransfer creates an HTTP manager with transfer capabilities
func NewHTTPManagerWithTransfer(baseURL, key, hostname, advertiseURL string) *HTTPManagerWithTransfer {
	httpManager := NewHTTPManager(baseURL, key, hostname, advertiseURL)
	transferManager := NewLeaseTransferManager(httpManager)
	
	return &HTTPManagerWithTransfer{
		HTTPManager:     httpManager,
		transferManager: transferManager,
	}
}

// ValidateWriteWithQuiescing validates writes considering quiescing mode
func (hmt *HTTPManagerWithTransfer) ValidateWriteWithQuiescing(fenceToken int64) error {
	if hmt.transferManager == nil {
		return fmt.Errorf("transfer manager is nil")
	}
	if fenceToken <= 0 {
		return fmt.Errorf("invalid fence token: %d", fenceToken)
	}
	return hmt.transferManager.BeginWrite(fenceToken)
}

// EndWrite should be called after write completion
func (hmt *HTTPManagerWithTransfer) EndWrite() {
	hmt.transferManager.EndWrite()
}

// PrepareTransfer initiates graceful lease transfer
func (hmt *HTTPManagerWithTransfer) PrepareTransfer(ctx context.Context) error {
	return hmt.transferManager.PrepareLeaseTransfer(ctx)
}

// CompleteTransfer finalizes lease transfer
func (hmt *HTTPManagerWithTransfer) CompleteTransfer(ctx context.Context) error {
	return hmt.transferManager.CompleteLeaseTransfer(ctx)
}