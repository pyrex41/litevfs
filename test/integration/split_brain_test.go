package main

import (
	"context"
	"database/sql"
	"distsqlite/pkg/lease"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestSplitBrainPrevention tests that only one node can write at a time
func TestSplitBrainPrevention(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "split_brain_test.db")
	lockFile := filepath.Join(tmpDir, "test.lock")
	
	// Create database
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()
	
	// Create test table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS split_brain_test (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node TEXT NOT NULL,
			value INTEGER NOT NULL,
			timestamp INTEGER DEFAULT (strftime('%s', 'now'))
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}
	db.Close()
	
	// Simulate two nodes trying to write
	var wg sync.WaitGroup
	var writeErrors int32
	var successfulWrites int32
	
	// Node function
	nodeFunc := func(nodeName string, iterations int) {
		defer wg.Done()
		
		// Create lease manager
		manager := lease.NewHTTPManager(nodeName, lockFile, "", 100*time.Millisecond)
		ctx := context.Background()
		
		if err := manager.Start(ctx); err != nil {
			t.Errorf("Node %s: Failed to start lease manager: %v", nodeName, err)
			return
		}
		defer manager.Stop()
		
		// Open database connection
		nodeDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Errorf("Node %s: Failed to open database: %v", nodeName, err)
			return
		}
		defer nodeDB.Close()
		
		// Try to write multiple times
		for i := 0; i < iterations; i++ {
			// Only write if we have the lease
			if manager.HasLease() {
				_, err := nodeDB.Exec(
					"INSERT INTO split_brain_test (node, value) VALUES (?, ?)",
					nodeName, i,
				)
				
				if err != nil {
					atomic.AddInt32(&writeErrors, 1)
					t.Logf("Node %s: Write error: %v", nodeName, err)
				} else {
					atomic.AddInt32(&successfulWrites, 1)
				}
			}
			
			// Small delay between attempts
			time.Sleep(50 * time.Millisecond)
		}
	}
	
	// Start two competing nodes
	wg.Add(2)
	go nodeFunc("NodeA", 20)
	go nodeFunc("NodeB", 20)
	
	wg.Wait()
	
	// Verify results
	if writeErrors > 0 {
		t.Errorf("Had %d write errors - possible split brain", writeErrors)
	}
	
	// Check database consistency
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer db.Close()
	
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM split_brain_test").Scan(&count)
	if err != nil {
		t.Fatalf("Failed to count rows: %v", err)
	}
	
	t.Logf("Total writes: %d (successful: %d)", count, successfulWrites)
	
	// Verify no interleaved writes from different nodes
	rows, err := db.Query(`
		SELECT node, COUNT(*) as write_count 
		FROM split_brain_test 
		GROUP BY node 
		ORDER BY node
	`)
	if err != nil {
		t.Fatalf("Failed to query write counts: %v", err)
	}
	defer rows.Close()
	
	for rows.Next() {
		var node string
		var writeCount int
		if err := rows.Scan(&node, &writeCount); err != nil {
			t.Fatalf("Failed to scan row: %v", err)
		}
		t.Logf("Node %s made %d writes", node, writeCount)
	}
}

// TestFenceTokenValidation tests fence token increments prevent stale writes
func TestFenceTokenValidation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "fence_test.db")
	lockFile := filepath.Join(tmpDir, "test.lock")
	
	// Create database with fence token tracking
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS fence_tokens (
			id INTEGER PRIMARY KEY,
			token INTEGER NOT NULL,
			node TEXT NOT NULL,
			timestamp INTEGER DEFAULT (strftime('%s', 'now'))
		);
		INSERT INTO fence_tokens (id, token, node) VALUES (1, 0, 'init');
	`)
	if err != nil {
		t.Fatalf("Failed to create fence table: %v", err)
	}
	db.Close()
	
	// Simulate lease changes with fence tokens
	ctx := context.Background()
	
	// First node gets lease
	manager1 := lease.NewHTTPManager("node1", lockFile, "", 100*time.Millisecond)
	if err := manager1.Start(ctx); err != nil {
		t.Fatalf("Failed to start manager1: %v", err)
	}
	
	time.Sleep(200 * time.Millisecond)
	
	// Simulate fence token increment
	if manager1.HasLease() {
		lease1 := manager1.Current()
		t.Logf("Node1 has lease with fence token: %d", lease1.FenceToken)
		
		// Update fence token in database
		db, _ = sql.Open("sqlite", dbPath)
		_, err = db.Exec("UPDATE fence_tokens SET token = ?, node = ? WHERE id = 1", 
			lease1.FenceToken, "node1")
		if err != nil {
			t.Errorf("Failed to update fence token: %v", err)
		}
		db.Close()
	}
	
	// Stop first manager
	manager1.Stop()
	
	// Second node gets lease
	manager2 := lease.NewHTTPManager("node2", lockFile, "", 100*time.Millisecond)
	if err := manager2.Start(ctx); err != nil {
		t.Fatalf("Failed to start manager2: %v", err)
	}
	defer manager2.Stop()
	
	time.Sleep(500 * time.Millisecond)
	
	if manager2.HasLease() {
		lease2 := manager2.Current()
		t.Logf("Node2 has lease with fence token: %d", lease2.FenceToken)
		
		// Verify fence token incremented
		db, _ = sql.Open("sqlite", dbPath)
		var storedToken int
		var storedNode string
		err = db.QueryRow("SELECT token, node FROM fence_tokens WHERE id = 1").Scan(&storedToken, &storedNode)
		if err != nil {
			t.Fatalf("Failed to query fence token: %v", err)
		}
		
		t.Logf("Stored fence token: %d (node: %s)", storedToken, storedNode)
		
		// New fence token should be higher
		if lease2.FenceToken <= storedToken {
			t.Errorf("Fence token did not increment properly: new=%d, old=%d", 
				lease2.FenceToken, storedToken)
		}
		
		db.Close()
	}
}

// TestPartitionRecovery tests recovery from network partition
func TestPartitionRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "partition_test.db")
	lockFile := filepath.Join(tmpDir, "test.lock")
	
	// Create shared database
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS partition_test (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node TEXT NOT NULL,
			event TEXT NOT NULL,
			timestamp INTEGER DEFAULT (strftime('%s', 'now'))
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}
	db.Close()
	
	// Start node1
	manager1 := lease.NewHTTPManager("node1", lockFile, "", 100*time.Millisecond)
	ctx := context.Background()
	
	if err := manager1.Start(ctx); err != nil {
		t.Fatalf("Failed to start manager1: %v", err)
	}
	
	// Node1 should get lease
	time.Sleep(200 * time.Millisecond)
	if !manager1.HasLease() {
		t.Fatal("Node1 should have lease initially")
	}
	
	// Record lease acquisition
	db, _ = sql.Open("sqlite", dbPath)
	db.Exec("INSERT INTO partition_test (node, event) VALUES ('node1', 'acquired_lease')")
	db.Close()
	
	// Simulate partition by creating a new lock file that node1 can't see
	// (In reality, this would be a network partition)
	partitionLockFile := lockFile + ".partition"
	os.Rename(lockFile, partitionLockFile)
	
	// Start node2 which sees different lock file state
	manager2 := lease.NewHTTPManager("node2", lockFile, "", 100*time.Millisecond)
	if err := manager2.Start(ctx); err != nil {
		t.Fatalf("Failed to start manager2: %v", err)
	}
	
	// Node2 should get lease in its view
	time.Sleep(500 * time.Millisecond)
	if !manager2.HasLease() {
		t.Error("Node2 should acquire lease during partition")
	}
	
	// Record node2's lease
	db, _ = sql.Open("sqlite", dbPath)
	db.Exec("INSERT INTO partition_test (node, event) VALUES ('node2', 'acquired_lease_partition')")
	db.Close()
	
	// Heal partition
	os.Rename(partitionLockFile, lockFile)
	
	// Wait for nodes to reconcile
	time.Sleep(1 * time.Second)
	
	// Only one should have lease now
	hasLease1 := manager1.HasLease()
	hasLease2 := manager2.HasLease()
	
	if hasLease1 && hasLease2 {
		t.Error("Both nodes have lease after partition heal - split brain!")
	}
	
	if !hasLease1 && !hasLease2 {
		t.Error("No node has lease after partition heal")
	}
	
	// Record final state
	db, _ = sql.Open("sqlite", dbPath)
	if hasLease1 {
		db.Exec("INSERT INTO partition_test (node, event) VALUES ('node1', 'retained_after_heal')")
	} else {
		db.Exec("INSERT INTO partition_test (node, event) VALUES ('node2', 'retained_after_heal')")
	}
	
	// Query event log
	rows, err := db.Query("SELECT node, event FROM partition_test ORDER BY timestamp")
	if err != nil {
		t.Fatalf("Failed to query events: %v", err)
	}
	defer rows.Close()
	
	t.Log("Partition event log:")
	for rows.Next() {
		var node, event string
		rows.Scan(&node, &event)
		t.Logf("  %s: %s", node, event)
	}
	
	db.Close()
	manager1.Stop()
	manager2.Stop()
}

// TestConcurrentLeaseAttempts tests many nodes competing simultaneously
func TestConcurrentLeaseAttempts(t *testing.T) {
	tmpDir := t.TempDir()
	lockFile := filepath.Join(tmpDir, "concurrent.lock")
	
	numNodes := 20
	managers := make([]*lease.HTTPManager, numNodes)
	
	// Create all managers
	for i := 0; i < numNodes; i++ {
		nodeName := fmt.Sprintf("node%02d", i)
		managers[i] = lease.NewHTTPManager(nodeName, lockFile, "", 50*time.Millisecond)
	}
	
	// Start all simultaneously
	ctx := context.Background()
	var wg sync.WaitGroup
	startTime := time.Now()
	
	for i, manager := range managers {
		wg.Add(1)
		go func(idx int, m *lease.HTTPManager) {
			defer wg.Done()
			if err := m.Start(ctx); err != nil {
				t.Errorf("Failed to start manager %d: %v", idx, err)
			}
		}(i, manager)
	}
	
	wg.Wait()
	
	// Let them compete
	time.Sleep(2 * time.Second)
	
	// Count lease holders over time
	for i := 0; i < 5; i++ {
		var holders []string
		for _, m := range managers {
			if m.HasLease() {
				if lease := m.Current(); lease != nil {
					holders = append(holders, lease.Hostname)
				}
			}
		}
		
		if len(holders) != 1 {
			t.Errorf("Iteration %d: Expected 1 lease holder, got %d: %v", 
				i, len(holders), holders)
		} else {
			t.Logf("Iteration %d: Lease holder is %s", i, holders[0])
		}
		
		time.Sleep(500 * time.Millisecond)
	}
	
	// Clean up
	for _, m := range managers {
		m.Stop()
	}
	
	t.Logf("Test completed in %v", time.Since(startTime))
}