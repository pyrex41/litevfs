package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/consul/api"
)

// Default lease settings - borrowed from LiteFS proven values
const (
	DefaultSessionName         = "distsqlite"
	DefaultTTL                = 10 * time.Second
	DefaultLockDelay          = 1 * time.Second
	DefaultHeartbeatInterval  = 3 * time.Second
	DefaultMaxClockSkew       = 5 * time.Second
	DefaultSplitBrainDetectionInterval = 30 * time.Second
	DefaultMinQuorumSize      = 2
)

// VectorClock represents a vector clock for distributed ordering
type VectorClock map[string]uint64

// Clone creates a copy of the vector clock
func (vc VectorClock) Clone() VectorClock {
	clone := make(VectorClock)
	for k, v := range vc {
		clone[k] = v
	}
	return clone
}

// Update increments the clock for the given node
func (vc VectorClock) Update(nodeID string) {
	vc[nodeID]++
}

// Merge merges another vector clock into this one
func (vc VectorClock) Merge(other VectorClock) {
	for nodeID, timestamp := range other {
		if vc[nodeID] < timestamp {
			vc[nodeID] = timestamp
		}
	}
}

// IsAfter returns true if this clock is after the other
func (vc VectorClock) IsAfter(other VectorClock) bool {
	hasGreater := false
	for nodeID, timestamp := range vc {
		if otherTimestamp, exists := other[nodeID]; exists {
			if timestamp < otherTimestamp {
				return false
			}
			if timestamp > otherTimestamp {
				hasGreater = true
			}
		}
	}
	return hasGreater
}

// NetworkPartitionInfo contains information about detected network partitions
type NetworkPartitionInfo struct {
	DetectedAt    time.Time `json:"detected_at"`
	PartitionID   string    `json:"partition_id"`
	NodesInQuorum []string  `json:"nodes_in_quorum"`
	IsolatedNodes []string  `json:"isolated_nodes"`
}

// SafetyMetrics contains safety-related metrics
type SafetyMetrics struct {
	FenceTokenViolations    uint64    `json:"fence_token_violations"`
	ClockSkewDetections     uint64    `json:"clock_skew_detections"`
	SplitBrainDetections    uint64    `json:"split_brain_detections"`
	AutomaticDemotions      uint64    `json:"automatic_demotions"`
	LastSafetyViolation     time.Time `json:"last_safety_violation"`
	MaxObservedClockSkew    time.Duration `json:"max_observed_clock_skew"`
}

// Lease represents the distributed lock state with safety mechanisms
type Lease struct {
	Holder       string      `json:"holder"`
	ExpiresAt    time.Time   `json:"expires_at"`
	Generation   int64       `json:"generation"`
	Region       string      `json:"region"`
	AdvertiseURL string      `json:"advertise_url"`
	
	// Safety mechanisms
	FenceToken   uint64      `json:"fence_token"`
	VectorClock  VectorClock `json:"vector_clock"`
	SafetyEpoch  uint64      `json:"safety_epoch"`
	NodeHealth   map[string]time.Time `json:"node_health"`
	
	// Split-brain protection
	QuorumSize     int       `json:"quorum_size"`
	QuorumNodes    []string  `json:"quorum_nodes"`
	PartitionInfo  *NetworkPartitionInfo `json:"partition_info,omitempty"`
}

// Manager manages distributed leases using Consul with safety mechanisms
type Manager struct {
	consulURL    string
	hostname     string
	advertiseURL string
	client       *api.Client
	
	// Configuration
	SessionName  string
	Key          string
	KeyPrefix    string
	TTL          time.Duration
	LockDelay    time.Duration
	
	// Safety configuration
	MaxClockSkew              time.Duration
	SplitBrainDetectionInterval time.Duration
	MinQuorumSize             int
	EnableFenceTokens         bool
	EnableVectorClocks        bool
	
	// Current state
	mu              sync.RWMutex
	currentLease    *Lease
	sessionID       string
	fenceToken      uint64  // Atomic counter
	vectorClock     VectorClock
	safetyEpoch     uint64  // Incremented on safety violations
	
	// Safety monitoring
	safetyMetrics   SafetyMetrics
	knownNodes      map[string]time.Time // Last seen time for each node
	
	// Background workers
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	
	// Callbacks
	onLeaseAcquired func(*Lease)
	onLeaseLost     func(*Lease)
	onSplitBrain    func(*NetworkPartitionInfo)
}

// NewManager creates a new lease manager with safety mechanisms
func NewManager(consulURL, key, hostname, advertiseURL string) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	
	return &Manager{
		consulURL:                  consulURL,
		hostname:                   hostname,
		advertiseURL:               advertiseURL,
		SessionName:                DefaultSessionName,
		Key:                        key,
		TTL:                        DefaultTTL,
		LockDelay:                  DefaultLockDelay,
		MaxClockSkew:               DefaultMaxClockSkew,
		SplitBrainDetectionInterval: DefaultSplitBrainDetectionInterval,
		MinQuorumSize:              DefaultMinQuorumSize,
		EnableFenceTokens:          true,
		EnableVectorClocks:         true,
		vectorClock:                make(VectorClock),
		knownNodes:                 make(map[string]time.Time),
		ctx:                        ctx,
		cancel:                     cancel,
	}
}

// Open initializes the Consul client and starts safety monitoring
func (m *Manager) Open() error {
	u, err := url.Parse(m.consulURL)
	if err != nil {
		return fmt.Errorf("invalid consul URL: %w", err)
	}

	if m.Key == "" {
		return fmt.Errorf("must specify a consul key")
	} else if m.hostname == "" {
		return fmt.Errorf("must specify a hostname for this node")
	} else if m.advertiseURL == "" {
		return fmt.Errorf("must specify an advertise URL for this node")
	}

	config := api.DefaultConfig()
	config.Address = u.Host
	config.Scheme = u.Scheme
	if u.User != nil {
		config.Token, _ = u.User.Password()
	}
	if v := strings.TrimPrefix(u.Path, "/"); v != "" {
		m.KeyPrefix = v
	}

	if m.client, err = api.NewClient(config); err != nil {
		return fmt.Errorf("failed to create consul client: %w", err)
	}

	// Register this node in Consul catalog
	if err := m.registerNode(); err != nil {
		return fmt.Errorf("failed to register node: %w", err)
	}
	
	// Initialize vector clock for this node
	m.vectorClock[m.hostname] = 0
	
	// Start background workers
	m.wg.Add(3)
	go m.heartbeatWorker()
	go m.splitBrainDetectionWorker()
	go m.safetyMonitorWorker()

	return nil
}

// Close cleans up the lease manager
func (m *Manager) Close() error {
	m.cancel()
	m.wg.Wait()
	
	if m.sessionID != "" {
		// Release the session
		_, err := m.client.Session().Destroy(m.sessionID, nil)
		return err
	}
	return nil
}

// AcquireLease attempts to acquire or renew the distributed lease with safety checks
func (m *Manager) AcquireLease(ctx context.Context) (*Lease, error) {
	// Check for split-brain conditions before attempting to acquire lease
	if partitionInfo := m.detectSplitBrain(); partitionInfo != nil {
		return nil, fmt.Errorf("split-brain detected, refusing to acquire lease: %v", partitionInfo)
	}
	
	// Create or renew session
	if m.sessionID == "" {
		if err := m.createSession(); err != nil {
			return nil, fmt.Errorf("failed to create session: %w", err)
		}
	}

	// Update vector clock and fence token
	m.mu.Lock()
	m.vectorClock.Update(m.hostname)
	newFenceToken := atomic.AddUint64(&m.fenceToken, 1)
	vectorClockCopy := m.vectorClock.Clone()
	m.mu.Unlock()
	
	keyPath := m.getKeyPath()
	
	lease := &Lease{
		Holder:       m.hostname,
		ExpiresAt:    time.Now().Add(m.TTL),
		Generation:   time.Now().UnixNano(),
		Region:       "default", // TODO: Make configurable
		AdvertiseURL: m.advertiseURL,
		FenceToken:   newFenceToken,
		VectorClock:  vectorClockCopy,
		SafetyEpoch:  m.safetyEpoch,
		NodeHealth:   m.getNodeHealthSnapshot(),
		QuorumSize:   m.MinQuorumSize,
		QuorumNodes:  m.getQuorumNodes(),
	}

	leaseData, err := json.Marshal(lease)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal lease: %w", err)
	}

	// Try to acquire the lock with safety checks
	pair := &api.KVPair{
		Key:     keyPath,
		Value:   leaseData,
		Session: m.sessionID,
	}

	acquired, _, err := m.client.KV().Acquire(pair, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lease: %w", err)
	}

	if acquired {
		// Verify lease integrity after acquisition
		if err := m.verifyLeaseIntegrity(lease); err != nil {
			// Safety violation detected, release lease immediately
			m.releaseLease(ctx)
			atomic.AddUint64(&m.safetyMetrics.FenceTokenViolations, 1)
			m.safetyMetrics.LastSafetyViolation = time.Now()
			return nil, fmt.Errorf("lease integrity verification failed: %w", err)
		}
		
		m.mu.Lock()
		m.currentLease = lease
		m.mu.Unlock()
		
		log.Printf("[DistSQLite Lease] Acquired lease for %s (fence_token: %d, safety_epoch: %d)", 
			m.hostname, lease.FenceToken, lease.SafetyEpoch)
		
		if m.onLeaseAcquired != nil {
			m.onLeaseAcquired(lease)
		}
		
		return lease, nil
	}

	// Failed to acquire, get current lease holder with safety checks
	currentLease, err := m.GetCurrentLease(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get current lease: %w", err)
	}
	
	// Verify current lease holder's integrity
	if err := m.verifyLeaseIntegrity(currentLease); err != nil {
		log.Printf("[DistSQLite Lease] Current lease holder failed integrity check: %v", err)
		// Attempt to invalidate the lease if we detect corruption
		m.invalidateCorruptedLease(currentLease)
	}

	return currentLease, nil
}

// verifyLeaseIntegrity performs safety checks on a lease
func (m *Manager) verifyLeaseIntegrity(lease *Lease) error {
	if lease == nil {
		return fmt.Errorf("lease is nil")
	}
	
	// Check fence token ordering
	if m.EnableFenceTokens {
		m.mu.RLock()
		lastFenceToken := atomic.LoadUint64(&m.fenceToken)
		m.mu.RUnlock()
		
		if lease.FenceToken <= lastFenceToken && lease.Holder != m.hostname {
			return fmt.Errorf("fence token violation: lease token %d <= last known token %d", 
				lease.FenceToken, lastFenceToken)
		}
	}
	
	// Check vector clock consistency
	if m.EnableVectorClocks && lease.VectorClock != nil {
		m.mu.RLock()
		localClock := m.vectorClock.Clone()
		m.mu.RUnlock()
		
		// Check for impossible clock values (basic sanity check)
		for nodeID, timestamp := range lease.VectorClock {
			if localTimestamp, exists := localClock[nodeID]; exists {
				skew := time.Duration(int64(timestamp)-int64(localTimestamp)) * time.Nanosecond
				if skew > m.MaxClockSkew {
					atomic.AddUint64(&m.safetyMetrics.ClockSkewDetections, 1)
					m.safetyMetrics.MaxObservedClockSkew = skew
					return fmt.Errorf("clock skew too large for node %s: %v", nodeID, skew)
				}
			}
		}
	}
	
	// Check quorum requirements
	if len(lease.QuorumNodes) < m.MinQuorumSize {
		return fmt.Errorf("lease does not meet minimum quorum size: %d < %d", 
			len(lease.QuorumNodes), m.MinQuorumSize)
	}
	
	return nil
}

// invalidateCorruptedLease attempts to invalidate a corrupted lease
func (m *Manager) invalidateCorruptedLease(lease *Lease) {
	log.Printf("[DistSQLite Lease] Attempting to invalidate corrupted lease from %s", lease.Holder)
	
	// Increment safety epoch to invalidate the lease
	atomic.AddUint64(&m.safetyEpoch, 1)
	
	// Try to forcibly release the corrupted lease (if we have permissions)
	keyPath := m.getKeyPath()
	_, err := m.client.KV().Delete(keyPath, nil)
	if err != nil {
		log.Printf("[DistSQLite Lease] Failed to delete corrupted lease: %v", err)
	}
}

// detectSplitBrain detects split-brain conditions
func (m *Manager) detectSplitBrain() *NetworkPartitionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	now := time.Now()
	activeNodes := make([]string, 0)
	isolatedNodes := make([]string, 0)
	
	// Check which nodes are still responsive
	for nodeID, lastSeen := range m.knownNodes {
		if now.Sub(lastSeen) < m.TTL*3 { // Consider node active if seen within 3 TTL periods
			activeNodes = append(activeNodes, nodeID)
		} else {
			isolatedNodes = append(isolatedNodes, nodeID)
		}
	}
	
	// Check if we have a quorum
	if len(activeNodes) < m.MinQuorumSize {
		partitionID := m.generatePartitionID()
		return &NetworkPartitionInfo{
			DetectedAt:    now,
			PartitionID:   partitionID,
			NodesInQuorum: activeNodes,
			IsolatedNodes: isolatedNodes,
		}
	}
	
	return nil
}

// generatePartitionID generates a unique partition identifier
func (m *Manager) generatePartitionID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// getNodeHealthSnapshot returns a snapshot of known node health
func (m *Manager) getNodeHealthSnapshot() map[string]time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	snapshot := make(map[string]time.Time)
	for nodeID, lastSeen := range m.knownNodes {
		snapshot[nodeID] = lastSeen
	}
	return snapshot
}

// getQuorumNodes returns the current quorum nodes
func (m *Manager) getQuorumNodes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	nodes := make([]string, 0, len(m.knownNodes))
	now := time.Now()
	
	for nodeID, lastSeen := range m.knownNodes {
		if now.Sub(lastSeen) < m.TTL*2 { // Consider node in quorum if seen recently
			nodes = append(nodes, nodeID)
		}
	}
	
	return nodes
}

// heartbeatWorker sends periodic heartbeats and updates node health
func (m *Manager) heartbeatWorker() {
	defer m.wg.Done()
	
	ticker := time.NewTicker(DefaultHeartbeatInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.sendHeartbeat()
			m.updateNodeHealth()
		}
	}
}

// splitBrainDetectionWorker monitors for split-brain conditions
func (m *Manager) splitBrainDetectionWorker() {
	defer m.wg.Done()
	
	ticker := time.NewTicker(m.SplitBrainDetectionInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if partitionInfo := m.detectSplitBrain(); partitionInfo != nil {
				m.handleSplitBrain(partitionInfo)
			}
		}
	}
}

// safetyMonitorWorker monitors safety violations and metrics
func (m *Manager) safetyMonitorWorker() {
	defer m.wg.Done()
	
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.monitorSafety()
		}
	}
}

// sendHeartbeat sends a heartbeat to indicate this node is alive
func (m *Manager) sendHeartbeat() {
	heartbeatKey := fmt.Sprintf("%s/heartbeat/%s", m.getKeyPath(), m.hostname)
	
	heartbeat := map[string]interface{}{
		"timestamp":     time.Now(),
		"advertise_url": m.advertiseURL,
		"vector_clock":  m.vectorClock.Clone(),
	}
	
	data, err := json.Marshal(heartbeat)
	if err != nil {
		log.Printf("[DistSQLite Lease] Failed to marshal heartbeat: %v", err)
		return
	}
	
	pair := &api.KVPair{
		Key:   heartbeatKey,
		Value: data,
		TTL:   m.TTL.String(),
	}
	
	_, err = m.client.KV().Put(pair, nil)
	if err != nil {
		log.Printf("[DistSQLite Lease] Failed to send heartbeat: %v", err)
	}
}

// updateNodeHealth updates the health status of known nodes
func (m *Manager) updateNodeHealth() {
	heartbeatPrefix := fmt.Sprintf("%s/heartbeat/", m.getKeyPath())
	
	pairs, _, err := m.client.KV().List(heartbeatPrefix, nil)
	if err != nil {
		log.Printf("[DistSQLite Lease] Failed to list heartbeats: %v", err)
		return
	}
	
	m.mu.Lock()
	defer m.mu.Unlock()
	
	for _, pair := range pairs {
		nodeID := strings.TrimPrefix(pair.Key, heartbeatPrefix)
		if nodeID == "" {
			continue
		}
		
		var heartbeat map[string]interface{}
		if err := json.Unmarshal(pair.Value, &heartbeat); err != nil {
			continue
		}
		
		if timestampStr, ok := heartbeat["timestamp"].(string); ok {
			if timestamp, err := time.Parse(time.RFC3339, timestampStr); err == nil {
				m.knownNodes[nodeID] = timestamp
			}
		}
		
		// Merge vector clocks
		if vectorClockData, ok := heartbeat["vector_clock"].(map[string]interface{}); ok {
			for node, ts := range vectorClockData {
				if timestamp, ok := ts.(float64); ok {
					if m.vectorClock[node] < uint64(timestamp) {
						m.vectorClock[node] = uint64(timestamp)
					}
				}
			}
		}
	}
}

// handleSplitBrain handles detected split-brain conditions
func (m *Manager) handleSplitBrain(partitionInfo *NetworkPartitionInfo) {
	log.Printf("[DistSQLite Lease] Split-brain detected: %+v", partitionInfo)
	
	atomic.AddUint64(&m.safetyMetrics.SplitBrainDetections, 1)
	m.safetyMetrics.LastSafetyViolation = time.Now()
	
	// If we're the lease holder and in a minority partition, demote ourselves
	m.mu.RLock()
	isLeaseHolder := m.currentLease != nil && m.currentLease.Holder == m.hostname
	m.mu.RUnlock()
	
	if isLeaseHolder && len(partitionInfo.NodesInQuorum) < m.MinQuorumSize {
		log.Printf("[DistSQLite Lease] Demoting self due to minority partition")
		atomic.AddUint64(&m.safetyMetrics.AutomaticDemotions, 1)
		
		// Release lease immediately
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		
		if err := m.releaseLease(ctx); err != nil {
			log.Printf("[DistSQLite Lease] Failed to release lease during split-brain: %v", err)
		}
		
		if m.onLeaseLost != nil {
			m.mu.RLock()
			lease := m.currentLease
			m.mu.RUnlock()
			if lease != nil {
				m.onLeaseLost(lease)
			}
		}
	}
	
	if m.onSplitBrain != nil {
		m.onSplitBrain(partitionInfo)
	}
}

// monitorSafety monitors for safety violations
func (m *Manager) monitorSafety() {
	// Check current lease for safety violations
	currentLease, err := m.GetCurrentLease(context.Background())
	if err != nil {
		return // No lease to check
	}
	
	if err := m.verifyLeaseIntegrity(currentLease); err != nil {
		log.Printf("[DistSQLite Lease] Safety violation detected: %v", err)
		m.safetyMetrics.LastSafetyViolation = time.Now()
		
		// If we're not the lease holder but detect violations, report them
		if currentLease.Holder != m.hostname {
			atomic.AddUint64(&m.safetyMetrics.FenceTokenViolations, 1)
		}
	}
}

// GetCurrentLease returns the current lease holder with safety checks
func (m *Manager) GetCurrentLease(ctx context.Context) (*Lease, error) {
	keyPath := m.getKeyPath()
	
	pair, _, err := m.client.KV().Get(keyPath, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get lease: %w", err)
	}

	if pair == nil || pair.Value == nil {
		return nil, fmt.Errorf("no active lease found")
	}

	var lease Lease
	if err := json.Unmarshal(pair.Value, &lease); err != nil {
		return nil, fmt.Errorf("failed to unmarshal lease: %w", err)
	}

	return &lease, nil
}

// IsLeaseHolder returns true if this node holds the lease
func (m *Manager) IsLeaseHolder() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentLease != nil && m.currentLease.Holder == m.hostname
}

// GetFenceToken returns the current fence token
func (m *Manager) GetFenceToken() uint64 {
	return atomic.LoadUint64(&m.fenceToken)
}

// GetSafetyMetrics returns current safety metrics
func (m *Manager) GetSafetyMetrics() SafetyMetrics {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.safetyMetrics
}

// SetCallbacks sets event callbacks
func (m *Manager) SetCallbacks(onAcquired, onLost func(*Lease), onSplitBrain func(*NetworkPartitionInfo)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onLeaseAcquired = onAcquired
	m.onLeaseLost = onLost
	m.onSplitBrain = onSplitBrain
}

// ReleaseLease releases the current lease
func (m *Manager) ReleaseLease(ctx context.Context) error {
	return m.releaseLease(ctx)
}

// releaseLease internal implementation of lease release
func (m *Manager) releaseLease(ctx context.Context) error {
	m.mu.RLock()
	isLeaseHolder := m.currentLease != nil && m.currentLease.Holder == m.hostname
	m.mu.RUnlock()
	
	if !isLeaseHolder {
		return fmt.Errorf("not the lease holder")
	}

	keyPath := m.getKeyPath()
	
	// Release the lock
	pair := &api.KVPair{
		Key:     keyPath,
		Session: m.sessionID,
	}

	released, _, err := m.client.KV().Release(pair, nil)
	if err != nil {
		return fmt.Errorf("failed to release lease: %w", err)
	}

	if !released {
		return fmt.Errorf("failed to release lease")
	}

	m.mu.Lock()
	m.currentLease = nil
	m.mu.Unlock()
	
	log.Printf("[DistSQLite Lease] Released lease for %s", m.hostname)
	return nil
}

// RenewLease renews the current lease with safety checks
func (m *Manager) RenewLease(ctx context.Context) error {
	m.mu.RLock()
	isLeaseHolder := m.currentLease != nil && m.currentLease.Holder == m.hostname
	currentLease := m.currentLease
	m.mu.RUnlock()
	
	if !isLeaseHolder {
		return fmt.Errorf("not the lease holder")
	}
	
	// Check for split-brain before renewal
	if partitionInfo := m.detectSplitBrain(); partitionInfo != nil {
		// Split-brain detected, don't renew
		log.Printf("[DistSQLite Lease] Refusing to renew lease due to split-brain: %v", partitionInfo)
		return fmt.Errorf("split-brain detected, cannot renew lease")
	}

	// Renew the session to extend TTL
	entry, _, err := m.client.Session().Renew(m.sessionID, nil)
	if err != nil {
		return fmt.Errorf("failed to renew session: %w", err)
	}

	if entry == nil {
		// Session expired, need to reacquire
		m.sessionID = ""
		m.mu.Lock()
		m.currentLease = nil
		m.mu.Unlock()
		return fmt.Errorf("session expired")
	}

	// Update lease expiration time
	if currentLease != nil {
		m.mu.Lock()
		m.currentLease.ExpiresAt = time.Now().Add(m.TTL)
		m.mu.Unlock()
	}
	
	return nil
}

// GetCurrentLeaseholder returns the hostname of current lease holder
func (m *Manager) GetCurrentLeaseholder(ctx context.Context) (string, error) {
	lease, err := m.GetCurrentLease(ctx)
	if err != nil {
		return "", err
	}
	return lease.Holder, nil
}

// Helper methods (keeping existing ones and adding new ones)

func (m *Manager) createSession() error {
	entry := &api.SessionEntry{
		Name:      m.SessionName,
		TTL:       m.TTL.String(),
		LockDelay: m.LockDelay,
		Behavior:  api.SessionBehaviorRelease,
	}

	sessionID, _, err := m.client.Session().Create(entry, nil)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}

	m.sessionID = sessionID
	return nil
}

func (m *Manager) registerNode() error {
	nodeName := m.getNodeName()
	if nodeName == "" {
		return nil // Skip registration if no node name
	}

	registration := &api.CatalogRegistration{
		Node:    nodeName,
		Address: "localhost", // Not used for our purposes
	}

	_, err := m.client.Catalog().Register(registration, nil)
	if err != nil {
		return fmt.Errorf("failed to register node %q: %w", nodeName, err)
	}

	return nil
}

func (m *Manager) getNodeName() string {
	// Use hostname as node name
	return m.hostname
}

func (m *Manager) getKeyPath() string {
	if m.KeyPrefix != "" {
		return fmt.Sprintf("%s/%s", m.KeyPrefix, m.Key)
	}
	return m.Key
}