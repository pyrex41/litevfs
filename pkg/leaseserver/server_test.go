package leaseserver

import (
	"testing"
	"time"
)

func TestNewLeaseServer(t *testing.T) {
	server := NewLeaseServer(8080, "/tmp/test.db")
	if server == nil {
		t.Fatal("NewLeaseServer() returned nil")
	}
	
	if server.port != 8080 {
		t.Errorf("Expected port 8080, got %d", server.port)
	}
	
	if server.dbPath != "/tmp/test.db" {
		t.Errorf("Expected dbPath '/tmp/test.db', got %s", server.dbPath)
	}
	
	if server.leaseTimeout != 30*time.Second {
		t.Errorf("Expected leaseTimeout 30s, got %v", server.leaseTimeout)
	}
}

func TestLeaseRequest(t *testing.T) {
	req := LeaseRequest{
		NodeID: "test-node",
		TTL:    60,
	}
	
	if req.NodeID != "test-node" {
		t.Errorf("Expected NodeID 'test-node', got %s", req.NodeID)
	}
	
	if req.TTL != 60 {
		t.Errorf("Expected TTL 60, got %d", req.TTL)
	}
}

func TestLeaseResponse(t *testing.T) {
	lease := &Lease{
		NodeID:     "node1",
		FenceToken: 123,
		ExpiresAt:  time.Now().Add(30 * time.Second),
		IssuedAt:   time.Now(),
	}
	
	resp := LeaseResponse{
		Success:    true,
		Lease:      lease,
		FenceToken: lease.FenceToken,
	}
	
	if !resp.Success {
		t.Error("Expected response to be successful")
	}
	
	if resp.FenceToken != 123 {
		t.Errorf("Expected fence token 123, got %d", resp.FenceToken)
	}
	
	if resp.Lease.NodeID != "node1" {
		t.Errorf("Expected lease NodeID 'node1', got %s", resp.Lease.NodeID)
	}
}

func TestLeaseExpiration(t *testing.T) {
	now := time.Now()
	
	// Test expired lease
	expiredLease := &Lease{
		NodeID:     "node1",
		FenceToken: 1,
		ExpiresAt:  now.Add(-1 * time.Second), // Expired
		IssuedAt:   now.Add(-31 * time.Second),
	}
	
	isExpired := expiredLease.ExpiresAt.Before(time.Now())
	if !isExpired {
		t.Error("Expected lease to be expired")
	}
	
	// Test valid lease
	validLease := &Lease{
		NodeID:     "node2",
		FenceToken: 2,
		ExpiresAt:  now.Add(30 * time.Second), // Valid
		IssuedAt:   now,
	}
	
	isValid := validLease.ExpiresAt.After(time.Now())
	if !isValid {
		t.Error("Expected lease to be valid")
	}
}