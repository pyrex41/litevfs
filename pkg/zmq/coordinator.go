package zmq

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Coordinator manages cluster state and lease coordination via ZMQ
type Coordinator struct {
	transport *Transport
	leases    *LeaseStore
	nodes     *NodeRegistry
	
	ctx    context.Context
	cancel context.CancelFunc
}

// LeaseStore manages in-memory lease state
type LeaseStore struct {
	mu     sync.RWMutex
	leases map[string]*LeaseState // dbPath -> lease state
}

// LeaseState represents the state of a database lease
type LeaseState struct {
	DBPath      string    `json:"db_path"`
	NodeID      string    `json:"node_id"`
	FenceToken  int64     `json:"fence_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	AcquiredAt  time.Time `json:"acquired_at"`
	RenewCount  int64     `json:"renew_count"`
}

// NodeRegistry tracks active cluster nodes
type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeInfo // nodeID -> node info
}

// NodeInfo represents information about a cluster node
type NodeInfo struct {
	NodeID      string            `json:"node_id"`
	Hostname    string            `json:"hostname"`
	Endpoints   map[string]string `json:"endpoints"`
	LastSeen    time.Time         `json:"last_seen"`
	Status      string            `json:"status"`
	Databases   []string          `json:"databases"`
}

// NewCoordinator creates a new ZMQ coordinator
func NewCoordinator(config *TransportConfig, nodeID string) (*Coordinator, error) {
	transport, err := NewTransport(config, nodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to create transport: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	c := &Coordinator{
		transport: transport,
		leases:    NewLeaseStore(),
		nodes:     NewNodeRegistry(),
		ctx:       ctx,
		cancel:    cancel,
	}

	return c, nil
}

// Start begins coordinator operations
func (c *Coordinator) Start() error {
	// Start transport in coordinator mode
	if err := c.transport.StartCoordinator(); err != nil {
		return fmt.Errorf("failed to start coordinator transport: %w", err)
	}

	// Start lease service
	go func() {
		if err := c.transport.ServeLease(c.ctx, c.leases); err != nil {
			log.Printf("[ZMQ Coordinator] Lease service error: %v", err)
		}
	}()

	// Start lease expiration cleanup
	go c.cleanupExpiredLeases()

	// Start node health monitoring
	go c.monitorNodes()

	log.Printf("[ZMQ Coordinator] Coordinator started successfully")
	return nil
}

// Stop shuts down the coordinator
func (c *Coordinator) Stop() error {
	c.cancel()
	return c.transport.Close()
}

// GetTransport returns the underlying transport for additional functionality
func (c *Coordinator) GetTransport() *Transport {
	return c.transport
}

// cleanupExpiredLeases periodically removes expired leases
func (c *Coordinator) cleanupExpiredLeases() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.leases.CleanupExpired()
		}
	}
}

// monitorNodes handles node health monitoring
func (c *Coordinator) monitorNodes() {
	// TODO: Implement node monitoring via ROUTER socket
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.nodes.CleanupInactive(30 * time.Second)
		}
	}
}

// LeaseStore implementation

// NewLeaseStore creates a new lease store
func NewLeaseStore() *LeaseStore {
	return &LeaseStore{
		leases: make(map[string]*LeaseState),
	}
}

// AcquireLease implements LeaseManager interface
func (ls *LeaseStore) AcquireLease(nodeID, dbPath string, ttl int64) *LeaseResponse {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	now := time.Now()

	// Check if lease exists and is still valid
	if lease, exists := ls.leases[dbPath]; exists {
		if now.Before(lease.ExpiresAt) {
			// Lease is still active
			if lease.NodeID == nodeID {
				// Same node requesting - extend the lease
				lease.ExpiresAt = now.Add(time.Duration(ttl) * time.Second)
				lease.RenewCount++
				
				log.Printf("[ZMQ Coordinator] Extended lease for %s to %s (token: %d)", 
					nodeID, dbPath, lease.FenceToken)
				
				return NewLeaseResponse(true, lease.FenceToken, lease.ExpiresAt.Unix(), 
					lease.NodeID, "")
			} else {
				// Different node requesting active lease
				return NewLeaseResponse(false, lease.FenceToken, lease.ExpiresAt.Unix(), 
					lease.NodeID, fmt.Sprintf("lease held by %s until %v", 
					lease.NodeID, lease.ExpiresAt))
			}
		}
		// Lease has expired, will be replaced below
	}

	// Create new lease
	fenceToken := now.UnixNano() // Use timestamp as fence token for uniqueness
	expiresAt := now.Add(time.Duration(ttl) * time.Second)

	newLease := &LeaseState{
		DBPath:     dbPath,
		NodeID:     nodeID,
		FenceToken: fenceToken,
		ExpiresAt:  expiresAt,
		AcquiredAt: now,
		RenewCount: 0,
	}

	ls.leases[dbPath] = newLease

	log.Printf("[ZMQ Coordinator] Granted lease for %s to %s (token: %d, expires: %v)", 
		dbPath, nodeID, fenceToken, expiresAt)

	return NewLeaseResponse(true, fenceToken, expiresAt.Unix(), nodeID, "")
}

// RenewLease implements LeaseManager interface
func (ls *LeaseStore) RenewLease(nodeID, dbPath string, ttl int64) *LeaseResponse {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	lease, exists := ls.leases[dbPath]
	if !exists {
		return NewLeaseResponse(false, 0, 0, "", "no lease exists for database")
	}

	if lease.NodeID != nodeID {
		return NewLeaseResponse(false, lease.FenceToken, lease.ExpiresAt.Unix(), 
			lease.NodeID, fmt.Sprintf("lease held by different node: %s", lease.NodeID))
	}

	// Renew the lease
	now := time.Now()
	lease.ExpiresAt = now.Add(time.Duration(ttl) * time.Second)
	lease.RenewCount++

	log.Printf("[ZMQ Coordinator] Renewed lease for %s by %s (token: %d)", 
		dbPath, nodeID, lease.FenceToken)

	return NewLeaseResponse(true, lease.FenceToken, lease.ExpiresAt.Unix(), nodeID, "")
}

// ReleaseLease implements LeaseManager interface
func (ls *LeaseStore) ReleaseLease(nodeID, dbPath string) *LeaseResponse {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	lease, exists := ls.leases[dbPath]
	if !exists {
		return NewLeaseResponse(false, 0, 0, "", "no lease exists for database")
	}

	if lease.NodeID != nodeID {
		return NewLeaseResponse(false, lease.FenceToken, lease.ExpiresAt.Unix(), 
			lease.NodeID, fmt.Sprintf("cannot release lease held by different node: %s", lease.NodeID))
	}

	log.Printf("[ZMQ Coordinator] Released lease for %s by %s (token: %d)", 
		dbPath, nodeID, lease.FenceToken)

	delete(ls.leases, dbPath)
	return NewLeaseResponse(true, 0, 0, "", "")
}

// GetLeaseStatus implements LeaseManager interface
func (ls *LeaseStore) GetLeaseStatus(dbPath string) *LeaseResponse {
	ls.mu.RLock()
	defer ls.mu.RUnlock()

	lease, exists := ls.leases[dbPath]
	if !exists {
		return NewLeaseResponse(false, 0, 0, "", "no active lease")
	}

	// Check if lease is expired
	if time.Now().After(lease.ExpiresAt) {
		return NewLeaseResponse(false, 0, 0, "", "lease expired")
	}

	return NewLeaseResponse(true, lease.FenceToken, lease.ExpiresAt.Unix(), 
		lease.NodeID, "")
}

// CleanupExpired removes expired leases
func (ls *LeaseStore) CleanupExpired() {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	now := time.Now()
	var expired []string

	for dbPath, lease := range ls.leases {
		if now.After(lease.ExpiresAt) {
			expired = append(expired, dbPath)
		}
	}

	for _, dbPath := range expired {
		lease := ls.leases[dbPath]
		log.Printf("[ZMQ Coordinator] Expired lease for %s (was held by %s)", 
			dbPath, lease.NodeID)
		delete(ls.leases, dbPath)
	}
}

// GetAllLeases returns all current leases (for debugging/monitoring)
func (ls *LeaseStore) GetAllLeases() map[string]*LeaseState {
	ls.mu.RLock()
	defer ls.mu.RUnlock()

	result := make(map[string]*LeaseState)
	for k, v := range ls.leases {
		// Create copy to avoid race conditions
		result[k] = &LeaseState{
			DBPath:     v.DBPath,
			NodeID:     v.NodeID,
			FenceToken: v.FenceToken,
			ExpiresAt:  v.ExpiresAt,
			AcquiredAt: v.AcquiredAt,
			RenewCount: v.RenewCount,
		}
	}
	return result
}

// NodeRegistry implementation

// NewNodeRegistry creates a new node registry
func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{
		nodes: make(map[string]*NodeInfo),
	}
}

// RegisterNode registers a new node in the cluster
func (nr *NodeRegistry) RegisterNode(nodeID, hostname string) {
	nr.mu.Lock()
	defer nr.mu.Unlock()

	node := &NodeInfo{
		NodeID:    nodeID,
		Hostname:  hostname,
		Endpoints: make(map[string]string),
		LastSeen:  time.Now(),
		Status:    "active",
		Databases: []string{},
	}

	nr.nodes[nodeID] = node
	log.Printf("[ZMQ Coordinator] Registered node: %s (%s)", nodeID, hostname)
}

// UpdateNodeHeartbeat updates node last seen timestamp
func (nr *NodeRegistry) UpdateNodeHeartbeat(nodeID string) {
	nr.mu.Lock()
	defer nr.mu.Unlock()

	if node, exists := nr.nodes[nodeID]; exists {
		node.LastSeen = time.Now()
		node.Status = "active"
	}
}

// CleanupInactive removes nodes that haven't been seen recently
func (nr *NodeRegistry) CleanupInactive(timeout time.Duration) {
	nr.mu.Lock()
	defer nr.mu.Unlock()

	now := time.Now()
	var inactive []string

	for nodeID, node := range nr.nodes {
		if now.Sub(node.LastSeen) > timeout {
			inactive = append(inactive, nodeID)
		}
	}

	for _, nodeID := range inactive {
		log.Printf("[ZMQ Coordinator] Removing inactive node: %s", nodeID)
		delete(nr.nodes, nodeID)
	}
}

// GetActiveNodes returns list of active nodes
func (nr *NodeRegistry) GetActiveNodes() []string {
	nr.mu.RLock()
	defer nr.mu.RUnlock()

	var active []string
	for nodeID, node := range nr.nodes {
		if node.Status == "active" {
			active = append(active, nodeID)
		}
	}
	return active
}