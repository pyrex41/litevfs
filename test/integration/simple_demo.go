package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// User represents a simple user record
type User struct {
	ID       int
	Name     string
	Email    string
	Created  time.Time
}

func main() {
	fmt.Println("🚀 DistSQLite Zero-Change Demo Starting...")
	fmt.Println("This is a regular SQLite application - NO code changes required!")
	fmt.Println()

	// This is exactly how you'd normally use SQLite
	// DistSQLite makes it distributed automatically via LD_PRELOAD
	db, err := sql.Open("sqlite3", "./demo.db")
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Check if DistSQLite sidecar is running
	checkSidecarStatus()

	// Initialize database schema
	if err := initializeSchema(db); err != nil {
		log.Fatalf("Failed to initialize schema: %v", err)
	}

	// Demonstrate various operations
	fmt.Println("📝 Performing database operations...")
	
	// Insert some users
	users := []User{
		{Name: "Alice Johnson", Email: "alice@example.com"},
		{Name: "Bob Smith", Email: "bob@example.com"},
		{Name: "Carol Davis", Email: "carol@example.com"},
	}
	
	for _, user := range users {
		if err := insertUser(db, user); err != nil {
			log.Printf("Failed to insert user: %v", err)
		}
	}

	// Read users back
	fmt.Println("\n👥 Users in database:")
	if err := listUsers(db); err != nil {
		log.Printf("Failed to list users: %v", err)
	}

	// Perform bulk operations to trigger rsync replication
	fmt.Println("\n🔄 Performing bulk operations (triggers rsync replication)...")
	if err := performBulkOperations(db); err != nil {
		log.Printf("Failed to perform bulk operations: %v", err)
	}

	// Demonstrate transaction
	fmt.Println("\n💳 Performing transaction...")
	if err := performTransaction(db); err != nil {
		log.Printf("Failed to perform transaction: %v", err)
	}

	// Check final state
	fmt.Println("\n📊 Final database state:")
	if err := showDatabaseStats(db); err != nil {
		log.Printf("Failed to show database stats: %v", err)
	}

	fmt.Println("\n✅ Demo completed successfully!")
	fmt.Println("🎉 Your SQLite app is now globally distributed with ZERO code changes!")
	fmt.Println()
	fmt.Println("Check the sidecar status:")
	fmt.Println("  curl http://localhost:8080/status")
	fmt.Println("  curl http://localhost:8080/stats")
	fmt.Println("  curl http://localhost:8080/ipc")
}

func checkSidecarStatus() {
	fmt.Println("🔍 Checking DistSQLite sidecar status...")
	
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:8080/health")
	if err != nil {
		fmt.Printf("⚠️  Sidecar not responding: %v\n", err)
		fmt.Println("   Start it with: ./build/distsqlite")
		fmt.Println()
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode == 200 {
		fmt.Println("✅ DistSQLite sidecar is healthy!")
	} else {
		fmt.Printf("⚠️  Sidecar health check failed: %d\n", resp.StatusCode)
	}
	fmt.Println()
}

func initializeSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		email TEXT UNIQUE NOT NULL,
		created DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	
	CREATE TABLE IF NOT EXISTS orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER,
		product TEXT NOT NULL,
		amount DECIMAL(10,2),
		created DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (user_id) REFERENCES users(id)
	);
	
	CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
	CREATE INDEX IF NOT EXISTS idx_orders_user_id ON orders(user_id);
	`
	
	_, err := db.Exec(schema)
	if err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}
	
	fmt.Println("✅ Database schema initialized")
	return nil
}

func insertUser(db *sql.DB, user User) error {
	query := `INSERT INTO users (name, email) VALUES (?, ?)`
	result, err := db.Exec(query, user.Name, user.Email)
	if err != nil {
		return fmt.Errorf("failed to insert user: %w", err)
	}
	
	id, _ := result.LastInsertId()
	fmt.Printf("   → Added user: %s (ID: %d)\n", user.Name, id)
	return nil
}

func listUsers(db *sql.DB) error {
	query := `SELECT id, name, email, created FROM users ORDER BY id`
	rows, err := db.Query(query)
	if err != nil {
		return fmt.Errorf("failed to query users: %w", err)
	}
	defer rows.Close()
	
	for rows.Next() {
		var user User
		err := rows.Scan(&user.ID, &user.Name, &user.Email, &user.Created)
		if err != nil {
			return fmt.Errorf("failed to scan user: %w", err)
		}
		
		fmt.Printf("   → %d: %s (%s) - %s\n", 
			user.ID, user.Name, user.Email, user.Created.Format("2006-01-02 15:04:05"))
	}
	
	return rows.Err()
}

func performBulkOperations(db *sql.DB) error {
	// Start a transaction for bulk inserts
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	
	// Insert many orders to trigger bulk replication
	orderStmt, err := tx.Prepare(`INSERT INTO orders (user_id, product, amount) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer orderStmt.Close()
	
	products := []string{
		"Laptop", "Mouse", "Keyboard", "Monitor", "Headphones",
		"Tablet", "Phone", "Charger", "Cable", "Speaker",
		"Camera", "Microphone", "Webcam", "Router", "Switch",
	}
	
	fmt.Printf("   → Inserting %d orders...\n", len(products)*3)
	
	for userID := 1; userID <= 3; userID++ {
		for i, product := range products {
			amount := float64(10 + i*5) // Varying amounts
			_, err := orderStmt.Exec(userID, product, amount)
			if err != nil {
				return fmt.Errorf("failed to insert order: %w", err)
			}
		}
	}
	
	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	
	fmt.Println("   → Bulk operations completed (should trigger rsync replication)")
	return nil
}

func performTransaction(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	
	// Update user email
	_, err = tx.Exec(`UPDATE users SET email = ? WHERE id = ?`, "alice.johnson@newdomain.com", 1)
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}
	
	// Add new order for that user
	_, err = tx.Exec(`INSERT INTO orders (user_id, product, amount) VALUES (?, ?, ?)`, 
		1, "Premium Support", 99.99)
	if err != nil {
		return fmt.Errorf("failed to insert order: %w", err)
	}
	
	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	
	fmt.Println("   → Transaction completed successfully")
	return nil
}

func showDatabaseStats(db *sql.DB) error {
	// Count users
	var userCount int
	err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&userCount)
	if err != nil {
		return fmt.Errorf("failed to count users: %w", err)
	}
	
	// Count orders
	var orderCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&orderCount)
	if err != nil {
		return fmt.Errorf("failed to count orders: %w", err)
	}
	
	// Total revenue
	var totalRevenue float64
	err = db.QueryRow(`SELECT COALESCE(SUM(amount), 0) FROM orders`).Scan(&totalRevenue)
	if err != nil {
		return fmt.Errorf("failed to calculate revenue: %w", err)
	}
	
	// Get database file size
	dbFile := "./demo.db"
	stat, err := os.Stat(dbFile)
	var fileSize int64
	if err == nil {
		fileSize = stat.Size()
	}
	
	fmt.Printf("   → Users: %d\n", userCount)
	fmt.Printf("   → Orders: %d\n", orderCount)
	fmt.Printf("   → Total Revenue: $%.2f\n", totalRevenue)
	fmt.Printf("   → Database Size: %d bytes\n", fileSize)
	
	return nil
}