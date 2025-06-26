package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"distsqlite/pkg/leaseserver"
)

func main() {
	var (
		port   = flag.Int("port", 8080, "Port to listen on")
		dbPath = flag.String("db", "main.db", "Path to the authoritative database")
	)
	flag.Parse()

	// Create and start the lease server
	server := leaseserver.NewLeaseServer(*port, *dbPath)
	
	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	
	go func() {
		<-sigChan
		log.Println("Shutting down lease server...")
		if err := server.Stop(); err != nil {
			log.Printf("Error stopping server: %v", err)
		}
		os.Exit(0)
	}()
	
	log.Printf("Starting DistSQLite lease server on port %d", *port)
	log.Printf("Managing database: %s", *dbPath)
	
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}