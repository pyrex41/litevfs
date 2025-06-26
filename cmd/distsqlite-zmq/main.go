package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"distsqlite/pkg/zmq"
)

func main() {
	var (
		mode     = flag.String("mode", "coordinator", "Mode: coordinator or node")
		nodeID   = flag.String("node-id", "node1", "Node identifier")
		bindAddr = flag.String("bind", "0.0.0.0:5555", "Bind address for coordinator")
		peers    = flag.String("peers", "", "Comma-separated list of coordinator peers")
		dbPath   = flag.String("db", "test.db", "Database path for testing")
		help     = flag.Bool("help", false, "Show help")
	)
	flag.Parse()

	if *help {
		flag.Usage()
		return
	}

	log.Printf("Starting DistSQLite ZMQ in %s mode", *mode)

	// Create transport config
	config := zmq.DefaultTransportConfig()
	config.BindAddr = *bindAddr
	if *peers != "" {
		// Simple parsing - in production use proper CSV parsing
		config.Peers = []string{*peers}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		cancel()
	}()

	switch *mode {
	case "coordinator":
		if err := runCoordinator(ctx, config, *nodeID); err != nil {
			log.Fatalf("Coordinator failed: %v", err)
		}
	case "node":
		if err := runNode(ctx, config, *nodeID, *dbPath); err != nil {
			log.Fatalf("Node failed: %v", err)
		}
	default:
		log.Fatalf("Unknown mode: %s", *mode)
	}
}

// runCoordinator starts a ZMQ coordinator
func runCoordinator(ctx context.Context, config *zmq.TransportConfig, nodeID string) error {
	coordinator, err := zmq.NewCoordinator(config, nodeID)
	if err != nil {
		return err
	}
	defer coordinator.Stop()

	if err := coordinator.Start(); err != nil {
		return err
	}

	log.Printf("ZMQ Coordinator running on %s", config.BindAddr)

	<-ctx.Done()
	return nil
}

// runNode starts a ZMQ node
func runNode(ctx context.Context, config *zmq.TransportConfig, nodeID, dbPath string) error {
	transport, err := zmq.NewTransport(config, nodeID)
	if err != nil {
		return err
	}
	defer transport.Close()

	if err := transport.StartNode(); err != nil {
		return err
	}

	log.Printf("ZMQ Node %s connected", nodeID)

	// Test lease operations
	go testLeaseOperations(ctx, transport, dbPath)

	<-ctx.Done()
	return nil
}

// testLeaseOperations demonstrates lease functionality
func testLeaseOperations(ctx context.Context, transport *zmq.Transport, dbPath string) {
	time.Sleep(1 * time.Second) // Give coordinator time to start

	// Try to acquire lease
	log.Printf("Attempting to acquire lease for %s", dbPath)
	resp, err := transport.AcquireLease(ctx, dbPath, 30)
	if err != nil {
		log.Printf("Failed to acquire lease: %v", err)
		return
	}

	if resp.Success {
		log.Printf("Successfully acquired lease (token: %d)", resp.FenceToken)

		// Start auto-renewal
		renewCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		errChan := transport.AutoRenewLease(renewCtx, dbPath, 10*time.Second)

		// Test lease for 30 seconds
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()

		select {
		case <-timer.C:
			log.Printf("Test completed, releasing lease")
			if err := transport.ReleaseLease(ctx, dbPath); err != nil {
				log.Printf("Failed to release lease: %v", err)
			}
		case err := <-errChan:
			log.Printf("Auto-renewal failed: %v", err)
		case <-ctx.Done():
			return
		}
	} else {
		log.Printf("Failed to acquire lease: %s", resp.Error)
	}
}