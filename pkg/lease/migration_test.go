package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// MockLeaseManager for testing
type MockLeaseManager struct {
	isHolder    bool
	fenceToken  int64
	releaseCalled bool
	validateFail  bool
	releaseFail   bool
	mu           sync.RWMutex
}

func (m *MockLeaseManager) IsLeaseHolder() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isHolder
}

func (m *MockLeaseManager) GetCurrentFenceToken() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.fenceToken
}

func (m *MockLeaseManager) ValidateWrite(fenceToken int64) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	if m.validateFail {
		return errors.New("validation failed")
	}
	if fenceToken != m.fenceToken {
		return errors.New("invalid fence token")
	}
	return nil
}

func (m *MockLeaseManager) ReleaseLease(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.releaseCalled = true
	if m.releaseFail {
		return errors.New("release failed")
	}
	m.isHolder = false
	return nil
}

func (m *MockLeaseManager) SetLeaseHolder(holder bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isHolder = holder
}

func (m *MockLeaseManager) SetFenceToken(token int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fenceToken = token
}

func (m *MockLeaseManager) SetValidateFail(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validateFail = fail
}

func (m *MockLeaseManager) SetReleaseFail(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseFail = fail
}

func (m *MockLeaseManager) WasReleaseCalled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.releaseCalled
}

func TestLeaseTransferManager_NewLeaseTransferManager(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	if ltm.leaseManager != mockLM {
		t.Error("Expected lease manager to be set")
	}
	
	if ltm.quiesceTimeout != 5*time.Second {
		t.Errorf("Expected default timeout 5s, got %v", ltm.quiesceTimeout)
	}
	
	if ltm.maxPendingOps != 10000 {
		t.Errorf("Expected max pending ops 10000, got %d", ltm.maxPendingOps)
	}
}

func TestLeaseTransferManager_BeginWrite_Success(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	err := ltm.BeginWrite(123)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	
	if ltm.GetPendingOperationsCount() != 1 {
		t.Errorf("Expected 1 pending operation, got %d", ltm.GetPendingOperationsCount())
	}
}

func TestLeaseTransferManager_BeginWrite_InvalidFenceToken(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	err := ltm.BeginWrite(999) // Wrong fence token
	if err == nil {
		t.Error("Expected error for invalid fence token")
	}
	
	if ltm.GetPendingOperationsCount() != 0 {
		t.Errorf("Expected 0 pending operations after failed write, got %d", ltm.GetPendingOperationsCount())
	}
}

func TestLeaseTransferManager_BeginWrite_QuiescingMode(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	// Set quiescing mode
	atomic.StoreInt32(&ltm.quiescing, 1)
	
	err := ltm.BeginWrite(123)
	if err == nil {
		t.Error("Expected error when in quiescing mode")
	}
	
	if ltm.GetPendingOperationsCount() != 0 {
		t.Errorf("Expected 0 pending operations when quiescing, got %d", ltm.GetPendingOperationsCount())
	}
}

func TestLeaseTransferManager_BeginWrite_OverflowProtection(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	ltm.maxPendingOps = 2 // Set low limit for testing
	
	// First two writes should succeed
	if err := ltm.BeginWrite(123); err != nil {
		t.Errorf("First write failed: %v", err)
	}
	if err := ltm.BeginWrite(123); err != nil {
		t.Errorf("Second write failed: %v", err)
	}
	
	// Third write should fail due to overflow protection
	if err := ltm.BeginWrite(123); err == nil {
		t.Error("Expected overflow protection to prevent third write")
	}
}

func TestLeaseTransferManager_EndWrite(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	// Begin a write
	ltm.BeginWrite(123)
	if ltm.GetPendingOperationsCount() != 1 {
		t.Errorf("Expected 1 pending operation, got %d", ltm.GetPendingOperationsCount())
	}
	
	// End the write
	ltm.EndWrite()
	if ltm.GetPendingOperationsCount() != 0 {
		t.Errorf("Expected 0 pending operations after EndWrite, got %d", ltm.GetPendingOperationsCount())
	}
}

func TestLeaseTransferManager_EndWrite_Underflow(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	// Call EndWrite without BeginWrite - should handle gracefully
	ltm.EndWrite()
	if ltm.GetPendingOperationsCount() != 0 {
		t.Errorf("Expected 0 pending operations after underflow, got %d", ltm.GetPendingOperationsCount())
	}
}

func TestLeaseTransferManager_ConcurrentWrites(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	numGoroutines := 100
	var wg sync.WaitGroup
	var successCount int32
	
	// Start many concurrent writes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ltm.BeginWrite(123); err == nil {
				atomic.AddInt32(&successCount, 1)
				ltm.EndWrite()
			}
		}()
	}
	
	wg.Wait()
	
	// All writes should succeed when not quiescing
	if int(successCount) != numGoroutines {
		t.Errorf("Expected %d successful writes, got %d", numGoroutines, successCount)
	}
	
	if ltm.GetPendingOperationsCount() != 0 {
		t.Errorf("Expected 0 pending operations after all writes complete, got %d", ltm.GetPendingOperationsCount())
	}
}

func TestLeaseTransferManager_ConcurrentWritesDuringQuiescing(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	numGoroutines := 50
	var wg sync.WaitGroup
	var successCount int32
	
	// Start some writes, then enable quiescing mode
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			
			// Half the goroutines start before quiescing
			if id < numGoroutines/2 {
				if err := ltm.BeginWrite(123); err == nil {
					atomic.AddInt32(&successCount, 1)
					time.Sleep(10 * time.Millisecond) // Simulate work
					ltm.EndWrite()
				}
			} else {
				// Give time for quiescing to be enabled
				time.Sleep(5 * time.Millisecond)
				if err := ltm.BeginWrite(123); err == nil {
					atomic.AddInt32(&successCount, 1)
					ltm.EndWrite()
				}
			}
		}(i)
	}
	
	// Enable quiescing after a short delay
	time.Sleep(2 * time.Millisecond)
	atomic.StoreInt32(&ltm.quiescing, 1)
	
	wg.Wait()
	
	// Some writes should succeed (those that started before quiescing)
	// Exact count depends on timing, but should be less than total
	t.Logf("Successful writes during quiescing test: %d/%d", successCount, numGoroutines)
	
	if ltm.GetPendingOperationsCount() != 0 {
		t.Errorf("Expected 0 pending operations after test, got %d", ltm.GetPendingOperationsCount())
	}
}

func TestLeaseTransferManager_PrepareLeaseTransfer_Success(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	ltm.SetQuiesceTimeout(100 * time.Millisecond)
	
	// Start a write operation
	ltm.BeginWrite(123)
	
	// Prepare transfer in goroutine
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- ltm.PrepareLeaseTransfer(ctx)
	}()
	
	// End the write after a short delay
	time.Sleep(50 * time.Millisecond)
	ltm.EndWrite()
	
	// Wait for preparation to complete
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("PrepareLeaseTransfer failed: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("PrepareLeaseTransfer timed out")
	}
	
	if !ltm.IsQuiescing() {
		t.Error("Expected to be in quiescing mode after preparation")
	}
}

func TestLeaseTransferManager_PrepareLeaseTransfer_Timeout(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	ltm.SetQuiesceTimeout(50 * time.Millisecond)
	
	// Start a write operation that won't end
	ltm.BeginWrite(123)
	
	ctx := context.Background()
	err := ltm.PrepareLeaseTransfer(ctx)
	
	if err == nil {
		t.Error("Expected timeout error")
	}
	
	if ltm.IsQuiescing() {
		t.Error("Should not be quiescing after timeout")
	}
	
	// Clean up
	ltm.EndWrite()
}

func TestLeaseTransferManager_PrepareLeaseTransfer_NotLeaseHolder(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: false, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	ctx := context.Background()
	err := ltm.PrepareLeaseTransfer(ctx)
	
	if err == nil {
		t.Error("Expected error when not lease holder")
	}
}

func TestLeaseTransferManager_PrepareLeaseTransfer_NilLeaseManager(t *testing.T) {
	ltm := NewLeaseTransferManager(nil)
	
	ctx := context.Background()
	err := ltm.PrepareLeaseTransfer(ctx)
	
	if err == nil {
		t.Error("Expected error with nil lease manager")
	}
}

func TestLeaseTransferManager_CompleteLeaseTransfer_Success(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	// Set quiescing mode first
	atomic.StoreInt32(&ltm.quiescing, 1)
	
	ctx := context.Background()
	err := ltm.CompleteLeaseTransfer(ctx)
	
	if err != nil {
		t.Errorf("CompleteLeaseTransfer failed: %v", err)
	}
	
	if !mockLM.WasReleaseCalled() {
		t.Error("Expected ReleaseLease to be called")
	}
	
	if ltm.IsQuiescing() {
		t.Error("Should not be quiescing after completion")
	}
}

func TestLeaseTransferManager_CompleteLeaseTransfer_ReleaseFails(t *testing.T) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	mockLM.SetReleaseFail(true)
	ltm := NewLeaseTransferManager(mockLM)
	
	// Set quiescing mode first
	atomic.StoreInt32(&ltm.quiescing, 1)
	
	ctx := context.Background()
	err := ltm.CompleteLeaseTransfer(ctx)
	
	if err == nil {
		t.Error("Expected error when ReleaseLease fails")
	}
	
	if ltm.IsQuiescing() {
		t.Error("Should not be quiescing even after release failure")
	}
}

func TestHTTPManagerWithTransfer_ValidateWriteWithQuiescing(t *testing.T) {
	mockHTTP := &HTTPManager{} // Minimal mock
	httpManager := &HTTPManagerWithTransfer{
		HTTPManager:     mockHTTP,
		transferManager: NewLeaseTransferManager(&MockLeaseManager{isHolder: true, fenceToken: 123}),
	}
	
	// Test valid fence token
	err := httpManager.ValidateWriteWithQuiescing(123)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	
	// Test invalid fence token
	err = httpManager.ValidateWriteWithQuiescing(0)
	if err == nil {
		t.Error("Expected error for invalid fence token")
	}
	
	// Test nil transfer manager
	httpManager.transferManager = nil
	err = httpManager.ValidateWriteWithQuiescing(123)
	if err == nil {
		t.Error("Expected error for nil transfer manager")
	}
}

func TestSmartLeaseHolderSelection_SelectBestLeaseHolder(t *testing.T) {
	sls := &SmartLeaseHolderSelection{
		nodes: map[string]*NodeMetrics{
			"node1": {
				NodeID:            "node1",
				LastSeen:          time.Now(),
				WriteLatency:      50 * time.Millisecond,
				ReplicationLag:    100 * time.Millisecond,
				ActiveConnections: 5,
				Region:            "us-east",
				LoadAverage:       1.0,
			},
			"node2": {
				NodeID:            "node2",
				LastSeen:          time.Now(),
				WriteLatency:      200 * time.Millisecond,
				ReplicationLag:    2 * time.Second,
				ActiveConnections: 10,
				Region:            "us-west",
				LoadAverage:       3.0,
			},
			"node3": {
				NodeID:            "node3",
				LastSeen:          time.Now().Add(-60 * time.Second), // Stale
				WriteLatency:      10 * time.Millisecond,
				ReplicationLag:    50 * time.Millisecond,
				ActiveConnections: 1,
				Region:            "us-east",
				LoadAverage:       0.5,
			},
		},
	}
	
	bestNode, err := sls.SelectBestLeaseHolder("us-east")
	if err != nil {
		t.Errorf("SelectBestLeaseHolder failed: %v", err)
	}
	
	// Should select node1 (same region, good metrics, not stale)
	if bestNode != "node1" {
		t.Errorf("Expected node1, got %s", bestNode)
	}
}

func TestSmartLeaseHolderSelection_NoAvailableNodes(t *testing.T) {
	sls := &SmartLeaseHolderSelection{
		nodes: map[string]*NodeMetrics{},
	}
	
	_, err := sls.SelectBestLeaseHolder("us-east")
	if err == nil {
		t.Error("Expected error when no nodes available")
	}
}

func BenchmarkLeaseTransferManager_BeginEndWrite(b *testing.B) {
	mockLM := &MockLeaseManager{isHolder: true, fenceToken: 123}
	ltm := NewLeaseTransferManager(mockLM)
	
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := ltm.BeginWrite(123); err == nil {
				ltm.EndWrite()
			}
		}
	})
}

func BenchmarkSmartLeaseHolderSelection_Select(b *testing.B) {
	sls := &SmartLeaseHolderSelection{
		nodes: make(map[string]*NodeMetrics),
	}
	
	// Add many nodes
	for i := 0; i < 100; i++ {
		nodeID := fmt.Sprintf("node%d", i)
		sls.nodes[nodeID] = &NodeMetrics{
			NodeID:            nodeID,
			LastSeen:          time.Now(),
			WriteLatency:      time.Duration(i) * time.Millisecond,
			ReplicationLag:    time.Duration(i*10) * time.Millisecond,
			ActiveConnections: i % 20,
			Region:            "us-east",
			LoadAverage:       float64(i % 5),
		}
	}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sls.SelectBestLeaseHolder("us-east")
	}
}