package unit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"distsqlite/pkg/leaseserver"
)

// TestCentralizedLeaseBasics tests basic centralized lease operations
func TestCentralizedLeaseBasics(t *testing.T) {
	// Start lease server
	server := leaseserver.NewLeaseServer(8081, "test.db")
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("Server start error: %v", err)
		}
	}()
	defer server.Stop()
	
	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	
	// Create client
	client := leaseserver.NewLeaseClient("http://localhost:8081", "node1")
	ctx := context.Background()
	
	// Test health check
	if err := client.HealthCheck(ctx); err != nil {
		t.Fatalf("Health check failed: %v", err)
	}
	
	// Test lease acquisition
	lease, err := client.AcquireLease(ctx, 30)
	if err != nil {
		t.Fatalf("Failed to acquire lease: %v", err)
	}
	
	if lease.NodeID != "node1" {
		t.Errorf("Expected NodeID 'node1', got %s", lease.NodeID)
	}
	
	if lease.FenceToken <= 0 {
		t.Errorf("Invalid fence token: %d", lease.FenceToken)
	}
	
	// Test lease renewal
	renewedLease, err := client.RenewLease(ctx, 60)
	if err != nil {
		t.Fatalf("Failed to renew lease: %v", err)
	}
	
	if renewedLease.FenceToken != lease.FenceToken {
		t.Errorf("Fence token changed during renewal: %d -> %d", 
			lease.FenceToken, renewedLease.FenceToken)
	}
	
	// Test lease release
	if err := client.ReleaseLease(ctx); err != nil {
		t.Fatalf("Failed to release lease: %v", err)
	}
	
	// Verify no lease holder
	if client.IsLeaseHolder() {
		t.Error("Client still thinks it holds lease after release")
	}
	
	t.Log("✓ Basic centralized lease operations work correctly")
}

// TestCentralizedLeaseCompetition tests multiple nodes competing for centralized lease
func TestCentralizedLeaseCompetition(t *testing.T) {
	// Start lease server
	server := leaseserver.NewLeaseServer(8082, "test.db")
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("Server start error: %v", err)
		}
	}()
	defer server.Stop()
	
	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	
	ctx := context.Background()
	numNodes := 10
	var successCount int32
	var wg sync.WaitGroup
	
	// Create multiple competing clients
	for i := 0; i < numNodes; i++ {
		wg.Add(1)
		go func(nodeID int) {
			defer wg.Done()
			
			client := leaseserver.NewLeaseClient("http://localhost:8082", 
				fmt.Sprintf("node%d", nodeID))
			
			// All nodes try to acquire lease simultaneously
			_, err := client.AcquireLease(ctx, 30)
			if err == nil {
				atomic.AddInt32(&successCount, 1)
				t.Logf("Node%d acquired lease", nodeID)
				
				// Hold lease briefly then release
				time.Sleep(10 * time.Millisecond)
				client.ReleaseLease(ctx)
			}
		}(i)
	}
	
	wg.Wait()
	
	// Only one node should have succeeded
	if successCount != 1 {
		t.Errorf("Expected exactly 1 successful lease acquisition, got %d", successCount)
		t.Error("Centralized lease server failed to prevent split-brain")
	} else {
		t.Logf("✓ Centralized lease prevented split-brain: %d/%d nodes acquired lease", 
			successCount, numNodes)
	}
}

// TestCentralizedLeaseSplitBrainPrevention tests split-brain prevention
func TestCentralizedLeaseSplitBrainPrevention(t *testing.T) {
	// Start lease server
	server := leaseserver.NewLeaseServer(8083, "test.db")
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("Server start error: %v", err)
		}
	}()
	defer server.Stop()
	
	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	
	ctx := context.Background()
	
	// Node1 acquires lease
	client1 := leaseserver.NewLeaseClient("http://localhost:8083", "node1")
	lease1, err := client1.AcquireLease(ctx, 30)
	if err != nil {
		t.Fatalf("Node1 failed to acquire lease: %v", err)
	}
	
	// Node2 tries to acquire while Node1 holds it
	client2 := leaseserver.NewLeaseClient("http://localhost:8083", "node2")
	_, err = client2.AcquireLease(ctx, 30)
	if err == nil {
		t.Error("Node2 should not be able to acquire lease while Node1 holds it")
	}
	
	// Verify only Node1 is lease holder
	if !client1.IsLeaseHolder() {
		t.Error("Node1 should be lease holder")
	}
	
	if client2.IsLeaseHolder() {
		t.Error("Node2 should not be lease holder")
	}
	
	// Test fence token validation
	serverLease, err := client2.GetLeaseStatus(ctx)
	if err != nil {
		t.Fatalf("Failed to get server lease status: %v", err)
	}
	
	if serverLease.FenceToken != lease1.FenceToken {
		t.Errorf("Server fence token mismatch: %d vs %d", 
			serverLease.FenceToken, lease1.FenceToken)
	}
	
	if serverLease.NodeID != "node1" {
		t.Errorf("Server shows wrong lease holder: %s", serverLease.NodeID)
	}
	
	t.Log("✓ Centralized lease successfully prevented split-brain scenario")
}

// TestCentralizedLeaseFailover tests automatic failover scenarios
func TestCentralizedLeaseFailover(t *testing.T) {
	// Start lease server
	server := leaseserver.NewLeaseServer(8084, "test.db")
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("Server start error: %v", err)
		}
	}()
	defer server.Stop()
	
	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	
	ctx := context.Background()
	
	// Node1 acquires lease with short TTL
	client1 := leaseserver.NewLeaseClient("http://localhost:8084", "node1")
	_, err := client1.AcquireLease(ctx, 1) // 1 second TTL
	if err != nil {
		t.Fatalf("Node1 failed to acquire lease: %v", err)
	}
	
	// Wait for lease to expire
	time.Sleep(2 * time.Second)
	
	// Node2 should now be able to acquire expired lease
	client2 := leaseserver.NewLeaseClient("http://localhost:8084", "node2")
	lease2, err := client2.AcquireLease(ctx, 30)
	if err != nil {
		t.Fatalf("Node2 failed to acquire expired lease: %v", err)
	}
	
	if lease2.NodeID != "node2" {
		t.Errorf("Expected Node2 to hold lease, got %s", lease2.NodeID)
	}
	
	// Verify Node1 no longer holds lease
	if client1.IsLeaseHolder() {
		t.Error("Node1 should no longer hold expired lease")
	}
	
	// Verify Node2 is new lease holder
	if !client2.IsLeaseHolder() {
		t.Error("Node2 should be new lease holder")
	}
	
	t.Log("✓ Centralized lease failover worked correctly")
}

// TestCentralizedLeaseRenewal tests automatic lease renewal
func TestCentralizedLeaseRenewal(t *testing.T) {
	// Start lease server
	server := leaseserver.NewLeaseServer(8085, "test.db")
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("Server start error: %v", err)
		}
	}()
	defer server.Stop()
	
	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	// Create client and acquire lease
	client := leaseserver.NewLeaseClient("http://localhost:8085", "node1")
	lease, err := client.AcquireLease(ctx, 2) // 2 second TTL
	if err != nil {
		t.Fatalf("Failed to acquire lease: %v", err)
	}
	
	originalExpiry := lease.ExpiresAt
	
	// Start automatic renewal
	errChan := client.StartLeaseRenewal(ctx, 500*time.Millisecond)
	
	// Wait for renewal to happen
	time.Sleep(1 * time.Second)
	
	// Check that lease was renewed
	currentLease := client.GetCurrentLease()
	if currentLease == nil {
		t.Fatal("Lease should still be held")
	}
	
	if !currentLease.ExpiresAt.After(originalExpiry) {
		t.Error("Lease should have been renewed with later expiry")
	}
	
	// Wait longer to see multiple renewals
	time.Sleep(2 * time.Second)
	
	// Should still hold lease due to automatic renewal
	if !client.IsLeaseHolder() {
		t.Error("Automatic renewal should have kept lease active")
	}
	
	// Check for any renewal errors
	select {
	case err := <-errChan:
		t.Errorf("Lease renewal error: %v", err)
	default:
		// No errors - good
	}
	
	t.Log("✓ Automatic lease renewal working correctly")
}

// TestCentralizedLeaseStressTest tests the centralized system under heavy load
func TestCentralizedLeaseStressTest(t *testing.T) {
	// Start lease server
	server := leaseserver.NewLeaseServer(8086, "test.db")
	go func() {
		if err := server.Start(); err != nil {
			t.Logf("Server start error: %v", err)
		}
	}()
	defer server.Stop()
	
	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	
	ctx := context.Background()
	numNodes := 50
	attempts := 100
	var totalAttempts int32
	var successCount int32
	var wg sync.WaitGroup
	
	startTime := time.Now()
	
	// Many nodes making many attempts
	for nodeID := 0; nodeID < numNodes; nodeID++ {
		wg.Add(1)
		go func(nID int) {
			defer wg.Done()
			
			client := leaseserver.NewLeaseClient("http://localhost:8086", 
				fmt.Sprintf("node%d", nID))
			
			for attempt := 0; attempt < attempts; attempt++ {
				atomic.AddInt32(&totalAttempts, 1)
				
				// Try to acquire lease
				_, err := client.AcquireLease(ctx, 1) // Short TTL
				if err == nil {
					atomic.AddInt32(&successCount, 1)
					// Hold very briefly then release
					time.Sleep(time.Millisecond)
					client.ReleaseLease(ctx)
				}
			}
		}(nodeID)
	}
	
	wg.Wait()
	
	elapsed := time.Since(startTime)
	successRate := float64(successCount) / float64(totalAttempts)
	requestsPerSec := float64(totalAttempts) / elapsed.Seconds()
	
	t.Logf("Stress test results:")
	t.Logf("  Total attempts: %d", totalAttempts)
	t.Logf("  Successful acquisitions: %d", successCount)
	t.Logf("  Success rate: %.2f%%", successRate*100)
	t.Logf("  Requests/sec: %.0f", requestsPerSec)
	t.Logf("  Duration: %v", elapsed)
	
	// Validate that the system maintained consistency
	if successCount == 0 {
		t.Error("No successful acquisitions - system may be broken")
	}
	
	// Under heavy load, success rate should be low (good coordination)
	if successRate > 0.05 { // More than 5%
		t.Logf("Warning: High success rate (%.2f%%) might indicate insufficient coordination", 
			successRate*100)
	}
	
	// Should handle reasonable load
	if requestsPerSec < 1000 {
		t.Logf("Warning: Low throughput (%.0f req/s) - performance may be suboptimal", 
			requestsPerSec)
	}
	
	t.Log("✓ Centralized lease system survived stress test")
}

// BenchmarkCentralizedLeaseAcquisition benchmarks lease acquisition performance
func BenchmarkCentralizedLeaseAcquisition(b *testing.B) {
	// Start lease server
	server := leaseserver.NewLeaseServer(8087, "test.db")
	go func() {
		server.Start()
	}()
	defer server.Stop()
	
	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	
	ctx := context.Background()
	
	b.ResetTimer()
	
	for i := 0; i < b.N; i++ {
		client := leaseserver.NewLeaseClient("http://localhost:8087", 
			fmt.Sprintf("bench_node_%d", i))
		
		// Acquire and immediately release
		_, err := client.AcquireLease(ctx, 1)
		if err == nil {
			client.ReleaseLease(ctx)
		}
	}
}

// BenchmarkCentralizedLeaseRenewal benchmarks lease renewal performance  
func BenchmarkCentralizedLeaseRenewal(b *testing.B) {
	// Start lease server
	server := leaseserver.NewLeaseServer(8088, "test.db")
	go func() {
		server.Start()
	}()
	defer server.Stop()
	
	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	
	ctx := context.Background()
	client := leaseserver.NewLeaseClient("http://localhost:8088", "bench_node")
	
	// Acquire initial lease
	_, err := client.AcquireLease(ctx, 3600) // Long TTL
	if err != nil {
		b.Fatalf("Failed to acquire initial lease: %v", err)
	}
	
	b.ResetTimer()
	
	for i := 0; i < b.N; i++ {
		_, err := client.RenewLease(ctx, 3600)
		if err != nil {
			b.Fatalf("Failed to renew lease: %v", err)
		}
	}
}