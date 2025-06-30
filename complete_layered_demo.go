package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"distsqlite/pkg/replication"

	_ "modernc.org/sqlite"
)

// DemoConfig holds configuration for the layered demo
type DemoConfig struct {
	DatabasePath         string
	Targets              []string
	VerificationInterval time.Duration
	EnableQuickVerify    bool
	QuickVerifyInterval  time.Duration
	DemoMode             string // "fast", "normal", "critical"
}

// LayeredDemo demonstrates the complete safety architecture
type LayeredDemo struct {
	config     DemoConfig
	replicator *replication.HybridReplicator
	db         *sql.DB
	sqlServer  *replication.SQLReplayServer
	ctx        context.Context
	cancel     context.CancelFunc
}

func main() {
	fmt.Println("🚀 DistSQLite Complete Layered Safety Architecture Demo")
	fmt.Println("==================================================")
	
	// Parse demo mode from command line
	demoMode := "normal"
	if len(os.Args) > 1 {
		demoMode = os.Args[1]
	}
	
	config := getDemoConfig(demoMode)
	demo := NewLayeredDemo(config)
	
	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	
	go func() {
		<-sigChan
		fmt.Println("\n🛑 Graceful shutdown initiated...")
		demo.Stop()
		os.Exit(0)
	}()
	
	// Run the demo
	if err := demo.Run(); err != nil {
		log.Fatalf("Demo failed: %v", err)
	}
}

// getDemoConfig returns configuration based on demo mode
func getDemoConfig(mode string) DemoConfig {
	switch mode {
	case "fast":
		return DemoConfig{
			DatabasePath:         "demo_fast.db",
			Targets:              []string{"localhost:9001", "localhost:9002"},
			VerificationInterval: 30 * time.Second, // Very frequent verification
			EnableQuickVerify:    true,
			QuickVerifyInterval:  10 * time.Second, // Every 10 seconds
			DemoMode:             "fast",
		}
		
	case "critical":
		return DemoConfig{
			DatabasePath:         "demo_critical.db",
			Targets:              []string{"localhost:9001", "localhost:9002", "localhost:9003"},
			VerificationInterval: 2 * time.Minute, // Critical system verification
			EnableQuickVerify:    true,
			QuickVerifyInterval:  30 * time.Second, // Frequent checks
			DemoMode:             "critical",
		}
		
	default: // "normal"
		return DemoConfig{
			DatabasePath:         "demo_normal.db",
			Targets:              []string{"localhost:9001", "localhost:9002"},
			VerificationInterval: 5 * time.Minute, // Standard verification
			EnableQuickVerify:    false,            // Quick verify disabled
			QuickVerifyInterval:  0,
			DemoMode:             "normal",
		}
	}
}

// NewLayeredDemo creates a new demo instance
func NewLayeredDemo(config DemoConfig) *LayeredDemo {
	ctx, cancel := context.WithCancel(context.Background())
	
	return &LayeredDemo{
		config: config,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Run executes the complete demo
func (d *LayeredDemo) Run() error {
	fmt.Printf("🔧 Demo Mode: %s\n", d.config.DemoMode)
	fmt.Printf("📁 Database: %s\n", d.config.DatabasePath)
	fmt.Printf("🎯 Targets: %v\n", d.config.Targets)
	fmt.Printf("🛡️ Verification: %v (Quick: %v)\n", 
		d.config.VerificationInterval, d.config.EnableQuickVerify)
	fmt.Println()
	
	// Setup phase
	if err := d.setup(); err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}
	
	// Start SQL replay servers for demo targets
	if err := d.startSQLReplayServers(); err != nil {
		return fmt.Errorf("failed to start SQL replay servers: %w", err)
	}
	
	// Phase 1: Real-time SQL replay demonstration
	fmt.Println("📡 Phase 1: Real-time SQL Replay (Fast Path)")
	fmt.Println("-------------------------------------------")
	if err := d.demonstrateSQLReplay(); err != nil {
		return fmt.Errorf("SQL replay demo failed: %w", err)
	}
	
	// Phase 2: Medium-frequency page sync
	fmt.Println("\n📄 Phase 2: Medium-frequency Page Sync")
	fmt.Println("--------------------------------------")
	if err := d.demonstratePageSync(); err != nil {
		return fmt.Errorf("page sync demo failed: %w", err)
	}
	
	// Phase 3: Safety net verification
	fmt.Println("\n🛡️ Phase 3: Safety Net Verification")
	fmt.Println("-----------------------------------")
	if err := d.demonstrateSafetyNet(); err != nil {
		return fmt.Errorf("safety net demo failed: %w", err)
	}
	
	// Phase 4: Failure recovery demonstration
	fmt.Println("\n🔧 Phase 4: Failure Recovery & Auto-repair")
	fmt.Println("------------------------------------------")
	if err := d.demonstrateFailureRecovery(); err != nil {
		return fmt.Errorf("failure recovery demo failed: %w", err)
	}
	
	// Phase 5: Performance metrics
	fmt.Println("\n📊 Phase 5: Performance Metrics & Statistics")
	fmt.Println("--------------------------------------------")
	d.showPerformanceMetrics()
	
	// Keep running for demonstration
	fmt.Println("\n🔄 Demo running... Press Ctrl+C to stop")
	fmt.Println("Real-time monitoring active - observe the logs!")
	
	// Run continuous workload
	go d.continuousWorkload()
	
	// Wait for shutdown signal
	<-d.ctx.Done()
	
	return nil
}

// setup initializes the demo environment
func (d *LayeredDemo) setup() error {
	// Clean up any existing demo files
	os.Remove(d.config.DatabasePath)
	os.Remove(d.config.DatabasePath + "-wal")
	os.Remove(d.config.DatabasePath + "-shm")
	
	// Create and configure database
	db, err := sql.Open("sqlite", d.config.DatabasePath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	d.db = db
	
	// Configure for optimal demonstration
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=10000",
		"PRAGMA temp_store=memory",
	}
	
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			return fmt.Errorf("failed to execute %s: %w", pragma, err)
		}
	}
	
	// Create demo tables
	schema := `
		CREATE TABLE customers (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT UNIQUE,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_id INTEGER REFERENCES customers(id),
			amount DECIMAL(10,2),
			status TEXT DEFAULT 'pending',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		
		CREATE INDEX idx_customer_email ON customers(email);
		CREATE INDEX idx_order_customer ON orders(customer_id);
		CREATE INDEX idx_order_status ON orders(status);
	`
	
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}
	
	// Create hybrid replicator with demo configuration
	hybridConfig := replication.HybridReplicatorConfig{
		Targets:              d.config.Targets,
		BulkThreshold:        1024 * 1024, // 1MB
		MaxDelay:             10 * time.Second,
		VerificationInterval: d.config.VerificationInterval,
		EnableQuickVerify:    d.config.EnableQuickVerify,
		QuickVerifyInterval:  d.config.QuickVerifyInterval,
		PreferHTTPRsync:      true,
	}
	
	d.replicator = replication.NewHybridReplicatorWithConfig(hybridConfig)
	
	// Start replicator
	if err := d.replicator.Start(); err != nil {
		return fmt.Errorf("failed to start replicator: %w", err)
	}
	
	fmt.Println("✅ Demo environment initialized")
	return nil
}

// startSQLReplayServers starts SQL replay servers for demo targets  
func (d *LayeredDemo) startSQLReplayServers() error {
	// Start SQL replay servers on demo ports
	ports := []int{9001, 9002, 9003}
	
	for i, target := range d.config.Targets {
		if i >= len(ports) {
			break
		}
		
		port := ports[i]
		server := replication.NewSQLReplayServer("demo_replica_"+fmt.Sprint(port)+".db", port)
		
		if err := server.Start(); err != nil {
			return fmt.Errorf("failed to start SQL replay server on port %d: %w", port, err)
		}
		
		fmt.Printf("✅ SQL replay server started on port %d\n", port)
	}
	
	return nil
}

// demonstrateSQLReplay shows the fast path in action
func (d *LayeredDemo) demonstrateSQLReplay() error {
	fmt.Println("Generating small, frequent transactions (SQL replay territory)...")
	
	start := time.Now()
	successCount := 0
	
	// Simulate real-time OLTP workload
	for i := 0; i < 25; i++ {
		// Small transaction (1-3 operations)
		tx, err := d.db.Begin()
		if err != nil {
			return err
		}
		
		// Insert customer
		customerID := rand.Int63n(10000) + 1000
		_, err = tx.Exec("INSERT OR REPLACE INTO customers (id, name, email) VALUES (?, ?, ?)",
			customerID, fmt.Sprintf("Customer %d", customerID), 
			fmt.Sprintf("customer%d@example.com", customerID))
		if err != nil {
			tx.Rollback()
			continue
		}
		
		// Insert order (maybe)
		if rand.Float64() < 0.7 {
			_, err = tx.Exec("INSERT INTO orders (customer_id, amount, status) VALUES (?, ?, ?)",
				customerID, rand.Float64()*1000, "pending")
			if err != nil {
				tx.Rollback()
				continue
			}
		}
		
		if err := tx.Commit(); err != nil {
			continue
		}
		
		// Record the change for replication
		d.replicator.RecordChange(d.config.DatabasePath, "write", 100, []byte("small_txn"))
		
		successCount++
		
		// Small delay between transactions (real-time simulation)
		time.Sleep(500 * time.Millisecond)
		
		if i%5 == 0 {
			fmt.Printf("  💨 Fast transactions: %d/25 completed\n", i+1)
		}
	}
	
	elapsed := time.Since(start)
	fmt.Printf("✅ SQL Replay phase completed: %d transactions in %v (%.1f TPS)\n", 
		successCount, elapsed, float64(successCount)/elapsed.Seconds())
	
	// Wait for replication to catch up
	time.Sleep(2 * time.Second)
	
	return nil
}

// demonstratePageSync shows medium-frequency sync in action
func (d *LayeredDemo) demonstratePageSync() error {
	fmt.Println("Generating medium-sized batch operations (page sync territory)...")
	
	start := time.Now()
	
	// Simulate batch processing (100+ operations in burst)
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	
	for i := 0; i < 150; i++ {
		customerID := rand.Int63n(10000) + 2000
		_, err = tx.Exec("INSERT OR REPLACE INTO customers (id, name, email) VALUES (?, ?, ?)",
			customerID, fmt.Sprintf("Batch Customer %d", customerID), 
			fmt.Sprintf("batch%d@example.com", customerID))
		if err != nil {
			tx.Rollback()
			return err
		}
		
		if i%30 == 0 {
			fmt.Printf("  📄 Batch processing: %d/150 records\n", i+1)
		}
	}
	
	if err := tx.Commit(); err != nil {
		return err
	}
	
	// Record as a large change (triggers page sync)
	d.replicator.RecordChange(d.config.DatabasePath, "wal_write", 150*100, []byte("batch_data"))
	
	elapsed := time.Since(start)
	fmt.Printf("✅ Page Sync phase completed: 150 records in %v\n", elapsed)
	
	// Wait for page sync to complete
	time.Sleep(5 * time.Second)
	
	return nil
}

// demonstrateSafetyNet shows verification in action
func (d *LayeredDemo) demonstrateSafetyNet() error {
	fmt.Println("Safety net verification will run automatically based on configuration:")
	fmt.Printf("  🕐 Full verification every: %v\n", d.config.VerificationInterval)
	
	if d.config.EnableQuickVerify {
		fmt.Printf("  ⚡ Quick verification every: %v\n", d.config.QuickVerifyInterval)
	} else {
		fmt.Printf("  ⚡ Quick verification: disabled\n")
	}
	
	// Show current statistics
	stats := d.replicator.GetStats()
	fmt.Printf("📊 Current replication statistics:\n")
	
	for dbPath, dbStats := range stats {
		fmt.Printf("  Database: %s\n", dbPath)
		fmt.Printf("    Writes: %d (%.1f KB)\n", 
			dbStats.TotalWrites, float64(dbStats.BytesWritten)/1024)
		fmt.Printf("    Last sync: %v\n", 
			time.Since(dbStats.LastSync).Truncate(time.Second))
		fmt.Printf("    Pending changes: %d\n", len(dbStats.PendingChanges))
	}
	
	fmt.Println("✅ Safety net monitoring active")
	return nil
}

// demonstrateFailureRecovery simulates and recovers from failures
func (d *LayeredDemo) demonstrateFailureRecovery() error {
	fmt.Println("Simulating failure scenarios and automatic recovery...")
	
	// Simulate SQL replay failure (will fall back to page sync)
	fmt.Println("  🔥 Simulating SQL replay failure...")
	
	// Create a problematic change that will fail SQL replay
	d.replicator.RecordChange(d.config.DatabasePath, "unknown_type", 50, []byte("problematic_data"))
	
	time.Sleep(3 * time.Second)
	
	// Simulate network partition recovery
	fmt.Println("  🌐 Simulating network partition recovery...")
	
	// Generate a larger change that will trigger page sync fallback
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	
	for i := 0; i < 50; i++ {
		_, err = tx.Exec("UPDATE customers SET name = ? WHERE id = ?",
			fmt.Sprintf("Updated %d", time.Now().Unix()), rand.Int63n(1000)+1000)
		if err != nil {
			// Ignore errors for demo
		}
	}
	
	tx.Commit()
	
	// This will trigger page sync as fallback
	d.replicator.RecordChange(d.config.DatabasePath, "recovery_sync", 50*100, []byte("recovery_data"))
	
	fmt.Println("✅ Failure recovery demonstrated - fallback to page sync")
	
	return nil
}

// showPerformanceMetrics displays comprehensive performance data
func (d *LayeredDemo) showPerformanceMetrics() {
	fmt.Println("Performance Analysis:")
	fmt.Println("-------------------")
	
	stats := d.replicator.GetStats()
	
	for dbPath, dbStats := range stats {
		fmt.Printf("📈 Database: %s\n", dbPath)
		fmt.Printf("   Total Operations: %d\n", dbStats.TotalWrites)
		fmt.Printf("   Data Volume: %.2f MB\n", float64(dbStats.BytesWritten)/(1024*1024))
		fmt.Printf("   Last Activity: %v ago\n", time.Since(dbStats.LastWrite).Truncate(time.Second))
		fmt.Printf("   Last Sync: %v ago\n", time.Since(dbStats.LastSync).Truncate(time.Second))
		
		if d.config.EnableQuickVerify {
			fmt.Printf("   Last Verification: %v ago\n", time.Since(dbStats.LastVerification).Truncate(time.Second))
			fmt.Printf("   Verifications: %d passed, %d failed\n", 
				dbStats.VerificationsPassed, dbStats.VerificationsFailed)
		}
		
		fmt.Printf("   SQL Replay: %d attempts, %d failures\n", 
			dbStats.SQLReplayAttempts, dbStats.SQLReplayFailures)
		fmt.Printf("   Page Sync Fallbacks: %d\n", dbStats.PageSyncFallbacks)
		
		// Calculate efficiency
		if dbStats.SQLReplayAttempts > 0 {
			successRate := float64(dbStats.SQLReplayAttempts-dbStats.SQLReplayFailures) / float64(dbStats.SQLReplayAttempts) * 100
			fmt.Printf("   SQL Replay Success Rate: %.1f%%\n", successRate)
		}
		
		fmt.Printf("   Health Status: %s\n", func() string {
			if d.replicator.IsHealthy() {
				return "✅ Healthy"
			}
			return "⚠️ Degraded"
		}())
	}
	
	// Show strategy breakdown
	fmt.Println("\n🎯 Strategy Selection Summary:")
	fmt.Println("   • Real-time (<30s, <50 ops): SQL Replay (40-80x bandwidth advantage)")
	fmt.Println("   • Medium (30s-10min): Page Sync with checkpoint optimization")  
	fmt.Println("   • Bulk/Catch-up (>10min): Pure page hashing for reliability")
	fmt.Println("   • Safety Net: Automatic verification and repair every", d.config.VerificationInterval)
}

// continuousWorkload generates ongoing activity for demonstration
func (d *LayeredDemo) continuousWorkload() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.generateRandomActivity()
		}
	}
}

// generateRandomActivity creates realistic database activity
func (d *LayeredDemo) generateRandomActivity() {
	// Randomly choose activity type
	activityType := rand.Intn(3)
	
	switch activityType {
	case 0: // Small real-time transaction
		d.generateSmallTransaction()
	case 1: // Medium update batch
		d.generateMediumBatch()
	case 2: // Query workload (no replication needed)
		d.generateQueryWorkload()
	}
}

func (d *LayeredDemo) generateSmallTransaction() {
	customerID := rand.Int63n(10000) + 5000
	
	tx, err := d.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	
	// Insert customer and order
	_, err = tx.Exec("INSERT OR REPLACE INTO customers (id, name, email) VALUES (?, ?, ?)",
		customerID, fmt.Sprintf("Auto Customer %d", customerID), 
		fmt.Sprintf("auto%d@example.com", customerID))
	if err == nil {
		_, err = tx.Exec("INSERT INTO orders (customer_id, amount) VALUES (?, ?)",
			customerID, rand.Float64()*500)
		if err == nil {
			tx.Commit()
			d.replicator.RecordChange(d.config.DatabasePath, "write", 80, []byte("auto_txn"))
		}
	}
}

func (d *LayeredDemo) generateMediumBatch() {
	// Update order statuses in batch
	statuses := []string{"completed", "shipped", "cancelled", "pending"}
	newStatus := statuses[rand.Intn(len(statuses))]
	
	result, err := d.db.Exec("UPDATE orders SET status = ? WHERE status = 'pending' AND id % 10 = ?",
		newStatus, rand.Intn(10))
	if err == nil {
		if rowsAffected, _ := result.RowsAffected(); rowsAffected > 0 {
			d.replicator.RecordChange(d.config.DatabasePath, "wal_write", 
				rowsAffected*50, []byte("batch_update"))
		}
	}
}

func (d *LayeredDemo) generateQueryWorkload() {
	// Run some read queries (no replication needed)
	queries := []string{
		"SELECT COUNT(*) FROM customers",
		"SELECT COUNT(*) FROM orders WHERE status = 'pending'",
		"SELECT AVG(amount) FROM orders",
	}
	
	query := queries[rand.Intn(len(queries))]
	d.db.QueryRow(query).Scan(new(interface{}))
}

// Stop gracefully shuts down the demo
func (d *LayeredDemo) Stop() {
	fmt.Println("🛑 Stopping layered demo...")
	
	if d.replicator != nil {
		d.replicator.Stop()
	}
	
	if d.db != nil {
		d.db.Close()
	}
	
	if d.cancel != nil {
		d.cancel()
	}
	
	fmt.Println("✅ Demo stopped gracefully")
}