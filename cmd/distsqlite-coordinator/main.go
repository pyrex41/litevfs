package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"distsqlite/pkg/coordinator"
)

func main() {
	var (
		addr           = flag.String("addr", ":9090", "HTTP server address")
		leaseServerPort = flag.Int("lease-port", 8080, "Embedded lease server port")
		dbPath         = flag.String("db", "main.db", "Path to authoritative database")
		help           = flag.Bool("help", false, "Show help")
	)
	flag.Parse()

	if *help {
		flag.Usage()
		return
	}

	log.Printf("Starting DistSQLite HTTP Coordinator on %s", *addr)
	log.Printf("Embedded lease server will run on port %d", *leaseServerPort)
	log.Printf("Managing database: %s", *dbPath)

	// Create coordinator with embedded lease server
	coord := coordinator.NewCoordinator(*leaseServerPort, *dbPath)

	// Start coordinator in goroutine
	go func() {
		if err := coord.Start(*addr); err != nil {
			log.Fatalf("Coordinator failed: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down coordinator...")
	if err := coord.Stop(); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}
	log.Println("Coordinator stopped")
}