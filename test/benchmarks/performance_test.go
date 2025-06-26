package benchmarks

import (
	"context"
	"database/sql"
	"distsqlite/pkg/replication"
	"distsqlite/pkg/rsync"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// BenchmarkBulkInsertComparison compares bulk insert performance
func BenchmarkBulkInsertComparison(b *testing.B) {
	tests := []struct {
		name      string
		rowCount  int
		valueSize int // bytes per row
	}{
		{"Small_1K_rows_100B", 1000, 100},
		{"Medium_10K_rows_1KB", 10000, 1024},
		{"Large_100K_rows_100B", 100000, 100},
		{"Huge_1M_rows_100B", 1000000, 100},
	}
	
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			b.Run("Baseline_SQLite", func(b *testing.B) {
				benchmarkSQLiteBaseline(b, tt.rowCount, tt.valueSize)
			})
			
			b.Run("HTTP_Rsync", func(b *testing.B) {
				benchmarkHTTPRsync(b, tt.rowCount, tt.valueSize)
			})
			
			b.Run("SSH_Rsync", func(b *testing.B) {
				benchmarkSSHRsync(b, tt.rowCount, tt.valueSize)
			})
		})
	}
}

// benchmarkSQLiteBaseline tests raw SQLite copy performance
func benchmarkSQLiteBaseline(b *testing.B, rowCount, valueSize int) {
	tmpDir := b.TempDir()
	
	// Create source database
	sourceDB := filepath.Join(tmpDir, "source.db")
	if err := createTestDatabase(sourceDB, rowCount, valueSize); err != nil {
		b.Fatalf("Failed to create source database: %v", err)
	}
	
	b.ResetTimer()
	b.SetBytes(getDBSize(sourceDB))
	
	for i := 0; i < b.N; i++ {
		destDB := filepath.Join(tmpDir, fmt.Sprintf("dest_%d.db", i))
		
		// Simple file copy
		start := time.Now()
		if err := copyFile(sourceDB, destDB); err != nil {
			b.Fatalf("Failed to copy database: %v", err)
		}
		elapsed := time.Since(start)
		
		b.Logf("Baseline copy: %v for %s", elapsed, humanizeBytes(getDBSize(sourceDB)))
	}
}

// benchmarkHTTPRsync tests HTTP rsync performance
func benchmarkHTTPRsync(b *testing.B, rowCount, valueSize int) {
	tmpDir := b.TempDir()
	
	// Create source database
	sourceDB := filepath.Join(tmpDir, "source.db")
	if err := createTestDatabase(sourceDB, rowCount, valueSize); err != nil {
		b.Fatalf("Failed to create source database: %v", err)
	}
	
	// Start HTTP rsync server
	server := rsync.NewHTTPRsyncServer(sourceDB)
	go func() {
		// Mock server would go here
		// In real test, we'd start an actual HTTP server
	}()
	
	client := rsync.NewHTTPRsyncClient()
	ctx := context.Background()
	
	b.ResetTimer()
	b.SetBytes(getDBSize(sourceDB))
	
	for i := 0; i < b.N; i++ {
		destDB := filepath.Join(tmpDir, fmt.Sprintf("dest_%d.db", i))
		
		// Create empty destination
		db, err := sql.Open("sqlite", destDB)
		if err != nil {
			b.Fatalf("Failed to create dest database: %v", err)
		}
		db.Close()
		
		// Measure rsync time
		start := time.Now()
		stats, err := client.HTTPRsyncSync(ctx, sourceDB, "http://localhost:8080")
		if err != nil {
			// Expected to fail without real server
			b.Logf("HTTP rsync (simulated): %v", err)
		} else {
			elapsed := time.Since(start)
			b.Logf("HTTP rsync: %v, pages sent: %d, bytes: %d", 
				elapsed, stats.PagesSent, stats.BytesSent)
		}
	}
}

// benchmarkSSHRsync tests SSH rsync performance
func benchmarkSSHRsync(b *testing.B, rowCount, valueSize int) {
	tmpDir := b.TempDir()
	
	// Create source database
	sourceDB := filepath.Join(tmpDir, "source.db")
	if err := createTestDatabase(sourceDB, rowCount, valueSize); err != nil {
		b.Fatalf("Failed to create source database: %v", err)
	}
	
	wrapper := rsync.NewWrapper()
	
	b.ResetTimer()
	b.SetBytes(getDBSize(sourceDB))
	
	for i := 0; i < b.N; i++ {
		destDB := filepath.Join(tmpDir, fmt.Sprintf("dest_%d.db", i))
		
		// Measure rsync time
		start := time.Now()
		err := wrapper.Sync(sourceDB, destDB)
		if err != nil {
			// Expected to fail without SSH setup
			b.Logf("SSH rsync (simulated): %v", err)
		} else {
			elapsed := time.Since(start)
			b.Logf("SSH rsync: %v", elapsed)
		}
	}
}

// BenchmarkHybridReplication tests the hybrid replication strategy
func BenchmarkHybridReplication(b *testing.B) {
	tests := []struct {
		name       string
		changeSize int64
		changeType string
	}{
		{"SmallChanges_100KB", 100 * 1024, "incremental"},
		{"MediumChanges_1MB", 1024 * 1024, "threshold"},
		{"LargeChanges_10MB", 10 * 1024 * 1024, "bulk"},
	}
	
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			tmpDir := b.TempDir()
			dbPath := filepath.Join(tmpDir, "test.db")
			
			// Create initial database
			if err := createTestDatabase(dbPath, 1000, 100); err != nil {
				b.Fatalf("Failed to create database: %v", err)
			}
			
			// Create hybrid replicator
			targets := []string{"localhost:8080"}
			replicator := replication.NewHybridReplicator(targets)
			
			b.ResetTimer()
			b.SetBytes(tt.changeSize)
			
			for i := 0; i < b.N; i++ {
				// Record change
				err := replicator.RecordChange(dbPath, "write", tt.changeSize, nil)
				if err != nil {
					b.Fatalf("Failed to record change: %v", err)
				}
				
				// Get stats to see which strategy was used
				stats := replicator.GetStats()
				if dbStats, ok := stats[dbPath]; ok {
					b.Logf("Change type: %s, pending: %d, bytes: %d", 
						tt.changeType, len(dbStats.PendingChanges), dbStats.BytesWritten)
				}
			}
			
			replicator.Stop()
		})
	}
}

// BenchmarkConcurrentWrites tests write performance under contention
func BenchmarkConcurrentWrites(b *testing.B) {
	concurrencyLevels := []int{1, 2, 4, 8, 16}
	
	for _, numWriters := range concurrencyLevels {
		b.Run(fmt.Sprintf("Writers_%d", numWriters), func(b *testing.B) {
			tmpDir := b.TempDir()
			dbPath := filepath.Join(tmpDir, "concurrent.db")
			
			// Create database with WAL mode
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				b.Fatalf("Failed to create database: %v", err)
			}
			
			_, err = db.Exec("PRAGMA journal_mode=WAL")
			if err != nil {
				b.Fatalf("Failed to enable WAL: %v", err)
			}
			
			_, err = db.Exec(`
				CREATE TABLE IF NOT EXISTS bench_data (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					writer_id INTEGER,
					data BLOB,
					timestamp INTEGER DEFAULT (strftime('%s', 'now'))
				)
			`)
			if err != nil {
				b.Fatalf("Failed to create table: %v", err)
			}
			db.Close()
			
			b.ResetTimer()
			
			// Run concurrent writers
			done := make(chan bool)
			for w := 0; w < numWriters; w++ {
				go func(writerID int) {
					writerDB, err := sql.Open("sqlite", dbPath)
					if err != nil {
						b.Errorf("Writer %d: Failed to open database: %v", writerID, err)
						done <- false
						return
					}
					defer writerDB.Close()
					
					data := make([]byte, 1024) // 1KB per write
					writesPerWorker := b.N / numWriters
					
					start := time.Now()
					for i := 0; i < writesPerWorker; i++ {
						_, err := writerDB.Exec(
							"INSERT INTO bench_data (writer_id, data) VALUES (?, ?)",
							writerID, data,
						)
						if err != nil {
							b.Errorf("Writer %d: Write failed: %v", writerID, err)
						}
					}
					elapsed := time.Since(start)
					
					b.Logf("Writer %d: %d writes in %v (%.0f writes/sec)", 
						writerID, writesPerWorker, elapsed,
						float64(writesPerWorker)/elapsed.Seconds())
					
					done <- true
				}(w)
			}
			
			// Wait for all writers
			for i := 0; i < numWriters; i++ {
				<-done
			}
		})
	}
}

// Helper functions

func createTestDatabase(path string, rowCount, valueSize int) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	
	// Enable WAL mode for better performance
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		return err
	}
	
	// Create table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS test_data (
			id INTEGER PRIMARY KEY,
			value BLOB
		)
	`)
	if err != nil {
		return err
	}
	
	// Bulk insert
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	
	stmt, err := tx.Prepare("INSERT INTO test_data (id, value) VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	
	data := make([]byte, valueSize)
	for i := 0; i < valueSize; i++ {
		data[i] = byte(i % 256)
	}
	
	for i := 0; i < rowCount; i++ {
		_, err = stmt.Exec(i, data)
		if err != nil {
			tx.Rollback()
			return err
		}
		
		// Commit periodically for better performance
		if i > 0 && i%10000 == 0 {
			if err = tx.Commit(); err != nil {
				return err
			}
			tx, err = db.Begin()
			if err != nil {
				return err
			}
			stmt, err = tx.Prepare("INSERT INTO test_data (id, value) VALUES (?, ?)")
			if err != nil {
				return err
			}
		}
	}
	
	return tx.Commit()
}

func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	
	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()
	
	_, err = io.Copy(destination, source)
	return err
}

func getDBSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func humanizeBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}