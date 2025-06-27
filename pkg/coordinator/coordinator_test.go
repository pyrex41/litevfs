package coordinator

import (
	"testing"
	"time"
)

func TestNewCoordinator(t *testing.T) {
	coord := NewCoordinator(9090, "/tmp/test.db")
	
	if coord == nil {
		t.Fatal("NewCoordinator() returned nil")
	}
	
	if coord.leaseServerPort != 9090 {
		t.Errorf("Expected lease server port 9090, got %d", coord.leaseServerPort)
	}
}

func TestNodeStruct(t *testing.T) {
	node := &Node{
		Hostname:     "test-node",
		AdvertiseURL: "http://test-node:8080",
		LastSeen:     time.Now(),
		Status:       "active",
		Databases:    make(map[string]string),
	}
	
	if node.Hostname != "test-node" {
		t.Errorf("Expected hostname 'test-node', got %s", node.Hostname)
	}
	
	if node.Status != "active" {
		t.Errorf("Expected status 'active', got %s", node.Status)
	}
	
	if node.Databases == nil {
		t.Error("Expected databases map to be initialized")
	}
}

func TestLeaseEvent(t *testing.T) {
	event := LeaseEvent{
		Event:    "acquired",
		Hostname: "node1",
		Time:     time.Now(),
	}
	
	if event.Event != "acquired" {
		t.Errorf("Expected event 'acquired', got %s", event.Event)
	}
	
	if event.Hostname != "node1" {
		t.Errorf("Expected hostname 'node1', got %s", event.Hostname)
	}
}

func TestReplicationRequest(t *testing.T) {
	req := &ReplicationRequest{
		ID:         "repl_123",
		SourceNode: "node1",
		TargetNode: "node2",
		Database:   "/data/test.db",
		Strategy:   "rsync",
		Priority:   1,
		CreatedAt:  time.Now(),
		Status:     "pending",
	}
	
	if req.Strategy != "rsync" {
		t.Errorf("Expected strategy 'rsync', got %s", req.Strategy)
	}
	
	if req.Status != "pending" {
		t.Errorf("Expected status 'pending', got %s", req.Status)
	}
	
	if req.Priority != 1 {
		t.Errorf("Expected priority 1, got %d", req.Priority)
	}
}

func TestCoordinatorLifecycle(t *testing.T) {
	coord := NewCoordinator(9091, "/tmp/test2.db")
	
	// Test that we can create and stop coordinator
	if coord == nil {
		t.Fatal("Failed to create coordinator")
	}
	
	// Test stop
	err := coord.Stop()
	if err != nil {
		t.Errorf("Stop() returned error: %v", err)
	}
}

func TestCoordinatorDataStructures(t *testing.T) {
	coord := NewCoordinator(9092, "/tmp/test3.db")
	defer coord.Stop()
	
	// Test that internal data structures are initialized
	if coord.nodes == nil {
		t.Error("Expected nodes map to be initialized")
	}
	
	if coord.leaseEvents == nil {
		t.Error("Expected lease events slice to be initialized")
	}
	
	if coord.replicationJobs == nil {
		t.Error("Expected replication jobs map to be initialized")
	}
	
	if coord.httpRsyncServers == nil {
		t.Error("Expected HTTP rsync servers map  to be initialized")
	}
}