package unit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Mock lease structure for testing
type MockLease struct {
	Hostname   string `json:"hostname"`
	FenceToken int64  `json:"fence_token"`
	Timestamp  int64  `json:"timestamp"`
}

// TestFileBasedLeaseAcquisition tests basic file-based lease coordination
func TestFileBasedLeaseAcquisition(t *testing.T) {
	tmpDir := t.TempDir()
	lockFile := filepath.Join(tmpDir, "test.lock")
	
	// Simulate acquiring a lease
	lease := &MockLease{
		Hostname:   "node1",
		FenceToken: 1,
		Timestamp:  time.Now().Unix(),
	}
	
	leaseData, err := json.Marshal(lease)
	if err != nil {
		t.Fatalf("Failed to marshal lease: %v", err)
	}
	
	// Write lease atomically (simulate file-based coordination)
	tempFile := lockFile + ".tmp"
	err = os.WriteFile(tempFile, leaseData, 0644)
	if err != nil {
		t.Fatalf("Failed to write temp lease: %v", err)
	}
	
	err = os.Rename(tempFile, lockFile)
	if err != nil {
		t.Fatalf("Failed to rename lease file: %v", err)
	}
	
	// Read back and verify
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("Failed to read lease: %v", err)
	}
	
	var readLease MockLease
	err = json.Unmarshal(data, &readLease)
	if err != nil {
		t.Fatalf("Failed to unmarshal lease: %v", err)
	}
	
	if readLease.Hostname != "node1" {
		t.Errorf("Expected hostname 'node1', got %s", readLease.Hostname)
	}
	
	if readLease.FenceToken != 1 {
		t.Errorf("Expected fence token 1, got %d", readLease.FenceToken)
	}
	
	t.Logf("Successfully acquired lease: %+v", readLease)
}

// TestLeaseCompetition tests multiple processes competing for a lease
func TestLeaseCompetition(t *testing.T) {
	tmpDir := t.TempDir()
	lockFile := filepath.Join(tmpDir, "competition.lock")
	
	numNodes := 10 // Increase contention
	var successCount int32
	var wg sync.WaitGroup
	
	// Simulate multiple nodes trying to acquire lease SIMULTANEOUSLY
	for i := 0; i < numNodes; i++ {
		wg.Add(1)
		go func(nodeID int) {
			defer wg.Done()
			
			hostname := fmt.Sprintf("node%d", nodeID)
			lease := &MockLease{
				Hostname:   hostname,
				FenceToken: int64(nodeID + 1),
				Timestamp:  time.Now().Unix(),
			}
			
			leaseData, _ := json.Marshal(lease)
			tempFile := fmt.Sprintf("%s.tmp.%s", lockFile, hostname)
			
			// Try to acquire lease
			err := os.WriteFile(tempFile, leaseData, 0644)
			if err != nil {
				return
			}
			
			// NO DELAY - create maximum race condition
			// All nodes try to claim at exactly the same time
			
			// Atomic rename to claim lease (this is where race condition happens)
			err = os.Rename(tempFile, lockFile)
			if err == nil {
				atomic.AddInt32(&successCount, 1)
				t.Logf("Node %s acquired lease", hostname)
			} else {
				// Expected behavior: only one rename should succeed
				os.Remove(tempFile) // Clean up failed attempt
			}
		}(i)
	}
	
	wg.Wait()
	
	// Only one node should have succeeded despite race condition
	if successCount != 1 {
		t.Errorf("Race condition test FAILED: Expected exactly 1 successful lease acquisition, got %d", successCount)
		t.Error("This indicates the atomic file operation failed to prevent split-brain")
	}
	
	// Verify final lease holder
	if data, err := os.ReadFile(lockFile); err == nil {
		var finalLease MockLease
		if err := json.Unmarshal(data, &finalLease); err == nil {
			t.Logf("Race condition winner: %s (token: %d)", finalLease.Hostname, finalLease.FenceToken)
		}
	} else {
		t.Error("No lease file exists after competition - atomic operations completely failed")
	}
}

// TestLeaseFailover tests lease failover scenario
func TestLeaseFailover(t *testing.T) {
	tmpDir := t.TempDir()
	lockFile := filepath.Join(tmpDir, "failover.lock")
	
	// Initial lease holder
	lease1 := &MockLease{
		Hostname:   "node1",
		FenceToken: 1,
		Timestamp:  time.Now().Unix(),
	}
	
	leaseData, _ := json.Marshal(lease1)
	err := os.WriteFile(lockFile, leaseData, 0644)
	if err != nil {
		t.Fatalf("Failed to write initial lease: %v", err)
	}
	
	// Verify lease exists
	if _, err := os.Stat(lockFile); os.IsNotExist(err) {
		t.Error("Lease file should exist")
	}
	
	// Simulate node1 failure (remove lease)
	time.Sleep(10 * time.Millisecond)
	err = os.Remove(lockFile)
	if err != nil {
		t.Fatalf("Failed to remove lease (simulate failure): %v", err)
	}
	
	// Node2 detects absence and acquires lease
	lease2 := &MockLease{
		Hostname:   "node2",
		FenceToken: 2, // Incremented fence token
		Timestamp:  time.Now().Unix(),
	}
	
	leaseData2, _ := json.Marshal(lease2)
	tempFile := lockFile + ".tmp.node2"
	
	err = os.WriteFile(tempFile, leaseData2, 0644)
	if err != nil {
		t.Fatalf("Failed to write temp lease: %v", err)
	}
	
	err = os.Rename(tempFile, lockFile)
	if err != nil {
		t.Fatalf("Failed to acquire lease: %v", err)
	}
	
	// Verify new lease holder
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("Failed to read new lease: %v", err)
	}
	
	var newLease MockLease
	err = json.Unmarshal(data, &newLease)
	if err != nil {
		t.Fatalf("Failed to unmarshal new lease: %v", err)
	}
	
	if newLease.Hostname != "node2" {
		t.Errorf("Expected new lease holder 'node2', got %s", newLease.Hostname)
	}
	
	if newLease.FenceToken <= lease1.FenceToken {
		t.Errorf("Fence token should have incremented: old=%d, new=%d", 
			lease1.FenceToken, newLease.FenceToken)
	}
	
	t.Logf("Successful failover: %s (token %d) -> %s (token %d)",
		lease1.Hostname, lease1.FenceToken,
		newLease.Hostname, newLease.FenceToken)
}

// TestFenceTokenValidation tests fence token prevents stale writes
func TestFenceTokenValidation(t *testing.T) {
	tmpDir := t.TempDir()
	lockFile := filepath.Join(tmpDir, "fence.lock")
	
	// Current lease with fence token 5
	currentLease := &MockLease{
		Hostname:   "node1",
		FenceToken: 5,
		Timestamp:  time.Now().Unix(),
	}
	
	leaseData, _ := json.Marshal(currentLease)
	err := os.WriteFile(lockFile, leaseData, 0644)
	if err != nil {
		t.Fatalf("Failed to write lease: %v", err)
	}
	
	// Function to validate write operations
	validateWrite := func(writeToken int64) bool {
		data, err := os.ReadFile(lockFile)
		if err != nil {
			return false
		}
		
		var lease MockLease
		if err := json.Unmarshal(data, &lease); err != nil {
			return false
		}
		
		return writeToken >= lease.FenceToken
	}
	
	// Test cases
	tests := []struct {
		writeToken int64
		shouldPass bool
		desc       string
	}{
		{3, false, "stale write (token < current)"},
		{5, true, "current write (token = current)"},
		{7, true, "future write (token > current)"},
	}
	
	for _, tt := range tests {
		result := validateWrite(tt.writeToken)
		if result != tt.shouldPass {
			t.Errorf("%s: expected %v, got %v", tt.desc, tt.shouldPass, result)
		} else {
			t.Logf("%s: ✓", tt.desc)
		}
	}
}

// TestConcurrentLeaseRenewals tests concurrent lease renewal attempts
func TestConcurrentLeaseRenewals(t *testing.T) {
	tmpDir := t.TempDir()
	lockFile := filepath.Join(tmpDir, "renewal.lock")
	
	// Initial lease
	lease := &MockLease{
		Hostname:   "node1",
		FenceToken: 1,
		Timestamp:  time.Now().Unix(),
	}
	
	leaseData, _ := json.Marshal(lease)
	err := os.WriteFile(lockFile, leaseData, 0644)
	if err != nil {
		t.Fatalf("Failed to write initial lease: %v", err)
	}
	
	var renewalCount int32
	var wg sync.WaitGroup
	
	// INTENSE concurrent renewal stress test
	numRenewers := 50 // Many more concurrent renewers
	for i := 0; i < numRenewers; i++ {
		wg.Add(1)
		go func(iteration int) {
			defer wg.Done()
			
			// Try to renew lease
			data, err := os.ReadFile(lockFile)
			if err != nil {
				return
			}
			
			var currentLease MockLease
			if err := json.Unmarshal(data, &currentLease); err != nil {
				return
			}
			
			// Update timestamp (renewal)
			currentLease.Timestamp = time.Now().Unix()
			currentLease.FenceToken++ // Increment on renewal
			
			newData, _ := json.Marshal(currentLease)
			tempFile := fmt.Sprintf("%s.tmp.%d", lockFile, iteration)
			
			// Atomic renewal with NO delay to maximize race condition
			if err := os.WriteFile(tempFile, newData, 0644); err == nil {
				if err := os.Rename(tempFile, lockFile); err == nil {
					atomic.AddInt32(&renewalCount, 1)
				} else {
					os.Remove(tempFile)
				}
			}
		}(i)
	}
	
	wg.Wait()
	
	// At least some renewals should succeed
	if renewalCount == 0 {
		t.Error("No lease renewals succeeded - atomic operations may be broken")
	}
	
	// Check final state
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("Failed to read final lease: %v", err)
	}
	
	var finalLease MockLease
	err = json.Unmarshal(data, &finalLease)
	if err != nil {
		t.Fatalf("Failed to unmarshal final lease: %v", err)
	}
	
	t.Logf("Renewals completed: %d/%d, final fence token: %d", 
		renewalCount, numRenewers, finalLease.FenceToken)
	
	// Fence token should have incremented
	if finalLease.FenceToken <= 1 {
		t.Error("Fence token should have incremented during renewals")
	}
	
	// The key test: even with intense concurrency, the file should be valid
	if finalLease.Hostname != "node1" {
		t.Error("Lease corruption detected - concurrent access broke atomic operations")
	}
}

// TestExtremeRaceCondition tests the worst-case scenario for lease contention
func TestExtremeRaceCondition(t *testing.T) {
	tmpDir := t.TempDir()
	lockFile := filepath.Join(tmpDir, "extreme.lock")
	
	numNodes := 100 // Many competing nodes
	attempts := 1000 // Multiple attempts per node
	var totalAttempts int32
	var successCount int32
	var wg sync.WaitGroup
	
	// Launch many nodes, each making many attempts
	for nodeID := 0; nodeID < numNodes; nodeID++ {
		wg.Add(1)
		go func(nID int) {
			defer wg.Done()
			
			hostname := fmt.Sprintf("node%d", nID)
			
			for attempt := 0; attempt < attempts; attempt++ {
				atomic.AddInt32(&totalAttempts, 1)
				
				lease := &MockLease{
					Hostname:   hostname,
					FenceToken: int64(nID*1000 + attempt),
					Timestamp:  time.Now().Unix(),
				}
				
				leaseData, _ := json.Marshal(lease)
				tempFile := fmt.Sprintf("%s.tmp.%s.%d", lockFile, hostname, attempt)
				
				// Try to acquire lease
				if err := os.WriteFile(tempFile, leaseData, 0644); err == nil {
					// Immediate atomic claim (maximum race condition)
					if err := os.Rename(tempFile, lockFile); err == nil {
						atomic.AddInt32(&successCount, 1)
						// Hold lease briefly then release
						time.Sleep(time.Microsecond)
						os.Remove(lockFile)
					} else {
						os.Remove(tempFile)
					}
				}
			}
		}(nodeID)
	}
	
	wg.Wait()
	
	t.Logf("Extreme race test: %d successes out of %d attempts", successCount, totalAttempts)
	
	// Success rate should be reasonable (not 0, not 100%)
	if successCount == 0 {
		t.Error("No successful acquisitions - atomic operations may be broken")
	}
	
	successRate := float64(successCount) / float64(totalAttempts)
	t.Logf("Success rate: %.2f%%", successRate*100)
	
	// With proper atomic operations, we should see some successes but not chaos
	if successRate > 0.1 { // More than 10% success might indicate lack of proper coordination
		t.Logf("Warning: High success rate (%.2f%%) might indicate insufficient coordination", successRate*100)
	}
}

// BenchmarkLeaseAcquisition benchmarks lease acquisition performance
func BenchmarkLeaseAcquisition(b *testing.B) {
	tmpDir := b.TempDir()
	
	b.ResetTimer()
	
	for i := 0; i < b.N; i++ {
		lockFile := filepath.Join(tmpDir, fmt.Sprintf("bench_%d.lock", i))
		
		lease := &MockLease{
			Hostname:   "bench_node",
			FenceToken: int64(i + 1),
			Timestamp:  time.Now().Unix(),
		}
		
		leaseData, _ := json.Marshal(lease)
		tempFile := lockFile + ".tmp"
		
		// Benchmark the atomic acquisition
		os.WriteFile(tempFile, leaseData, 0644)
		os.Rename(tempFile, lockFile)
	}
}

// BenchmarkFenceTokenValidation benchmarks fence token validation
func BenchmarkFenceTokenValidation(b *testing.B) {
	tmpDir := b.TempDir()
	lockFile := filepath.Join(tmpDir, "fence_bench.lock")
	
	// Setup lease
	lease := &MockLease{
		Hostname:   "bench_node",
		FenceToken: 1000,
		Timestamp:  time.Now().Unix(),
	}
	
	leaseData, _ := json.Marshal(lease)
	os.WriteFile(lockFile, leaseData, 0644)
	
	b.ResetTimer()
	
	for i := 0; i < b.N; i++ {
		// Benchmark validation
		data, _ := os.ReadFile(lockFile)
		var currentLease MockLease
		json.Unmarshal(data, &currentLease)
		
		// Simulate validation
		writeToken := int64(1000 + i%100)
		_ = writeToken >= currentLease.FenceToken
	}
}