package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"distsqlite/pkg/lease"
	"distsqlite/pkg/replication"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

const (
	DefaultHTTPAddr = ":8080"
	DefaultConsulURL = "http://localhost:8500"
	DefaultLeaseKey = "distsqlite/primary"
)

// Config holds the sidecar configuration
type Config struct {
	// General settings
	Hostname     string
	AdvertiseURL string
	DataDir      string
	
	// HTTP server settings
	HTTPAddr string
	
	// Consul lease settings
	ConsulURL string
	LeaseKey  string
	
	// Replication settings
	ReplicationTargets []string
	
	// Auto-start mode (launched by injection library)
	AutoStart bool
}

// Server represents the main DistSQLite sidecar server
type Server struct {
	config      *Config
	logger      *zap.Logger
	leaseManager *lease.Manager
	replicator  *replication.HybridReplicator
	httpServer  *http.Server
	
	// Shutdown coordination
	ctx    context.Context
	cancel context.CancelFunc
}

func main() {
	var config Config
	
	rootCmd := &cobra.Command{
		Use:   "distsqlite",
		Short: "DistSQLite - Zero-change distributed SQLite",
		Long: `DistSQLite provides zero-code-change distribution for SQLite applications.
It acts as a sidecar process that handles replication and coordination automatically.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServer(&config)
		},
	}
	
	// Add flags
	rootCmd.Flags().StringVar(&config.Hostname, "hostname", getHostname(), "Node hostname")
	rootCmd.Flags().StringVar(&config.AdvertiseURL, "advertise-url", "", "URL for other nodes to access this node")
	rootCmd.Flags().StringVar(&config.DataDir, "data-dir", "./data", "Data directory for databases")
	rootCmd.Flags().StringVar(&config.HTTPAddr, "http-addr", DefaultHTTPAddr, "HTTP server address")
	rootCmd.Flags().StringVar(&config.ConsulURL, "consul-url", DefaultConsulURL, "Consul server URL")
	rootCmd.Flags().StringVar(&config.LeaseKey, "lease-key", DefaultLeaseKey, "Consul key for lease coordination")
	rootCmd.Flags().StringSliceVar(&config.ReplicationTargets, "targets", []string{}, "Replication target nodes")
	rootCmd.Flags().BoolVar(&config.AutoStart, "auto-start", false, "Auto-start mode (launched by injection library)")
	
	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Failed to execute command: %v", err)
	}
}

func runServer(config *Config) error {
	// Set up structured logging
	logger, err := zap.NewProduction()
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}
	defer logger.Sync()
	
	// Validate configuration
	if err := validateConfig(config); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	
	// Create server
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		config: config,
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	}
	
	// Initialize components
	if err := server.initialize(); err != nil {
		return fmt.Errorf("failed to initialize server: %w", err)
	}
	
	// Start server
	if err := server.start(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}
	
	// Wait for shutdown signal
	server.waitForShutdown()
	
	// Graceful shutdown
	return server.shutdown()
}

func (s *Server) initialize() error {
	s.logger.Info("Initializing DistSQLite sidecar",
		zap.String("hostname", s.config.Hostname),
		zap.String("advertise_url", s.config.AdvertiseURL),
		zap.String("data_dir", s.config.DataDir),
	)
	
	// Ensure data directory exists
	if err := os.MkdirAll(s.config.DataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}
	
	// Initialize lease manager
	s.leaseManager = lease.NewManager(
		s.config.ConsulURL,
		s.config.LeaseKey,
		s.config.Hostname,
		s.config.AdvertiseURL,
	)
	
	if err := s.leaseManager.Open(); err != nil {
		return fmt.Errorf("failed to initialize lease manager: %w", err)
	}
	
	// Initialize replicator
	s.replicator = replication.NewHybridReplicator(s.config.ReplicationTargets)
	
	// Set up HTTP server
	s.setupHTTPServer()
	
	s.logger.Info("DistSQLite sidecar initialized successfully")
	return nil
}

func (s *Server) start() error {
	s.logger.Info("Starting DistSQLite sidecar services")
	
	// Start replicator
	if err := s.replicator.Start(); err != nil {
		return fmt.Errorf("failed to start replicator: %w", err)
	}
	
	// Start HTTP server
	go func() {
		s.logger.Info("Starting HTTP server", zap.String("addr", s.config.HTTPAddr))
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP server error", zap.Error(err))
		}
	}()
	
	// Start lease management loop
	go s.leaseLoop()
	
	s.logger.Info("DistSQLite sidecar started successfully")
	return nil
}

func (s *Server) leaseLoop() {
	ticker := time.NewTicker(5 * time.Second) // Check lease every 5 seconds
	defer ticker.Stop()
	
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.handleLeaseCheck()
		}
	}
}

func (s *Server) handleLeaseCheck() {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	
	if s.leaseManager.IsLeaseHolder() {
		// We hold the lease - try to renew it
		if err := s.leaseManager.RenewLease(ctx); err != nil {
			s.logger.Warn("Failed to renew lease", zap.Error(err))
		} else {
			s.logger.Debug("Lease renewed successfully")
		}
	} else {
		// We don't hold the lease - try to acquire it
		lease, err := s.leaseManager.AcquireLease(ctx)
		if err != nil {
			s.logger.Debug("Failed to acquire lease", zap.Error(err))
			return
		}
		
		if lease.Holder == s.config.Hostname {
			s.logger.Info("Acquired primary lease")
		} else {
			s.logger.Debug("Current lease holder", zap.String("holder", lease.Holder))
		}
	}
}

func (s *Server) setupHTTPServer() {
	mux := http.NewServeMux()
	
	// Health check endpoint
	mux.HandleFunc("/health", s.handleHealth)
	
	// Status endpoint
	mux.HandleFunc("/status", s.handleStatus)
	
	// Replication stats endpoint
	mux.HandleFunc("/stats", s.handleStats)
	
	// Lease information endpoint
	mux.HandleFunc("/lease", s.handleLease)
	
	s.httpServer = &http.Server{
		Addr:    s.config.HTTPAddr,
		Handler: mux,
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Check if replicator is healthy
	healthy := s.replicator.IsHealthy()
	
	if healthy {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status": "healthy", "timestamp": "%s"}`, time.Now().Format(time.RFC3339))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"status": "unhealthy", "timestamp": "%s"}`, time.Now().Format(time.RFC3339))
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	isPrimary := s.leaseManager.IsLeaseHolder()
	currentLeaseHolder := "unknown"
	
	if lease, err := s.leaseManager.GetCurrentLease(r.Context()); err == nil {
		currentLeaseHolder = lease.Holder
	}
	
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{
		"hostname": "%s",
		"is_primary": %t,
		"current_primary": "%s",
		"advertise_url": "%s",
		"replicator_healthy": %t,
		"timestamp": "%s"
	}`, s.config.Hostname, isPrimary, currentLeaseHolder, s.config.AdvertiseURL, 
		s.replicator.IsHealthy(), time.Now().Format(time.RFC3339))
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	stats := s.replicator.GetStats()
	
	w.Header().Set("Content-Type", "application/json")
	
	// Simple JSON serialization for now
	fmt.Fprintf(w, `{"databases": %d, "timestamp": "%s"}`, 
		len(stats), time.Now().Format(time.RFC3339))
}

func (s *Server) handleLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	lease, err := s.leaseManager.GetCurrentLease(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get lease: %v", err), http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{
		"holder": "%s",
		"expires_at": "%s",
		"generation": %d,
		"region": "%s",
		"advertise_url": "%s"
	}`, lease.Holder, lease.ExpiresAt.Format(time.RFC3339), 
		lease.Generation, lease.Region, lease.AdvertiseURL)
}

func (s *Server) waitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	
	select {
	case sig := <-sigChan:
		s.logger.Info("Received shutdown signal", zap.String("signal", sig.String()))
	case <-s.ctx.Done():
		s.logger.Info("Context cancelled, shutting down")
	}
}

func (s *Server) shutdown() error {
	s.logger.Info("Shutting down DistSQLite sidecar")
	
	// Cancel context to stop background goroutines
	s.cancel()
	
	// Shutdown HTTP server
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		
		if err := s.httpServer.Shutdown(ctx); err != nil {
			s.logger.Error("Failed to shutdown HTTP server", zap.Error(err))
		}
	}
	
	// Stop replicator
	if s.replicator != nil {
		if err := s.replicator.Stop(); err != nil {
			s.logger.Error("Failed to stop replicator", zap.Error(err))
		}
	}
	
	// Release lease and close lease manager
	if s.leaseManager != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		
		if s.leaseManager.IsLeaseHolder() {
			if err := s.leaseManager.ReleaseLease(ctx); err != nil {
				s.logger.Error("Failed to release lease", zap.Error(err))
			}
		}
		
		if err := s.leaseManager.Close(); err != nil {
			s.logger.Error("Failed to close lease manager", zap.Error(err))
		}
	}
	
	s.logger.Info("DistSQLite sidecar shutdown complete")
	return nil
}

func validateConfig(config *Config) error {
	if config.Hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	
	if config.AdvertiseURL == "" {
		// Generate default advertise URL
		config.AdvertiseURL = fmt.Sprintf("http://%s%s", config.Hostname, config.HTTPAddr)
	}
	
	if config.DataDir == "" {
		return fmt.Errorf("data directory is required")
	}
	
	// Validate data directory path
	if !filepath.IsAbs(config.DataDir) {
		abs, err := filepath.Abs(config.DataDir)
		if err != nil {
			return fmt.Errorf("invalid data directory path: %w", err)
		}
		config.DataDir = abs
	}
	
	return nil
}

func getHostname() string {
	if hostname := os.Getenv("HOSTNAME"); hostname != "" {
		return hostname
	}
	
	if hostname := os.Getenv("DISTSQLITE_HOSTNAME"); hostname != "" {
		return hostname
	}
	
	if hostname, err := os.Hostname(); err == nil {
		// Clean up hostname to be DNS-safe
		hostname = strings.ToLower(hostname)
		hostname = strings.ReplaceAll(hostname, "_", "-")
		return hostname
	}
	
	return "unknown"
}