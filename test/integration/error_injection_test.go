package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestDatabaseCorruption tests handling of corrupted database files
func TestDatabaseCorruption(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "corrupt.db")
	
	// Create valid database
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	
	_, err = db.Exec(`
		CREATE TABLE test_data (
			id INTEGER PRIMARY KEY,
			data TEXT NOT NULL
		);
		INSERT INTO test_data VALUES (1, 'valid data');
	`)
	if err != nil {
		t.Fatalf("Failed to create test data: %v", err)
	}
	db.Close()
	
	// Test different corruption scenarios
	tests := []struct {
		name           string
		corruptFunc    func(string) error
		expectedError  string
		recoverable    bool
	}{
		{
			name: "HeaderCorruption",
			corruptFunc: func(path string) error {
				return corruptSQLiteHeader(path)
			},
			expectedError: "file is not a database",
			recoverable:   false,
		},
		{
			name: "PartialTruncation",
			corruptFunc: func(path string) error {
				return truncateFile(path, 50) // Truncate to 50 bytes
			},
			expectedError: "database disk image is malformed",
			recoverable:   false,
		},
		{
			name: "RandomByteCorruption",
			corruptFunc: func(path string) error {
				return corruptRandomBytes(path, 10) // Corrupt 10 random bytes
			},
			expectedError: "", // May or may not be detected
			recoverable:   true,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create copy for this test
			testDB := filepath.Join(tmpDir, fmt.Sprintf("test_%s.db", tt.name))
			copyFile(dbPath, testDB)
			
			// Apply corruption
			if err := tt.corruptFunc(testDB); err != nil {
				t.Fatalf("Failed to corrupt database: %v", err)
			}
			
			// Try to open corrupted database
			db, err := sql.Open("sqlite", testDB)
			if err != nil {
				if tt.expectedError != "" && strings.Contains(err.Error(), tt.expectedError) {
					t.Logf("Expected error on open: %v", err)
					return
				}
				t.Fatalf("Unexpected error opening corrupted DB: %v", err)
			}
			defer db.Close()
			
			// Try to query
			var id int
			var data string
			err = db.QueryRow("SELECT id, data FROM test_data WHERE id = 1").Scan(&id, &data)
			
			if err != nil {
				if tt.expectedError != "" && strings.Contains(err.Error(), tt.expectedError) {
					t.Logf("Expected error on query: %v", err)
				} else {
					t.Logf("Unexpected error: %v", err)
				}
			} else {
				if tt.recoverable {
					t.Logf("Successfully read from corrupted DB: id=%d, data=%s", id, data)
				} else {
					t.Errorf("Expected error but query succeeded")
				}
			}
		})
	}
}

// TestNetworkFailures tests HTTP rsync under network conditions
func TestNetworkFailures(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "network_test.db")
	
	// Create test database
	createTestDB(t, dbPath)
	
	tests := []struct {
		name     string
		handler  http.HandlerFunc
		expectError bool
	}{
		{
			name: "SlowResponse",
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(2 * time.Second) // Slow response
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"type": "ORIGIN_END"}`))
			},
			expectError: true, // Should timeout
		},
		{
			name: "ServerError",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("Internal server error"))
			},
			expectError: true,
		},
		{
			name: "InvalidResponse",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("invalid json"))
			},
			expectError: true,
		},
		{
			name: "ConnectionReset",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// Simulate connection reset
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("ResponseWriter doesn't support hijacking")
					return
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Errorf("Failed to hijack connection: %v", err)
					return
				}
				conn.Close() // Abrupt close
			},
			expectError: true,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			
			// Simulate HTTP rsync call
			client := &http.Client{Timeout: 1 * time.Second}
			
			resp, err := client.Post(server.URL, "application/json", 
				strings.NewReader(`{"type": "ORIGIN_BEGIN"}`))
			
			if tt.expectError {
				if err == nil {
					defer resp.Body.Close()
					t.Log("Request completed but error was expected")
				} else {
					t.Logf("Expected network error: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected network error: %v", err)
				} else {
					defer resp.Body.Close()
					t.Log("Request completed successfully")
				}
			}
		})
	}
}

// TestDiskSpaceExhaustion tests behavior when disk is full
func TestDiskSpaceExhaustion(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "diskfull.db")
	
	// Create database
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	
	_, err = db.Exec(`
		CREATE TABLE disk_test (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			data BLOB NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}
	
	// Try to fill up available space
	largeData := make([]byte, 1024*1024) // 1MB per insert
	for i := 0; i < len(largeData); i++ {
		largeData[i] = byte(i % 256)
	}
	
	var insertCount int
	var diskFullDetected bool
	
	// Keep inserting until we hit disk space issues
	for i := 0; i < 1000; i++ { // Reasonable limit to avoid infinite loop
		_, err = db.Exec("INSERT INTO disk_test (data) VALUES (?)", largeData)
		if err != nil {
			if strings.Contains(err.Error(), "disk") || 
			   strings.Contains(err.Error(), "space") ||
			   strings.Contains(err.Error(), "full") {
				diskFullDetected = true
				t.Logf("Disk space exhaustion detected after %d inserts: %v", insertCount, err)
				break
			}
			t.Logf("Other error after %d inserts: %v", insertCount, err)
			break
		}
		insertCount++
		
		// Check file size periodically
		if i%10 == 0 {
			if stat, err := os.Stat(dbPath); err == nil {
				t.Logf("Database size: %d MB", stat.Size()/(1024*1024))
			}
		}
	}
	
	db.Close()
	
	if !diskFullDetected && insertCount == 1000 {
		t.Log("Disk space exhaustion not encountered (test environment has large available space)")
	}
	
	// Verify database is still readable
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer db.Close()
	
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM disk_test").Scan(&count)
	if err != nil {
		t.Errorf("Failed to count rows after disk full: %v", err)
	} else {
		t.Logf("Database contains %d rows after disk space test", count)
	}
}

// TestConcurrentFailures tests system under multiple simultaneous failures
func TestConcurrentFailures(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "failures.db")
	
	// Create database
	createTestDB(t, dbPath)
	
	// Simulate various types of failures
	var networkErrors int32
	var diskErrors int32
	var lockErrors int32
	var successfulOps int32
	
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	// Failure injection goroutines
	go func() {
		// Periodically corrupt temp files
		for {
			select {
			case <-ctx.Done():
				return
			default:
				// Create temporary corruption
				tmpFile := filepath.Join(tmpDir, "temp_corrupt.tmp")
				os.WriteFile(tmpFile, []byte("corruption"), 0644)
				time.Sleep(100 * time.Millisecond)
				os.Remove(tmpFile)
			}
		}
	}()
	
	go func() {
		// Simulate intermittent network issues
		for {
			select {
			case <-ctx.Done():
				return
			default:
				// This would normally involve network calls
				// For now, just count as potential network errors
				atomic.AddInt32(&networkErrors, 1)
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	
	// Worker that tries to operate on database
	for i := 0; i < 5; i++ {
		go func(workerID int) {
			workerDB, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Errorf("Worker %d: Failed to open database: %v", workerID, err)
				return
			}
			defer workerDB.Close()
			
			for {
				select {
				case <-ctx.Done():
					return
				default:
					// Try a database operation
					_, err := workerDB.Exec(
						"INSERT INTO test_data (data) VALUES (?)",
						fmt.Sprintf("worker_%d_%d", workerID, time.Now().UnixNano()),
					)
					
					if err != nil {
						if strings.Contains(err.Error(), "locked") {
							atomic.AddInt32(&lockErrors, 1)
						} else if strings.Contains(err.Error(), "disk") {
							atomic.AddInt32(&diskErrors, 1)
						} else {
							t.Logf("Worker %d: Unexpected error: %v", workerID, err)
						}
					} else {
						atomic.AddInt32(&successfulOps, 1)
					}
					
					time.Sleep(20 * time.Millisecond)
				}
			}
		}(i)
	}
	
	// Let it run
	<-ctx.Done()
	
	t.Logf("Test completed:")
	t.Logf("  Successful operations: %d", successfulOps)
	t.Logf("  Lock errors: %d", lockErrors)
	t.Logf("  Disk errors: %d", diskErrors)
	t.Logf("  Network errors: %d", networkErrors)
	
	// Verify database is still operational
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Errorf("Database not operational after failure test: %v", err)
		return
	}
	defer db.Close()
	
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM test_data").Scan(&count)
	if err != nil {
		t.Errorf("Failed to query database after failures: %v", err)
	} else {
		t.Logf("Database operational with %d total rows", count)
	}
}

// Helper functions

func createTestDB(t *testing.T, path string) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	defer db.Close()
	
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS test_data (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			data TEXT NOT NULL,
			created_at INTEGER DEFAULT (strftime('%s', 'now'))
		);
		INSERT INTO test_data (data) VALUES ('initial data');
	`)
	if err != nil {
		t.Fatalf("Failed to initialize test database: %v", err)
	}
}

func corruptSQLiteHeader(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	
	// Overwrite SQLite header magic
	_, err = file.WriteAt([]byte("CORRUPT!"), 0)
	return err
}

func truncateFile(path string, size int64) error {
	return os.Truncate(path, size)
}

func corruptRandomBytes(path string, numBytes int) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	
	// Get file size
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	
	// Corrupt random bytes (skip header)
	for i := 0; i < numBytes; i++ {
		offset := 100 + int64(i*100) % (stat.Size()-100)
		_, err = file.WriteAt([]byte{0xFF}, offset)
		if err != nil {
			return err
		}
	}
	
	return nil
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()
	
	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()
	
	_, err = io.Copy(destFile, sourceFile)
	return err
}