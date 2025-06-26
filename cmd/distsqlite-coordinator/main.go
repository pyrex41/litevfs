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
		addr = flag.String("addr", ":9090", "HTTP server address")
		help = flag.Bool("help", false, "Show help")
	)
	flag.Parse()

	if *help {
		flag.Usage()
		return
	}

	log.Printf("Starting DistSQLite HTTP Coordinator on %s", *addr)

	// Create coordinator
	coord := coordinator.NewCoordinator()

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