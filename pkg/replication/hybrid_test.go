package replication

import (
	"testing"
	"time"
)

func TestNewHybridReplicator(t *testing.T) {
	rep := NewHybridReplicator([]string{"peer1:8080", "peer2:8080"})
	
	if rep == nil {
		t.Fatal("NewHybridReplicator() returned nil")
	}
	
	if len(rep.targets) != 2 {
		t.Errorf("Expected 2 targets, got %d", len(rep.targets))
	}
	
	if rep.targets[0] != "peer1:8080" {
		t.Errorf("Expected first target 'peer1:8080', got %s", rep.targets[0])
	}
	
	if rep.targets[1] != "peer2:8080" {
		t.Errorf("Expected second target 'peer2:8080', got %s", rep.targets[1])
	}
}

func TestReplicationConfiguration(t *testing.T) {
	rep := NewHybridReplicator([]string{"peer:8080"})
	
	// Test initial configuration
	if rep.bulkThreshold != BulkThreshold {
		t.Errorf("Expected bulkThreshold %d, got %d", BulkThreshold, rep.bulkThreshold)
	}
	
	if rep.maxDelay != MaxReplicationDelay {
		t.Errorf("Expected maxDelay %v, got %v", MaxReplicationDelay, rep.maxDelay)
	}
	
	if !rep.preferHTTPRsync {
		t.Error("Expected preferHTTPRsync to be true by default")
	}
}

func TestChangeStruct(t *testing.T) {
	change := Change{
		Database:  "/test/db.sqlite",
		Timestamp: time.Now(),
		Size:      1024,
		Type:      "write",
		Data:      []byte("test data"),
	}
	
	if change.Database != "/test/db.sqlite" {
		t.Errorf("Expected database '/test/db.sqlite', got %s", change.Database)
	}
	
	if change.Size != 1024 {
		t.Errorf("Expected size 1024, got %d", change.Size)
	}
	
	if change.Type != "write" {
		t.Errorf("Expected type 'write', got %s", change.Type)
	}
}

func TestReplicationStrategies(t *testing.T) {
	tests := []struct {
		name     string
		strategy ReplicationStrategy
		expected string
	}{
		{"TxLog strategy", StrategyTxLog, "txlog"},
		{"HTTP Rsync strategy", StrategyHTTPRsync, "http-rsync"},
		{"SSH Rsync strategy", StrategySSHRsync, "ssh-rsync"},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.strategy) != tt.expected {
				t.Errorf("Expected strategy %s, got %s", tt.expected, string(tt.strategy))
			}
		})
	}
}

func TestDatabaseStats(t *testing.T) {
	stats := &DatabaseStats{
		TotalWrites:    10,
		BytesWritten:   2048,
		LastWrite:      time.Now(),
		PendingChanges: make([]Change, 0),
		LastSync:       time.Now().Add(-5 * time.Minute),
	}
	
	if stats.TotalWrites != 10 {
		t.Errorf("Expected TotalWrites 10, got %d", stats.TotalWrites)
	}
	
	if stats.BytesWritten != 2048 {
		t.Errorf("Expected BytesWritten 2048, got %d", stats.BytesWritten)
	}
	
	if stats.PendingChanges == nil {
		t.Error("Expected PendingChanges to be initialized")
	}
}

func TestHybridReplicatorLifecycle(t *testing.T) {
	rep := NewHybridReplicator([]string{"peer:8080"})
	
	// Test start
	err := rep.Start()
	if err != nil {
		t.Errorf("Start() returned error: %v", err)
	}
	
	// Test stop
	err = rep.Stop()
	if err != nil {
		t.Errorf("Stop() returned error: %v", err)
	}
}

func TestRecordChange(t *testing.T) {
	rep := NewHybridReplicator([]string{"peer:8080"})
	defer rep.Stop()
	
	// Test recording a change
	err := rep.RecordChange("/test/db.sqlite", "write", 512, []byte("test"))
	if err != nil {
		t.Errorf("RecordChange() returned error: %v", err)
	}
	
	// Check that the change was recorded
	stats := rep.GetStats()
	if len(stats) != 1 {
		t.Errorf("Expected 1 database in stats, got %d", len(stats))
	}
	
	dbStats, exists := stats["/test/db.sqlite"]
	if !exists {
		t.Error("Expected database stats to exist")
	}
	
	if dbStats.TotalWrites != 1 {
		t.Errorf("Expected 1 total write, got %d", dbStats.TotalWrites)
	}
	
	if dbStats.BytesWritten != 512 {
		t.Errorf("Expected 512 bytes written, got %d", dbStats.BytesWritten)
	}
}

func TestHealthyStatus(t *testing.T) {
	rep := NewHybridReplicator([]string{"peer:8080"})
	defer rep.Stop()
	
	// New replicator should be healthy
	if !rep.IsHealthy() {
		t.Error("New replicator should be healthy")
	}
}

func TestConstants(t *testing.T) {
	if BulkThreshold != 1*1024*1024 {
		t.Errorf("Expected BulkThreshold to be 1MB, got %d", BulkThreshold)
	}
	
	if MaxReplicationDelay != 5*time.Second {
		t.Errorf("Expected MaxReplicationDelay to be 5s, got %v", MaxReplicationDelay)
	}
	
	if TxLogBufferSize != 100 {
		t.Errorf("Expected TxLogBufferSize to be 100, got %d", TxLogBufferSize)
	}
}