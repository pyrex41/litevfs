package lease

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

// LeaseTransferManager handles graceful lease migration
type LeaseTransferManager struct {
	leaseManager LeaseManager
	quiescing    int32 // atomic flag for quiescing mode
	pendingOps   int32 // count of pending operations
}

// LeaseManager interface for different implementations
type LeaseManager interface {
	IsLeaseHolder() bool
	GetCurrentFenceToken() int64
	ValidateWrite(fenceToken int64) error
	ReleaseLease(ctx context.Context) error
}

// NewLeaseTransferManager creates a new transfer manager
func NewLeaseTransferManager(lm LeaseManager) *LeaseTransferManager {
	return &LeaseTransferManager{
		leaseManager: lm,
	}
}

// BeginWrite checks if writes are allowed and increments pending operations
func (ltm *LeaseTransferManager) BeginWrite(fenceToken int64) error {
	// Check if we're in quiescing mode
	if atomic.LoadInt32(&ltm.quiescing) != 0 {
		return fmt.Errorf("node is quiescing - no new writes accepted")
	}
	
	// Validate fence token
	if err := ltm.leaseManager.ValidateWrite(fenceToken); err != nil {
		return err
	}
	
	// Increment pending operations
	atomic.AddInt32(&ltm.pendingOps, 1)
	return nil
}

// EndWrite decrements pending operations count
func (ltm *LeaseTransferManager) EndWrite() {
	atomic.AddInt32(&ltm.pendingOps, -1)
}

// PrepareLeaseTransfer initiates graceful lease migration
func (ltm *LeaseTransferManager) PrepareLeaseTransfer(ctx context.Context) error {
	if !ltm.leaseManager.IsLeaseHolder() {
		return fmt.Errorf("not the lease holder")
	}
	
	log.Printf("[Lease Migration] Starting graceful lease transfer")
	
	// Step 1: Enter quiescing mode (stop accepting new writes)
	atomic.StoreInt32(&ltm.quiescing, 1)
	log.Printf("[Lease Migration] Entered quiescing mode - no new writes accepted")
	
	// Step 2: Wait for pending writes to complete
	timeout := 5 * time.Second
	deadline := time.Now().Add(timeout)
	
	for time.Now().Before(deadline) {
		pending := atomic.LoadInt32(&ltm.pendingOps)
		if pending == 0 {
			log.Printf("[Lease Migration] All pending writes completed")
			break
		}
		
		log.Printf("[Lease Migration] Waiting for %d pending writes to complete", pending)
		time.Sleep(100 * time.Millisecond)
	}
	
	// Check if we timed out
	if pending := atomic.LoadInt32(&ltm.pendingOps); pending > 0 {
		// Reset quiescing mode and return error
		atomic.StoreInt32(&ltm.quiescing, 0)
		return fmt.Errorf("timeout waiting for %d pending writes to complete", pending)
	}
	
	log.Printf("[Lease Migration] Ready for lease transfer")
	return nil
}

// CompleteLeasetransfer finalizes the lease migration
func (ltm *LeaseTransferManager) CompleteleaseTransfer(ctx context.Context) error {
	defer func() {
		// Reset quiescing mode regardless of outcome
		atomic.StoreInt32(&ltm.quiescing, 0)
	}()
	
	// Release the lease
	if err := ltm.leaseManager.ReleaseLease(ctx); err != nil {
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

// SmartLeaseHolderSelection helps choose the best next lease holder
type SmartLeaseHolderSelection struct {
	nodes map[string]*NodeMetrics
}

// NodeMetrics tracks metrics for lease holder selection
type NodeMetrics struct {
	NodeID          string
	LastSeen        time.Time
	WriteLatency    time.Duration
	ReplicationLag  time.Duration
	ActiveConnections int
	Region          string
	LoadAverage     float64
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
	return hmt.transferManager.CompleteleaseTransfer(ctx)
}