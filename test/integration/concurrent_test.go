package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestConcurrentWriters tests multiple writers with proper serialization
func TestConcurrentWriters(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "concurrent.db")
	
	// Create database with WAL mode
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	
	// Enable WAL mode for concurrent readers
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		t.Fatalf("Failed to enable WAL: %v", err)
	}
	
	// Create test table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS concurrent_test (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			writer_id INTEGER NOT NULL,
			sequence INTEGER NOT NULL,
			value TEXT NOT NULL,
			timestamp INTEGER DEFAULT (strftime('%s', 'now'))
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}
	db.Close()
	
	// Test parameters
	numWriters := 5
	writesPerWriter := 100
	
	// Tracking
	var totalWrites int32
	var writeErrors int32
	var contentionCount int32
	
	// Writer function
	writerFunc := func(writerID int, wg *sync.WaitGroup) {
		defer wg.Done()
		
		// Each writer gets its own connection
		writerDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Errorf("Writer %d: Failed to open database: %v", writerID, err)
			return
		}
		defer writerDB.Close()
		
		// Set busy timeout to handle contention
		_, err = writerDB.Exec("PRAGMA busy_timeout=5000")
		if err != nil {
			t.Errorf("Writer %d: Failed to set busy timeout: %v", writerID, err)
			return
		}
		
		for seq := 0; seq < writesPerWriter; seq++ {
			value := fmt.Sprintf("writer_%d_seq_%d_data_%d", writerID, seq, rand.Int())
			
			// Try to insert with retry logic
			var retries int
			for retries < 3 {
				_, err := writerDB.Exec(
					"INSERT INTO concurrent_test (writer_id, sequence, value) VALUES (?, ?, ?)",
					writerID, seq, value,
				)
				
				if err == nil {
					atomic.AddInt32(&totalWrites, 1)
					break
				} else if err.Error() == "database is locked" {
					atomic.AddInt32(&contentionCount, 1)
					retries++
					time.Sleep(time.Duration(rand.Intn(10)) * time.Millisecond)
				} else {
					atomic.AddInt32(&writeErrors, 1)
					t.Errorf("Writer %d: Unexpected error: %v", writerID, err)
					break
				}
			}
			
			if retries == 3 {
				atomic.AddInt32(&writeErrors, 1)
				t.Errorf("Writer %d: Failed after 3 retries for sequence %d", writerID, seq)
			}
		}
	}
	
	// Start all writers concurrently
	var wg sync.WaitGroup
	startTime := time.Now()
	
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go writerFunc(i, &wg)
	}
	
	wg.Wait()
	elapsed := time.Since(startTime)
	
	// Verify results
	t.Logf("Test completed in %v", elapsed)
	t.Logf("Total writes: %d (expected: %d)", totalWrites, numWriters*writesPerWriter)
	t.Logf("Write errors: %d", writeErrors)
	t.Logf("Contention events: %d", contentionCount)
	
	// Verify data integrity
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer db.Close()
	
	// Check total count
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM concurrent_test").Scan(&count)
	if err != nil {
		t.Fatalf("Failed to count rows: %v", err)
	}
	
	if count != int(totalWrites) {
		t.Errorf("Row count mismatch: expected %d, got %d", totalWrites, count)
	}
	
	// Check sequence integrity for each writer
	for writerID := 0; writerID < numWriters; writerID++ {
		rows, err := db.Query(
			"SELECT sequence FROM concurrent_test WHERE writer_id = ? ORDER BY sequence",
			writerID,
		)
		if err != nil {
			t.Fatalf("Failed to query sequences: %v", err)
		}
		
		var sequences []int
		for rows.Next() {
			var seq int
			if err := rows.Scan(&seq); err != nil {
				t.Fatalf("Failed to scan sequence: %v", err)
			}
			sequences = append(sequences, seq)
		}
		rows.Close()
		
		// Verify no gaps or duplicates
		for i, seq := range sequences {
			if seq != i {
				t.Errorf("Writer %d: Sequence gap or duplicate at position %d, got %d", 
					writerID, i, seq)
				break
			}
		}
		
		t.Logf("Writer %d: %d sequences verified", writerID, len(sequences))
	}
}

// TestReadWriteInteraction tests readers don't block writers in WAL mode
func TestReadWriteInteraction(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "read_write.db")
	
	// Create database
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	
	// Enable WAL mode
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		t.Fatalf("Failed to enable WAL: %v", err)
	}
	
	// Create table with initial data
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS read_write_test (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			value INTEGER NOT NULL,
			updated_at INTEGER DEFAULT (strftime('%s', 'now'))
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}
	
	// Insert initial data
	for i := 0; i < 1000; i++ {
		_, err = db.Exec("INSERT INTO read_write_test (value) VALUES (?)", i)
		if err != nil {
			t.Fatalf("Failed to insert initial data: %v", err)
		}
	}
	db.Close()
	
	// Tracking
	var readCount int32
	var writeCount int32
	var readErrors int32
	var writeErrors int32
	
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	// Long-running reader
	go func() {
		readerDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Errorf("Reader: Failed to open database: %v", err)
			return
		}
		defer readerDB.Close()
		
		for {
			select {
			case <-ctx.Done():
				return
			default:
				// Long-running query
				rows, err := readerDB.Query("SELECT COUNT(*), SUM(value) FROM read_write_test")
				if err != nil {
					atomic.AddInt32(&readErrors, 1)
					continue
				}
				
				if rows.Next() {
					var count, sum int
					rows.Scan(&count, &sum)
					atomic.AddInt32(&readCount, 1)
				}
				rows.Close()
				
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	
	// Concurrent writers
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			
			writerDB, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Errorf("Writer %d: Failed to open database: %v", writerID, err)
				return
			}
			defer writerDB.Close()
			
			for j := 0; j < 50; j++ {
				select {
				case <-ctx.Done():
					return
				default:
					_, err := writerDB.Exec(
						"UPDATE read_write_test SET value = value + 1 WHERE id = ?",
						rand.Intn(1000)+1,
					)
					
					if err != nil {
						atomic.AddInt32(&writeErrors, 1)
					} else {
						atomic.AddInt32(&writeCount, 1)
					}
					
					time.Sleep(20 * time.Millisecond)
				}
			}
		}(i)
	}
	
	wg.Wait()
	cancel() // Stop reader
	
	t.Logf("Reads completed: %d (errors: %d)", readCount, readErrors)
	t.Logf("Writes completed: %d (errors: %d)", writeCount, writeErrors)
	
	// In WAL mode, reads shouldn't block writes
	if writeErrors > 0 {
		t.Errorf("Writers were blocked: %d errors", writeErrors)
	}
}

// TestTransactionIsolation tests ACID properties under concurrent access
func TestTransactionIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "isolation.db")
	
	// Create database
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	
	// Enable WAL mode and set isolation
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		t.Fatalf("Failed to enable WAL: %v", err)
	}
	
	// Create account table for transfer test
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY,
			balance INTEGER NOT NULL CHECK (balance >= 0),
			version INTEGER DEFAULT 0
		);
		INSERT INTO accounts (id, balance) VALUES 
			(1, 1000),
			(2, 1000),
			(3, 1000);
	`)
	if err != nil {
		t.Fatalf("Failed to create accounts: %v", err)
	}
	db.Close()
	
	// Track successful transfers
	var successfulTransfers int32
	var failedTransfers int32
	var isolationViolations int32
	
	// Transfer function
	transfer := func(from, to int, amount int, wg *sync.WaitGroup) {
		defer wg.Done()
		
		txDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Errorf("Failed to open database: %v", err)
			return
		}
		defer txDB.Close()
		
		// Begin transaction
		tx, err := txDB.Begin()
		if err != nil {
			atomic.AddInt32(&failedTransfers, 1)
			return
		}
		
		// Read balances with lock
		var fromBalance, toBalance int
		var fromVersion, toVersion int
		
		err = tx.QueryRow("SELECT balance, version FROM accounts WHERE id = ?", from).
			Scan(&fromBalance, &fromVersion)
		if err != nil {
			tx.Rollback()
			atomic.AddInt32(&failedTransfers, 1)
			return
		}
		
		err = tx.QueryRow("SELECT balance, version FROM accounts WHERE id = ?", to).
			Scan(&toBalance, &toVersion)
		if err != nil {
			tx.Rollback()
			atomic.AddInt32(&failedTransfers, 1)
			return
		}
		
		// Check sufficient balance
		if fromBalance < amount {
			tx.Rollback()
			return // Not enough balance, expected
		}
		
		// Perform transfer
		_, err = tx.Exec(
			"UPDATE accounts SET balance = balance - ?, version = version + 1 WHERE id = ? AND version = ?",
			amount, from, fromVersion,
		)
		if err != nil {
			tx.Rollback()
			atomic.AddInt32(&failedTransfers, 1)
			return
		}
		
		_, err = tx.Exec(
			"UPDATE accounts SET balance = balance + ?, version = version + 1 WHERE id = ? AND version = ?",
			amount, to, toVersion,
		)
		if err != nil {
			tx.Rollback()
			atomic.AddInt32(&failedTransfers, 1)
			return
		}
		
		// Commit
		if err = tx.Commit(); err != nil {
			atomic.AddInt32(&failedTransfers, 1)
		} else {
			atomic.AddInt32(&successfulTransfers, 1)
		}
	}
	
	// Run many concurrent transfers
	var wg sync.WaitGroup
	numTransfers := 100
	
	for i := 0; i < numTransfers; i++ {
		wg.Add(1)
		// Random transfers between accounts
		from := rand.Intn(3) + 1
		to := rand.Intn(3) + 1
		if from == to {
			to = (to % 3) + 1
		}
		amount := rand.Intn(50) + 1
		
		go transfer(from, to, amount, &wg)
	}
	
	wg.Wait()
	
	// Verify integrity
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer db.Close()
	
	// Check total balance (should remain constant)
	var totalBalance int
	err = db.QueryRow("SELECT SUM(balance) FROM accounts").Scan(&totalBalance)
	if err != nil {
		t.Fatalf("Failed to sum balances: %v", err)
	}
	
	expectedTotal := 3000 // Initial total
	if totalBalance != expectedTotal {
		t.Errorf("Balance integrity violated: expected %d, got %d", expectedTotal, totalBalance)
		atomic.AddInt32(&isolationViolations, 1)
	}
	
	// Check no negative balances
	var negativeCount int
	err = db.QueryRow("SELECT COUNT(*) FROM accounts WHERE balance < 0").Scan(&negativeCount)
	if err != nil {
		t.Fatalf("Failed to check negative balances: %v", err)
	}
	
	if negativeCount > 0 {
		t.Errorf("Found %d accounts with negative balance", negativeCount)
		atomic.AddInt32(&isolationViolations, int32(negativeCount))
	}
	
	t.Logf("Successful transfers: %d", successfulTransfers)
	t.Logf("Failed transfers: %d", failedTransfers)
	t.Logf("Isolation violations: %d", isolationViolations)
	
	// Show final balances
	rows, _ := db.Query("SELECT id, balance, version FROM accounts ORDER BY id")
	defer rows.Close()
	
	t.Log("Final account states:")
	for rows.Next() {
		var id, balance, version int
		rows.Scan(&id, &balance, &version)
		t.Logf("  Account %d: balance=%d, version=%d", id, balance, version)
	}
}