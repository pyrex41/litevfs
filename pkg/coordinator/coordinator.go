package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
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
	
	// Configuration
	nodeTimeout    time.Duration
	cleanupTicker  *time.Ticker
	ctx            context.Context
	cancel         context.CancelFunc
}

// NewCoordinator creates a new coordinator instance
func NewCoordinator() *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	
	c := &Coordinator{
		nodes:           make(map[string]*Node),
		leaseEvents:     make([]LeaseEvent, 0),
		replicationJobs: make(map[string]*ReplicationRequest),
		nodeTimeout:     30 * time.Second,
		ctx:             ctx,
		cancel:          cancel,
	}
	
	// Start background cleanup
	c.cleanupTicker = time.NewTicker(10 * time.Second)
	go c.cleanupLoop()
	
	return c
}

// Start begins the coordinator HTTP server
func (c *Coordinator) Start(addr string) error {
	mux := http.NewServeMux()
	
	// Cluster management endpoints
	mux.HandleFunc("/notify", c.handleNotify)
	mux.HandleFunc("/nodes", c.handleNodes)
	mux.HandleFunc("/cluster", c.handleCluster)
	
	// Replication coordination endpoints
	mux.HandleFunc("/replicate", c.handleReplication)
	mux.HandleFunc("/replication/queue", c.handleReplicationQueue)
	mux.HandleFunc("/replication/status", c.handleReplicationStatus)
	
	// Health and status
	mux.HandleFunc("/health", c.handleHealth)
	mux.HandleFunc("/status", c.handleStatus)
	
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	
	log.Printf("[DistSQLite Coordinator] Starting on %s", addr)
	return server.ListenAndServe()
}

// Stop shuts down the coordinator
func (c *Coordinator) Stop() error {
	c.cancel()
	if c.cleanupTicker != nil {
		c.cleanupTicker.Stop()
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
		"service":        "distsqlite-coordinator",
		"version":        "1.0.0",
		"nodes":          len(c.nodes),
		"events":         len(c.leaseEvents),
		"pending_jobs":   len(c.replicationJobs),
		"uptime_seconds": int(time.Since(time.Now()).Seconds()),
		"timestamp":      time.Now().Format(time.RFC3339),
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