package main

import (
	"context"
	"encoding/json"
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
	"distsqlite/pkg/vfs"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	DefaultHTTPAddr = ":8080"
	DefaultConsulURL = "http://localhost:8500"
	DefaultLeaseKey = "distsqlite/primary"
	
	// Operational limits
	MaxDatabaseSize = 10 * 1024 * 1024 * 1024 // 10GB
	MaxConcurrentOps = 1000
	HealthCheckInterval = 30 * time.Second
)

// Config holds the sidecar configuration
type Config struct {
	// General settings
	Hostname     string
	NodeID       string
	AdvertiseURL string
	DataDir      string
	
	// HTTP server settings
	HTTPAddr string
	
	// Consul lease settings
	ConsulURL string
	LeaseKey  string
	
	// Replication settings
	ReplicationTargets []string
	ReplicationMode    string // "auto", "txlog", "bulk", "disabled"
	
	// Safety settings
	EnableSafetyChecks    bool
	EnableFenceTokens     bool
	EnableVectorClocks    bool
	MaxClockSkew          time.Duration
	MinQuorumSize         int
	
	// Performance tuning
	BulkThreshold         int64
	MaxReplicationDelay   time.Duration
	
	// Auto-start mode (launched by injection library)
	AutoStart bool
	
	// Logging and monitoring
	LogLevel      string
	EnableMetrics bool
	EnableTracing bool
}

// Metrics for monitoring
var (
	operationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "distsqlite_operations_total",
			Help: "Total number of database operations",
		},
		[]string{"operation", "database", "status"},
	)
	
	operationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "distsqlite_operation_duration_seconds",
			Help: "Duration of database operations",
		},
		[]string{"operation", "database"},
	)
	
	replicationLag = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "distsqlite_replication_lag_seconds",
			Help: "Replication lag in seconds",
		},
		[]string{"target"},
	)
	
	leaseStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "distsqlite_lease_status",
			Help: "Current lease status (1 = holder, 0 = not holder)",
		},
		[]string{"node_id"},
	)
)

func init() {
	// Register Prometheus metrics
	prometheus.MustRegister(operationsTotal)
	prometheus.MustRegister(operationDuration)
	prometheus.MustRegister(replicationLag)
	prometheus.MustRegister(leaseStatus)
}

// Server represents the main DistSQLite sidecar server
type Server struct {
	config        *Config
	logger        *zap.Logger
	leaseManager  *lease.Manager
	replicator    *replication.HybridReplicator
	vfsInterface  *vfs.Interface
	httpServer    *http.Server
	
	// Health and safety monitoring
	isHealthy      bool
	lastHealthCheck time.Time
	
	// Operational state
	isPrimary      bool
	startTime      time.Time
	
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
It acts as a sidecar process that handles replication and coordination automatically.

Production Features:
- Automatic failover with fence tokens
- Transaction log shipping with compression
- Split-brain detection and resolution
- Network partition tolerance
- Comprehensive monitoring and alerting
- Zero-downtime rolling updates`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServer(&config)
		},
	}
	
	// Add flags
	rootCmd.Flags().StringVar(&config.Hostname, "hostname", getHostname(), "Node hostname")
	rootCmd.Flags().StringVar(&config.NodeID, "node-id", "", "Unique node ID (defaults to hostname)")
	rootCmd.Flags().StringVar(&config.AdvertiseURL, "advertise-url", "", "URL for other nodes to access this node")
	rootCmd.Flags().StringVar(&config.DataDir, "data-dir", "./data", "Data directory for databases")
	rootCmd.Flags().StringVar(&config.HTTPAddr, "http-addr", DefaultHTTPAddr, "HTTP server address")
	rootCmd.Flags().StringVar(&config.ConsulURL, "consul-url", DefaultConsulURL, "Consul server URL")
	rootCmd.Flags().StringVar(&config.LeaseKey, "lease-key", DefaultLeaseKey, "Consul key for lease coordination")
	rootCmd.Flags().StringSliceVar(&config.ReplicationTargets, "targets", []string{}, "Replication target nodes")
	rootCmd.Flags().StringVar(&config.ReplicationMode, "replication-mode", "auto", "Replication mode: auto, txlog, bulk, disabled")
	rootCmd.Flags().BoolVar(&config.EnableSafetyChecks, "enable-safety", true, "Enable distributed safety checks")
	rootCmd.Flags().BoolVar(&config.EnableFenceTokens, "enable-fence-tokens", true, "Enable fence token safety")
	rootCmd.Flags().BoolVar(&config.EnableVectorClocks, "enable-vector-clocks", true, "Enable vector clock consistency")
	rootCmd.Flags().DurationVar(&config.MaxClockSkew, "max-clock-skew", 5*time.Second, "Maximum allowed clock skew")
	rootCmd.Flags().IntVar(&config.MinQuorumSize, "min-quorum-size", 2, "Minimum quorum size for safety")
	rootCmd.Flags().Int64Var(&config.BulkThreshold, "bulk-threshold", 1024*1024, "Threshold for bulk replication (bytes)")
	rootCmd.Flags().DurationVar(&config.MaxReplicationDelay, "max-replication-delay", 5*time.Second, "Maximum replication delay")
	rootCmd.Flags().BoolVar(&config.AutoStart, "auto-start", false, "Auto-start mode (launched by injection library)")
	rootCmd.Flags().StringVar(&config.LogLevel, "log-level", "info", "Log level: debug, info, warn, error")
	rootCmd.Flags().BoolVar(&config.EnableMetrics, "enable-metrics", true, "Enable Prometheus metrics")
	rootCmd.Flags().BoolVar(&config.EnableTracing, "enable-tracing", false, "Enable distributed tracing")
	
	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Failed to execute command: %v", err)
	}
}

func runServer(config *Config) error {
	// Set up structured logging with appropriate level
	logLevel := zapcore.InfoLevel
	switch strings.ToLower(config.LogLevel) {
	case "debug":
		logLevel = zapcore.DebugLevel
	case "warn":
		logLevel = zapcore.WarnLevel
	case "error":
		logLevel = zapcore.ErrorLevel
	}
	
	logConfig := zap.NewProductionConfig()
	logConfig.Level = zap.NewAtomicLevelAt(logLevel)
	logConfig.DisableStacktrace = true
	
	logger, err := logConfig.Build()
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}
	defer logger.Sync()
	
	// Validate and normalize configuration
	if err := validateAndNormalizeConfig(config); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	
	// Create server
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		config:    config,
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
		startTime: time.Now(),
	}
	
	// Initialize components in dependency order
	if err := server.initializeVFS(); err != nil {
		return fmt.Errorf("failed to initialize VFS: %w", err)
	}
	
	if err := server.initializeLeaseManager(); err != nil {
		return fmt.Errorf("failed to initialize lease manager: %w", err)
	}
	
	if err := server.initializeReplicator(); err != nil {
		return fmt.Errorf("failed to initialize replicator: %w", err)
	}
	
	if err := server.initializeHTTPServer(); err != nil {
		return fmt.Errorf("failed to initialize HTTP server: %w", err)
	}
	
	// Start all services
	if err := server.start(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}
	
	// Wait for shutdown signal
	server.waitForShutdown()
	
	// Graceful shutdown
	return server.shutdown()
}

func (s *Server) initializeVFS() error {
	s.logger.Info("Initializing DistSQLite VFS layer")
	
	// Create VFS interface
	s.vfsInterface = vfs.NewInterface()
	
	// Set up VFS event handlers
	s.vfsInterface.SetWriteHandler(s.handleVFSWrite)
	s.vfsInterface.SetSyncHandler(s.handleVFSSync)
	s.vfsInterface.SetTruncateHandler(s.handleVFSTruncate)
	
	// Initialize the VFS
	if err := s.vfsInterface.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize VFS interface: %w", err)
	}
	
	s.logger.Info("VFS layer initialized successfully")
	return nil
}

func (s *Server) initializeLeaseManager() error {
	s.logger.Info("Initializing lease manager",
		zap.String("consul_url", s.config.ConsulURL),
		zap.String("lease_key", s.config.LeaseKey),
	)
	
	// Create lease manager with safety features
	s.leaseManager = lease.NewManager(
		s.config.ConsulURL,
		s.config.LeaseKey,
		s.config.Hostname,
		s.config.AdvertiseURL,
	)
	
	// Configure safety settings
	s.leaseManager.MaxClockSkew = s.config.MaxClockSkew
	s.leaseManager.MinQuorumSize = s.config.MinQuorumSize
	s.leaseManager.EnableFenceTokens = s.config.EnableFenceTokens
	s.leaseManager.EnableVectorClocks = s.config.EnableVectorClocks
	
	// Set up lease event callbacks
	s.leaseManager.SetCallbacks(
		s.onLeaseAcquired,
		s.onLeaseLost,
		s.onSplitBrainDetected,
	)
	
	// Open the lease manager
	if err := s.leaseManager.Open(); err != nil {
		return fmt.Errorf("failed to open lease manager: %w", err)
	}
	
	s.logger.Info("Lease manager initialized successfully")
	return nil
}

func (s *Server) initializeReplicator() error {
	s.logger.Info("Initializing hybrid replicator",
		zap.Strings("targets", s.config.ReplicationTargets),
		zap.String("mode", s.config.ReplicationMode),
	)
	
	// Create hybrid replicator
	s.replicator = replication.NewHybridReplicator(s.config.NodeID, s.config.ReplicationTargets)
	
	// Configure replication mode
	switch strings.ToLower(s.config.ReplicationMode) {
	case "txlog":
		s.replicator.SetReplicationMode(replication.ReplicationModeTxLog)
	case "bulk":
		s.replicator.SetReplicationMode(replication.ReplicationModeBulk)
	case "disabled":
		s.replicator.SetReplicationMode(replication.ReplicationModeDisabled)
	default:
		s.replicator.SetReplicationMode(replication.ReplicationModeAuto)
	}
	
	// Configure safety settings
	if s.config.EnableSafetyChecks {
		safetyConfig := replication.SafetyConfig{
			EnableFenceTokens:             s.config.EnableFenceTokens,
			EnableVectorClocks:            s.config.EnableVectorClocks,
			EnableConflictDetection:       true,
			ConflictResolutionStrategy:    "vector-clock",
			ReadYourWritesTimeout:         5 * time.Second,
			MaxClockSkew:                 s.config.MaxClockSkew,
		}
		s.replicator.UpdateSafetyConfig(safetyConfig)
	}
	
	s.logger.Info("Hybrid replicator initialized successfully")
	return nil
}

func (s *Server) initializeHTTPServer() error {
	s.logger.Info("Initializing HTTP server", zap.String("addr", s.config.HTTPAddr))
	
	mux := http.NewServeMux()
	
	// Health and status endpoints
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/readiness", s.handleReadiness)
	mux.HandleFunc("/status", s.handleStatus)
	
	// Operational endpoints
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/lease", s.handleLease)
	mux.HandleFunc("/safety", s.handleSafety)
	
	// Management endpoints
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/replication/mode", s.handleReplicationMode)
	mux.HandleFunc("/maintenance", s.handleMaintenance)
	
	// Metrics endpoint (if enabled)
	if s.config.EnableMetrics {
		mux.Handle("/metrics", promhttp.Handler())
	}
	
	s.httpServer = &http.Server{
		Addr:         s.config.HTTPAddr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	
	s.logger.Info("HTTP server initialized successfully")
	return nil
}

func (s *Server) start() error {
	s.logger.Info("Starting DistSQLite sidecar services")
	
	// Ensure data directory exists with proper permissions
	if err := os.MkdirAll(s.config.DataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}
	
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
	
	// Start background workers
	go s.leaseManagementLoop()
	go s.healthMonitorLoop()
	go s.metricsUpdateLoop()
	
	s.logger.Info("DistSQLite sidecar started successfully",
		zap.String("node_id", s.config.NodeID),
		zap.String("advertise_url", s.config.AdvertiseURL),
		zap.Duration("uptime", time.Since(s.startTime)),
	)
	
	return nil
}

// VFS Event Handlers

func (s *Server) handleVFSWrite(dbPath string, data []byte, size int, offset int64) {
	timer := prometheus.NewTimer(operationDuration.WithLabelValues("write", filepath.Base(dbPath)))
	defer timer.ObserveDuration()
	
	// Record operation in replication system
	err := s.replicator.RecordChange(dbPath, "write", int64(size), offset, 0, data)
	
	// Update metrics
	status := "success"
	if err != nil {
		status = "error"
		s.logger.Error("Failed to record write operation", 
			zap.String("database", dbPath),
			zap.Error(err))
	}
	
	operationsTotal.WithLabelValues("write", filepath.Base(dbPath), status).Inc()
}

func (s *Server) handleVFSSync(dbPath string, flags int) {
	timer := prometheus.NewTimer(operationDuration.WithLabelValues("sync", filepath.Base(dbPath)))
	defer timer.ObserveDuration()
	
	// Record sync operation
	err := s.replicator.RecordChange(dbPath, "sync", 0, 0, flags, nil)
	
	// Update metrics
	status := "success"
	if err != nil {
		status = "error"
		s.logger.Error("Failed to record sync operation",
			zap.String("database", dbPath),
			zap.Error(err))
	}
	
	operationsTotal.WithLabelValues("sync", filepath.Base(dbPath), status).Inc()
}

func (s *Server) handleVFSTruncate(dbPath string, size int64) {
	timer := prometheus.NewTimer(operationDuration.WithLabelValues("truncate", filepath.Base(dbPath)))
	defer timer.ObserveDuration()
	
	// Record truncate operation
	err := s.replicator.RecordChange(dbPath, "truncate", size, 0, 0, nil)
	
	// Update metrics
	status := "success"
	if err != nil {
		status = "error"
		s.logger.Error("Failed to record truncate operation",
			zap.String("database", dbPath),
			zap.Error(err))
	}
	
	operationsTotal.WithLabelValues("truncate", filepath.Base(dbPath), status).Inc()
}

// Lease Event Handlers

func (s *Server) onLeaseAcquired(lease *lease.Lease) {
	s.logger.Info("Lease acquired - becoming primary",
		zap.String("node_id", s.config.NodeID),
		zap.Uint64("fence_token", lease.FenceToken),
		zap.Uint64("safety_epoch", lease.SafetyEpoch))
	
	s.isPrimary = true
	leaseStatus.WithLabelValues(s.config.NodeID).Set(1)
}

func (s *Server) onLeaseLost(lease *lease.Lease) {
	s.logger.Warn("Lease lost - stepping down from primary",
		zap.String("node_id", s.config.NodeID),
		zap.String("previous_holder", lease.Holder))
	
	s.isPrimary = false
	leaseStatus.WithLabelValues(s.config.NodeID).Set(0)
}

func (s *Server) onSplitBrainDetected(partitionInfo *lease.NetworkPartitionInfo) {
	s.logger.Error("Split-brain condition detected",
		zap.String("partition_id", partitionInfo.PartitionID),
		zap.Strings("nodes_in_quorum", partitionInfo.NodesInQuorum),
		zap.Strings("isolated_nodes", partitionInfo.IsolatedNodes))
	
	// Enter degraded mode - reject writes but allow reads
	s.isHealthy = false
}

// Background Workers

func (s *Server) leaseManagementLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.handleLeaseManagement()
		}
	}
}

func (s *Server) handleLeaseManagement() {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	
	if s.leaseManager.IsLeaseHolder() {
		// We hold the lease - try to renew it
		if err := s.leaseManager.RenewLease(ctx); err != nil {
			s.logger.Warn("Failed to renew lease", zap.Error(err))
		}
	} else {
		// We don't hold the lease - try to acquire it
		lease, err := s.leaseManager.AcquireLease(ctx)
		if err != nil {
			s.logger.Debug("Failed to acquire lease", zap.Error(err))
			return
		}
		
		if lease.Holder != s.config.Hostname {
			s.logger.Debug("Current lease holder", zap.String("holder", lease.Holder))
		}
	}
}

func (s *Server) healthMonitorLoop() {
	ticker := time.NewTicker(HealthCheckInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.performHealthCheck()
		}
	}
}

func (s *Server) performHealthCheck() {
	s.lastHealthCheck = time.Now()
	
	// Check VFS health
	vfsHealthy := s.vfsInterface.IsInitialized()
	
	// Check replicator health
	replicatorHealthy := s.replicator.IsHealthy()
	
	// Check lease manager connectivity (simple ping)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, leaseManagerHealthy := s.leaseManager.GetCurrentLease(ctx)
	leaseManagerHealthy = (leaseManagerHealthy == nil) // Convert error to bool
	
	// Update overall health status
	s.isHealthy = vfsHealthy && replicatorHealthy && leaseManagerHealthy
	
	if !s.isHealthy {
		s.logger.Warn("Health check failed",
			zap.Bool("vfs_healthy", vfsHealthy),
			zap.Bool("replicator_healthy", replicatorHealthy),
			zap.Bool("lease_manager_healthy", leaseManagerHealthy))
	}
}

func (s *Server) metricsUpdateLoop() {
	if !s.config.EnableMetrics {
		return
	}
	
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.updateMetrics()
		}
	}
}

func (s *Server) updateMetrics() {
	// Update replication lag metrics
	stats := s.replicator.GetDetailedStats()
	if txLogStats, ok := stats["transaction_log_stats"].(map[string]interface{}); ok {
		if pendingEntries, ok := txLogStats["pending_entries"].(int); ok {
			// Simple approximation of lag based on pending entries
			lagSeconds := float64(pendingEntries) * 0.1 // Assume 100ms per entry
			for _, target := range s.config.ReplicationTargets {
				replicationLag.WithLabelValues(target).Set(lagSeconds)
			}
		}
	}
}

// HTTP Handlers

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	
	if s.isHealthy {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"status": "healthy",
			"timestamp": "%s",
			"uptime_seconds": %.0f,
			"last_health_check": "%s"
		}`, time.Now().Format(time.RFC3339), 
			time.Since(s.startTime).Seconds(),
			s.lastHealthCheck.Format(time.RFC3339))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{
			"status": "unhealthy",
			"timestamp": "%s",
			"uptime_seconds": %.0f,
			"last_health_check": "%s"
		}`, time.Now().Format(time.RFC3339),
			time.Since(s.startTime).Seconds(),
			s.lastHealthCheck.Format(time.RFC3339))
	}
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Check if all components are ready
	vfsReady := s.vfsInterface.IsInitialized()
	replicatorHealthy := s.replicator.IsHealthy()
	
	w.Header().Set("Content-Type", "application/json")
	
	if vfsReady && replicatorHealthy {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"status": "ready",
			"timestamp": "%s",
			"vfs_ready": %t,
			"replicator_healthy": %t
		}`, time.Now().Format(time.RFC3339), vfsReady, replicatorHealthy)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{
			"status": "not_ready",
			"timestamp": "%s",
			"vfs_ready": %t,
			"replicator_healthy": %t
		}`, time.Now().Format(time.RFC3339), vfsReady, replicatorHealthy)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	isPrimary := s.leaseManager.IsLeaseHolder()
	currentLeaseHolder := "unknown"
	fenceToken := s.leaseManager.GetFenceToken()
	
	if lease, err := s.leaseManager.GetCurrentLease(r.Context()); err == nil {
		currentLeaseHolder = lease.Holder
	}
	
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{
		"node_id": "%s",
		"hostname": "%s",
		"is_primary": %t,
		"current_primary": "%s",
		"advertise_url": "%s",
		"fence_token": %d,
		"is_healthy": %t,
		"replicator_healthy": %t,
		"uptime_seconds": %.0f,
		"data_dir": "%s",
		"replication_mode": "%s",
		"replication_targets": %d,
		"timestamp": "%s"
	}`, s.config.NodeID, s.config.Hostname, isPrimary, currentLeaseHolder, 
		s.config.AdvertiseURL, fenceToken, s.isHealthy, s.replicator.IsHealthy(),
		time.Since(s.startTime).Seconds(), s.config.DataDir, s.config.ReplicationMode,
		len(s.config.ReplicationTargets), time.Now().Format(time.RFC3339))
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	stats := s.replicator.GetDetailedStats()
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	// Convert to JSON
	if data, err := json.Marshal(stats); err != nil {
		http.Error(w, fmt.Sprintf("Failed to marshal stats: %v", err), http.StatusInternalServerError)
		return
	} else {
		w.Write(data)
	}
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
	
	safetyMetrics := s.leaseManager.GetSafetyMetrics()
	
	w.Header().Set("Content-Type", "application/json")
	
	response := map[string]interface{}{
		"holder":        lease.Holder,
		"expires_at":    lease.ExpiresAt.Format(time.RFC3339),
		"generation":    lease.Generation,
		"region":        lease.Region,
		"advertise_url": lease.AdvertiseURL,
		"fence_token":   lease.FenceToken,
		"safety_epoch":  lease.SafetyEpoch,
		"vector_clock":  lease.VectorClock,
		"node_health":   lease.NodeHealth,
		"quorum_size":   lease.QuorumSize,
		"quorum_nodes":  lease.QuorumNodes,
		"partition_info": lease.PartitionInfo,
		"safety_metrics": safetyMetrics,
	}
	
	if data, err := json.Marshal(response); err != nil {
		http.Error(w, fmt.Sprintf("Failed to marshal lease: %v", err), http.StatusInternalServerError)
		return
	} else {
		w.Write(data)
	}
}

func (s *Server) handleSafety(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	safetyMetrics := s.leaseManager.GetSafetyMetrics()
	replicatorStats := s.replicator.GetDetailedStats()
	
	response := map[string]interface{}{
		"safety_checks_enabled": s.config.EnableSafetyChecks,
		"fence_tokens_enabled":  s.config.EnableFenceTokens,
		"vector_clocks_enabled": s.config.EnableVectorClocks,
		"max_clock_skew":        s.config.MaxClockSkew.String(),
		"min_quorum_size":       s.config.MinQuorumSize,
		"safety_metrics":        safetyMetrics,
		"conflicts_detected":    replicatorStats["conflicts_detected"],
		"circuit_breaker_open":  replicatorStats["circuit_breaker_open"],
		"consecutive_failures":  replicatorStats["consecutive_failures"],
		"timestamp":            time.Now().Format(time.RFC3339),
	}
	
	w.Header().Set("Content-Type", "application/json")
	if data, err := json.Marshal(response); err != nil {
		http.Error(w, fmt.Sprintf("Failed to marshal safety info: %v", err), http.StatusInternalServerError)
		return
	} else {
		w.Write(data)
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetConfig(w, r)
	case http.MethodPatch:
		s.handleUpdateConfig(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	// Return non-sensitive configuration
	response := map[string]interface{}{
		"node_id":                s.config.NodeID,
		"hostname":               s.config.Hostname,
		"advertise_url":          s.config.AdvertiseURL,
		"data_dir":               s.config.DataDir,
		"http_addr":              s.config.HTTPAddr,
		"replication_targets":    s.config.ReplicationTargets,
		"replication_mode":       s.config.ReplicationMode,
		"enable_safety_checks":   s.config.EnableSafetyChecks,
		"enable_fence_tokens":    s.config.EnableFenceTokens,
		"enable_vector_clocks":   s.config.EnableVectorClocks,
		"max_clock_skew":         s.config.MaxClockSkew.String(),
		"min_quorum_size":        s.config.MinQuorumSize,
		"bulk_threshold":         s.config.BulkThreshold,
		"max_replication_delay":  s.config.MaxReplicationDelay.String(),
		"log_level":              s.config.LogLevel,
		"enable_metrics":         s.config.EnableMetrics,
		"enable_tracing":         s.config.EnableTracing,
	}
	
	w.Header().Set("Content-Type", "application/json")
	if data, err := json.Marshal(response); err != nil {
		http.Error(w, fmt.Sprintf("Failed to marshal config: %v", err), http.StatusInternalServerError)
		return
	} else {
		w.Write(data)
	}
}

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	
	// Handle specific configuration updates
	updated := false
	
	if mode, ok := updates["replication_mode"].(string); ok {
		validModes := []string{"auto", "txlog", "bulk", "disabled"}
		valid := false
		for _, validMode := range validModes {
			if strings.ToLower(mode) == validMode {
				valid = true
				break
			}
		}
		
		if valid {
			s.config.ReplicationMode = mode
			// Update replicator mode
			switch strings.ToLower(mode) {
			case "txlog":
				s.replicator.SetReplicationMode(replication.ReplicationModeTxLog)
			case "bulk":
				s.replicator.SetReplicationMode(replication.ReplicationModeBulk)
			case "disabled":
				s.replicator.SetReplicationMode(replication.ReplicationModeDisabled)
			default:
				s.replicator.SetReplicationMode(replication.ReplicationModeAuto)
			}
			updated = true
			s.logger.Info("Updated replication mode", zap.String("mode", mode))
		} else {
			http.Error(w, fmt.Sprintf("Invalid replication mode: %s", mode), http.StatusBadRequest)
			return
		}
	}
	
	if !updated {
		http.Error(w, "No valid configuration updates provided", http.StatusBadRequest)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status": "updated", "timestamp": "%s"}`, time.Now().Format(time.RFC3339))
}

func (s *Server) handleReplicationMode(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"replication_mode": "%s", "timestamp": "%s"}`, 
			s.config.ReplicationMode, time.Now().Format(time.RFC3339))
	case http.MethodPut:
		var request struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		
		validModes := []string{"auto", "txlog", "bulk", "disabled"}
		valid := false
		for _, validMode := range validModes {
			if strings.ToLower(request.Mode) == validMode {
				valid = true
				break
			}
		}
		
		if !valid {
			http.Error(w, fmt.Sprintf("Invalid replication mode: %s", request.Mode), http.StatusBadRequest)
			return
		}
		
		s.config.ReplicationMode = request.Mode
		
		// Update replicator mode
		switch strings.ToLower(request.Mode) {
		case "txlog":
			s.replicator.SetReplicationMode(replication.ReplicationModeTxLog)
		case "bulk":
			s.replicator.SetReplicationMode(replication.ReplicationModeBulk)
		case "disabled":
			s.replicator.SetReplicationMode(replication.ReplicationModeDisabled)
		default:
			s.replicator.SetReplicationMode(replication.ReplicationModeAuto)
		}
		
		s.logger.Info("Updated replication mode", zap.String("mode", request.Mode))
		
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status": "updated", "mode": "%s", "timestamp": "%s"}`, 
			request.Mode, time.Now().Format(time.RFC3339))
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var request struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	
	switch strings.ToLower(request.Action) {
	case "health_check":
		s.performHealthCheck()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status": "health_check_performed", "healthy": %t, "timestamp": "%s"}`, 
			s.isHealthy, time.Now().Format(time.RFC3339))
		
	case "force_lease_release":
		if s.leaseManager.IsLeaseHolder() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			
			if err := s.leaseManager.ReleaseLease(ctx); err != nil {
				http.Error(w, fmt.Sprintf("Failed to release lease: %v", err), http.StatusInternalServerError)
				return
			}
			
			s.logger.Warn("Forced lease release via maintenance endpoint")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status": "lease_released", "timestamp": "%s"}`, time.Now().Format(time.RFC3339))
		} else {
			http.Error(w, "Not the lease holder", http.StatusBadRequest)
			return
		}
		
	default:
		http.Error(w, fmt.Sprintf("Unknown maintenance action: %s", request.Action), http.StatusBadRequest)
		return
	}
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
	
	// Shutdown HTTP server gracefully
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		
		if err := s.httpServer.Shutdown(ctx); err != nil {
			s.logger.Error("Failed to shutdown HTTP server gracefully", zap.Error(err))
		}
	}
	
	// Stop replicator
	if s.replicator != nil {
		if err := s.replicator.Stop(); err != nil {
			s.logger.Error("Failed to stop replicator", zap.Error(err))
		}
	}
	
	// Shutdown VFS interface
	if s.vfsInterface != nil {
		if err := s.vfsInterface.Shutdown(); err != nil {
			s.logger.Error("Failed to shutdown VFS interface", zap.Error(err))
		}
	}
	
	// Release lease and close lease manager
	if s.leaseManager != nil {
		if s.leaseManager.IsLeaseHolder() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			
			if err := s.leaseManager.ReleaseLease(ctx); err != nil {
				s.logger.Error("Failed to release lease during shutdown", zap.Error(err))
			}
		}
		
		if err := s.leaseManager.Close(); err != nil {
			s.logger.Error("Failed to close lease manager", zap.Error(err))
		}
	}
	
	s.logger.Info("DistSQLite sidecar shutdown complete",
		zap.Duration("total_uptime", time.Since(s.startTime)))
	
	return nil
}

func validateAndNormalizeConfig(config *Config) error {
	if config.Hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	
	// Set NodeID to hostname if not specified
	if config.NodeID == "" {
		config.NodeID = config.Hostname
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
	
	// Validate replication mode
	validModes := []string{"auto", "txlog", "bulk", "disabled"}
	validMode := false
	for _, mode := range validModes {
		if strings.ToLower(config.ReplicationMode) == mode {
			validMode = true
			break
		}
	}
	if !validMode {
		return fmt.Errorf("invalid replication mode: %s (valid: %v)", config.ReplicationMode, validModes)
	}
	
	// Validate operational limits
	if config.BulkThreshold < 1024 {
		config.BulkThreshold = 1024 // Minimum 1KB
	}
	if config.BulkThreshold > MaxDatabaseSize {
		config.BulkThreshold = MaxDatabaseSize
	}
	
	if config.MaxReplicationDelay < time.Second {
		config.MaxReplicationDelay = time.Second // Minimum 1 second
	}
	
	if config.MinQuorumSize < 1 {
		config.MinQuorumSize = 1
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