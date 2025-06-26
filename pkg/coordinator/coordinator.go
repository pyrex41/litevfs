package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"distsqlite/pkg/rsync"
	"distsqlite/pkg/leaseserver"
)

// Node represents a cluster node
type Node struct {
	Hostname     string            `json:"hostname"`
	AdvertiseURL string            `json:"advertise_url"`
	LastSeen     time.Time         `json:"last_seen"`
	Status       string            `json:"status"` // "active", "inactive", "unknown"
	Databases    map[string]string `json:"databases"` // db_path -> role (primary/replica)
}

// LeaseEvent represents a lease state change
type LeaseEvent struct {
	Event    string      `json:"event"`    // "acquired", "released", "renewed"
	Hostname string      `json:"hostname"`
	Lease    interface{} `json:"lease"`
	Time     time.Time   `json:"time"`
}

// ReplicationRequest represents a replication job
type ReplicationRequest struct {
	ID         string    `json:"id"`
	SourceNode string    `json:"source_node"`
	TargetNode string    `json:"target_node"`
	Database   string    `json:"database"`
	Strategy   string    `json:"strategy"` // "rsync", "txlog"
	Priority   int       `json:"priority"`
	CreatedAt  time.Time `json:"created_at"`
	Status     string    `json:"status"` // "pending", "in_progress", "completed", "failed"
}

// Coordinator manages cluster state and coordinates replication
type Coordinator struct {
	mu             sync.RWMutex
	nodes          map[string]*Node
	leaseEvents    []LeaseEvent
	replicationJobs map[string]*ReplicationRequest
	
	// HTTP rsync servers (per database)
	httpRsyncServers map[string]*rsync.HTTPRsyncServer
	
	// Centralized lease server
	leaseServer    *leaseserver.LeaseServer
	leaseServerPort int
	
	// Configuration
	nodeTimeout    time.Duration
	cleanupTicker  *time.Ticker
	ctx            context.Context
	cancel         context.CancelFunc
}

// NewCoordinator creates a new coordinator instance
func NewCoordinator(leaseServerPort int, dbPath string) *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	
	c := &Coordinator{
		nodes:            make(map[string]*Node),
		leaseEvents:      make([]LeaseEvent, 0),
		replicationJobs:  make(map[string]*ReplicationRequest),
		httpRsyncServers: make(map[string]*rsync.HTTPRsyncServer),
		leaseServerPort:  leaseServerPort,
		nodeTimeout:      30 * time.Second,
		ctx:              ctx,
		cancel:           cancel,
	}
	
	// Create embedded lease server
	c.leaseServer = leaseserver.NewLeaseServer(leaseServerPort, dbPath)
	
	// Start background cleanup
	c.cleanupTicker = time.NewTicker(10 * time.Second)
	go c.cleanupLoop()
	
	return c
}

// Start begins the coordinator HTTP server
func (c *Coordinator) Start(addr string) error {
	// Start embedded lease server first
	go func() {
		log.Printf("[DistSQLite Coordinator] Starting embedded lease server on port %d", c.leaseServerPort)
		if err := c.leaseServer.Start(); err != nil {
			log.Printf("[DistSQLite Coordinator] Lease server error: %v", err)
		}
	}()
	
	// Give lease server time to start
	time.Sleep(100 * time.Millisecond)
	
	mux := http.NewServeMux()
	
	// Cluster management endpoints
	mux.HandleFunc("/notify", c.handleNotify)
	mux.HandleFunc("/nodes", c.handleNodes)
	mux.HandleFunc("/cluster", c.handleCluster)
	
	// Replication coordination endpoints
	mux.HandleFunc("/replicate", c.handleReplication)
	mux.HandleFunc("/replication/queue", c.handleReplicationQueue)
	mux.HandleFunc("/replication/status", c.handleReplicationStatus)
	
	// HTTP rsync endpoints
	mux.HandleFunc("/rsync", c.handleHTTPRsync)
	mux.HandleFunc("/rsync/", c.handleHTTPRsync)
	
	// Lease server proxy endpoints
	mux.HandleFunc("/lease/", c.handleLeaseProxy)
	
	// Health and status
	mux.HandleFunc("/health", c.handleHealth)
	mux.HandleFunc("/status", c.handleStatus)
	
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	
	log.Printf("[DistSQLite Coordinator] Starting on %s", addr)
	log.Printf("[DistSQLite Coordinator] Lease server available at :%d", c.leaseServerPort)
	return server.ListenAndServe()
}

// Stop shuts down the coordinator
func (c *Coordinator) Stop() error {
	c.cancel()
	if c.cleanupTicker != nil {
		c.cleanupTicker.Stop()
	}
	if c.leaseServer != nil {
		c.leaseServer.Stop()
	}
	return nil
}

// handleNotify processes lease events from nodes
func (c *Coordinator) handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var event LeaseEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	
	event.Time = time.Now()
	
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// Update node status
	node, exists := c.nodes[event.Hostname]
	if !exists {
		node = &Node{
			Hostname:  event.Hostname,
			Databases: make(map[string]string),
		}
		c.nodes[event.Hostname] = node
	}
	
	node.LastSeen = time.Now()
	node.Status = "active"
	
	// Store lease event
	c.leaseEvents = append(c.leaseEvents, event)
	
	// Keep only last 1000 events
	if len(c.leaseEvents) > 1000 {
		c.leaseEvents = c.leaseEvents[len(c.leaseEvents)-1000:]
	}
	
	log.Printf("[DistSQLite Coordinator] Lease event: %s %s", event.Event, event.Hostname)
	
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status": "ok", "timestamp": "%s"}`, time.Now().Format(time.RFC3339))
}

// handleNodes returns cluster node information
func (c *Coordinator) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"nodes":     c.nodes,
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// handleCluster returns overall cluster status
func (c *Coordinator) handleCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	activeNodes := 0
	inactiveNodes := 0
	
	for _, node := range c.nodes {
		if node.Status == "active" {
			activeNodes++
		} else {
			inactiveNodes++
		}
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_nodes":    len(c.nodes),
		"active_nodes":   activeNodes,
		"inactive_nodes": inactiveNodes,
		"recent_events":  len(c.leaseEvents),
		"pending_jobs":   len(c.replicationJobs),
		"timestamp":      time.Now().Format(time.RFC3339),
	})
}

// handleReplication handles replication requests
func (c *Coordinator) handleReplication(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		c.handleReplicationRequest(w, r)
	case http.MethodGet:
		c.handleReplicationQuery(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleReplicationRequest creates a new replication job
func (c *Coordinator) handleReplicationRequest(w http.ResponseWriter, r *http.Request) {
	var req ReplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	
	// Generate ID and set defaults
	req.ID = fmt.Sprintf("repl_%d", time.Now().UnixNano())
	req.CreatedAt = time.Now()
	req.Status = "pending"
	
	if req.Strategy == "" {
		req.Strategy = "rsync" // Default strategy
	}
	
	c.mu.Lock()
	c.replicationJobs[req.ID] = &req
	c.mu.Unlock()
	
	log.Printf("[DistSQLite Coordinator] Replication job created: %s (%s -> %s)", 
		req.ID, req.SourceNode, req.TargetNode)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":        req.ID,
		"status":    "created",
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// handleReplicationQuery returns pending replication work for a node
func (c *Coordinator) handleReplicationQuery(w http.ResponseWriter, r *http.Request) {
	hostname := r.URL.Query().Get("hostname")
	if hostname == "" {
		http.Error(w, "hostname parameter required", http.StatusBadRequest)
		return
	}
	
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	var jobs []*ReplicationRequest
	for _, job := range c.replicationJobs {
		if job.Status == "pending" && (job.SourceNode == hostname || job.TargetNode == hostname) {
			jobs = append(jobs, job)
		}
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"jobs":      jobs,
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// handleReplicationQueue returns all replication jobs
func (c *Coordinator) handleReplicationQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"jobs":      c.replicationJobs,
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// handleReplicationStatus updates replication job status
func (c *Coordinator) handleReplicationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	jobID := r.URL.Query().Get("id")
	if jobID == "" {
		http.Error(w, "id parameter required", http.StatusBadRequest)
		return
	}
	
	var update struct {
		Status string `json:"status"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	
	c.mu.Lock()
	defer c.mu.Unlock()
	
	job, exists := c.replicationJobs[jobID]
	if !exists {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	
	job.Status = update.Status
	
	log.Printf("[DistSQLite Coordinator] Replication job %s status: %s", jobID, update.Status)
	
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status": "updated", "timestamp": "%s"}`, time.Now().Format(time.RFC3339))
}

// handleHealth returns coordinator health
func (c *Coordinator) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status": "healthy", "timestamp": "%s"}`, time.Now().Format(time.RFC3339))
}

// handleHTTPRsync handles HTTP rsync requests
func (c *Coordinator) handleHTTPRsync(w http.ResponseWriter, r *http.Request) {
	// Extract database path from query or header
	dbPath := r.URL.Query().Get("db")
	if dbPath == "" {
		dbPath = r.Header.Get("X-Database-Path")
	}
	if dbPath == "" {
		dbPath = "default.db" // Default database
	}
	
	// Get or create HTTP rsync server for this database
	c.mu.Lock()
	server, exists := c.httpRsyncServers[dbPath]
	if !exists {
		server = rsync.NewHTTPRsyncServer(dbPath)
		c.httpRsyncServers[dbPath] = server
	}
	c.mu.Unlock()
	
	// Delegate to the HTTP rsync server
	server.HandleRsync(w, r)
}

// handleLeaseProxy proxies lease requests to the embedded lease server
func (c *Coordinator) handleLeaseProxy(w http.ResponseWriter, r *http.Request) {
	// Forward requests to embedded lease server
	proxyURL := fmt.Sprintf("http://localhost:%d%s", c.leaseServerPort, r.URL.Path)
	
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(r.Method, proxyURL, r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Proxy error: %v", err), http.StatusInternalServerError)
		return
	}
	
	// Copy headers
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Lease server unavailable: %v", err), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	
	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	
	w.WriteHeader(resp.StatusCode)
	
	// Copy response body
	if resp.Body != nil {
		_, err = io.Copy(w, resp.Body)
		if err != nil {
			log.Printf("Error copying lease response: %v", err)
		}
	}
}

// handleStatus returns detailed coordinator status
func (c *Coordinator) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"service":         "distsqlite-coordinator",
		"version":         "1.0.0",
		"nodes":           len(c.nodes),
		"events":          len(c.leaseEvents),
		"pending_jobs":    len(c.replicationJobs),
		"rsync_servers":   len(c.httpRsyncServers),
		"lease_server_port": c.leaseServerPort,
		"uptime_seconds":  int(time.Since(time.Now()).Seconds()),
		"timestamp":       time.Now().Format(time.RFC3339),
	})
}

// cleanupLoop periodically cleans up inactive nodes and old jobs
func (c *Coordinator) cleanupLoop() {
	for {
		select {
		case <-c.cleanupTicker.C:
			c.cleanup()
		case <-c.ctx.Done():
			return
		}
	}
}

// cleanup removes inactive nodes and completed jobs
func (c *Coordinator) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	now := time.Now()
	
	// Mark nodes as inactive if not seen recently
	for hostname, node := range c.nodes {
		if now.Sub(node.LastSeen) > c.nodeTimeout {
			if node.Status == "active" {
				node.Status = "inactive"
				log.Printf("[DistSQLite Coordinator] Node %s marked inactive", hostname)
			}
		}
	}
	
	// Remove completed/failed jobs older than 1 hour
	for id, job := range c.replicationJobs {
		if (job.Status == "completed" || job.Status == "failed") && 
			now.Sub(job.CreatedAt) > time.Hour {
			delete(c.replicationJobs, id)
		}
	}
}