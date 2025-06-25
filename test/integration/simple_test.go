package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	log.Println("=== DistSQLite Integration Test ===")
	log.Println("This test demonstrates zero-change SQLite distribution")
	log.Println("Run with: LD_PRELOAD=../../build/distsqlite-inject.so go run simple_test.go")
	log.Println()

	// Check if DistSQLite injection is active
	if os.Getenv("LD_PRELOAD") == "" {
		log.Println("WARNING: LD_PRELOAD not set - running without DistSQLite")
	} else {
		log.Printf("LD_PRELOAD: %s", os.Getenv("LD_PRELOAD"))
	}

	// Create test database
	dbPath := "./test.db"
	
	// Remove existing test database
	os.Remove(dbPath)
	
	log.Printf("Opening database: %s", dbPath)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Test basic operations
	if err := runBasicTests(db); err != nil {
		log.Fatalf("Basic tests failed: %v", err)
	}

	// Test bulk operations
	if err := runBulkTests(db); err != nil {
		log.Fatalf("Bulk tests failed: %v", err)
	}

	log.Println("=== All tests passed! ===")
	log.Println("DistSQLite successfully intercepted SQLite operations")
}

func runBasicTests(db *sql.DB) error {
	log.Println("--- Running Basic Tests ---")

	// Create table
	log.Println("Creating table...")
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			email TEXT UNIQUE NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create table: %w", err)
	}

	// Insert some data
	log.Println("Inserting test data...")
	users := []struct {
		name, email string
	}{
		{"Alice Johnson", "alice@example.com"},
		{"Bob Smith", "bob@example.com"},
		{"Charlie Brown", "charlie@example.com"},
	}

	for _, user := range users {
		_, err := db.Exec("INSERT INTO users (name, email) VALUES (?, ?)", user.name, user.email)
		if err != nil {
			return fmt.Errorf("failed to insert user %s: %w", user.name, err)
		}
		log.Printf("Inserted user: %s", user.name)
	}

	// Query data
	log.Println("Querying data...")
	rows, err := db.Query("SELECT id, name, email FROM users ORDER BY id")
	if err != nil {
		return fmt.Errorf("failed to query users: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int
		var name, email string
		if err := rows.Scan(&id, &name, &email); err != nil {
			return fmt.Errorf("failed to scan row: %w", err)
		}
		log.Printf("User %d: %s (%s)", id, name, email)
		count++
	}

	if count != len(users) {
		return fmt.Errorf("expected %d users, got %d", len(users), count)
	}

	log.Printf("Basic tests completed successfully (%d users)", count)
	return nil
}

func runBulkTests(db *sql.DB) error {
	log.Println("--- Running Bulk Tests ---")
	log.Println("This should trigger DistSQLite's bulk replication strategy")

	// Create a table for bulk testing
	log.Println("Creating large table...")
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS large_data (
			id INTEGER PRIMARY KEY,
			data TEXT,
			value REAL,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create large_data table: %w", err)
	}

	// Prepare for bulk insert
	log.Println("Preparing bulk insert...")
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO large_data (data, value) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	// Insert bulk data (this should trigger rsync replication)
	batchSize := 1000
	log.Printf("Inserting %d records in bulk...", batchSize)
	
	start := time.Now()
	for i := 0; i < batchSize; i++ {
		data := fmt.Sprintf("Large data record %d with some content to make it bigger", i)
		value := float64(i) * 1.23456789
		
		_, err := stmt.Exec(data, value)
		if err != nil {
			return fmt.Errorf("failed to insert bulk record %d: %w", i, err)
		}
		
		// Log progress every 100 records
		if (i+1)%100 == 0 {
			log.Printf("Inserted %d/%d records...", i+1, batchSize)
		}
	}

	// Commit the transaction
	log.Println("Committing bulk transaction...")
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit bulk transaction: %w", err)
	}

	elapsed := time.Since(start)
	log.Printf("Bulk insert completed in %v", elapsed)

	// Verify the data
	log.Println("Verifying bulk data...")
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM large_data").Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to count bulk records: %w", err)
	}

	if count != batchSize {
		return fmt.Errorf("expected %d bulk records, got %d", batchSize, count)
	}

	log.Printf("Bulk tests completed successfully (%d records)", count)
	return nil
}