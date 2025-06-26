package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hashicorp/consul/api"
)

// Default lease settings - borrowed from LiteFS proven values
const (
	DefaultSessionName = "distsqlite"
	DefaultTTL         = 10 * time.Second
	DefaultLockDelay   = 1 * time.Second
)

// Lease represents the distributed lock state
type Lease struct {
	Holder       string    `json:"holder"`
	ExpiresAt    time.Time `json:"expires_at"`
	Generation   int64     `json:"generation"`   // Monotonic counter for ordering
	FenceToken   int64     `json:"fence_token"`  // Unique token for this lease
	Region       string    `json:"region"`
	AdvertiseURL string    `json:"advertise_url"`
	AcquiredAt   time.Time `json:"acquired_at"`
}

// Manager manages distributed leases using Consul
type Manager struct {
	consulURL    string
	hostname     string
	advertiseURL string
	client       *api.Client

	// SessionName is the name associated with the Consul session
	SessionName string

	// Key is the Consul KV key used to acquire the lock
	Key string

	// KeyPrefix is prepended to the key if URL contains a path
	KeyPrefix string

	// TTL is the time until the lease expires
	TTL time.Duration

	// LockDelay is the time after lock expires that a new lock can be acquired
	LockDelay time.Duration

	// Current lease information
	currentLease   *Lease
	sessionID      string
	lastFenceToken int64 // Last fence token we've seen
	
	// Fence token counter (for generating unique tokens)
	fenceCounter int64
}

// NewManager creates a new lease manager
func NewManager(consulURL, key, hostname, advertiseURL string) *Manager {
	return &Manager{
		consulURL:    consulURL,
		hostname:     hostname,
		advertiseURL: advertiseURL,
		SessionName:  DefaultSessionName,
		Key:          key,
		TTL:          DefaultTTL,
		LockDelay:    DefaultLockDelay,
		fenceCounter: time.Now().UnixNano(), // Initialize with current time
	}
}

// Open initializes the Consul client
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

	// Initialize fence counter from existing leases
	if err := m.initializeFenceCounter(); err != nil {
		log.Printf("[DistSQLite Lease] Warning: Could not initialize fence counter: %v", err)
	}

	return nil
}

// Close cleans up the lease manager
func (m *Manager) Close() error {
	if m.sessionID != "" {
		// Release the session
		_, err := m.client.Session().Destroy(m.sessionID, nil)
		return err
	}
	return nil
}

// AcquireLease attempts to acquire or renew the distributed lease
func (m *Manager) AcquireLease(ctx context.Context) (*Lease, error) {
	// Create or renew session
	if m.sessionID == "" {
		if err := m.createSession(); err != nil {
			return nil, fmt.Errorf("failed to create session: %w", err)
		}
	}

	// Get current lease to check fence tokens
	currentLease, err := m.GetCurrentLease(ctx)
	if err == nil {
		// Validate fence token to prevent acquiring expired leases
		if !m.validateFenceToken(currentLease) {
			return nil, fmt.Errorf("invalid fence token detected - possible split brain")
		}
		
		// If current holder is us and lease is still valid, just return it
		if currentLease.Holder == m.hostname && currentLease.ExpiresAt.After(time.Now()) {
			m.currentLease = currentLease
			return currentLease, nil
		}
	}

	// Generate new fence token
	fenceToken := atomic.AddInt64(&m.fenceCounter, 1)
	
	keyPath := m.getKeyPath()
	
	lease := &Lease{
		Holder:       m.hostname,
		ExpiresAt:    time.Now().Add(m.TTL),
		Generation:   time.Now().UnixNano(),
		FenceToken:   fenceToken,
		Region:       "default", // TODO: Make configurable
		AdvertiseURL: m.advertiseURL,
		AcquiredAt:   time.Now(),
	}

	leaseData, err := json.Marshal(lease)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal lease: %w", err)
	}

	// Try to acquire the lock
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
		m.currentLease = lease
		m.lastFenceToken = fenceToken
		log.Printf("[DistSQLite Lease] Acquired lease for %s (fence token: %d)", m.hostname, fenceToken)
		return lease, nil
	}

	// Failed to acquire, get current lease holder
	currentLease, err = m.GetCurrentLease(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get current lease: %w", err)
	}

	return currentLease, nil
}

// GetCurrentLease returns the current lease holder
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
	if m.currentLease == nil {
		return false
	}
	
	// Check if lease has expired
	if m.currentLease.ExpiresAt.Before(time.Now()) {
		m.currentLease = nil
		return false
	}
	
	return m.currentLease.Holder == m.hostname
}

// GetCurrentFenceToken returns the fence token for the current lease
func (m *Manager) GetCurrentFenceToken() int64 {
	if m.currentLease == nil {
		return 0
	}
	return m.currentLease.FenceToken
}

// ValidateWrite checks if a write operation is safe based on fence token
func (m *Manager) ValidateWrite(fenceToken int64) error {
	if !m.IsLeaseHolder() {
		return fmt.Errorf("not the lease holder")
	}
	
	if fenceToken != m.currentLease.FenceToken {
		return fmt.Errorf("invalid fence token: expected %d, got %d", 
			m.currentLease.FenceToken, fenceToken)
	}
	
	// Check if lease has expired
	if m.currentLease.ExpiresAt.Before(time.Now()) {
		return fmt.Errorf("lease has expired")
	}
	
	return nil
}

// ReleaseLease releases the current lease
func (m *Manager) ReleaseLease(ctx context.Context) error {
	if !m.IsLeaseHolder() {
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

	log.Printf("[DistSQLite Lease] Released lease for %s (fence token: %d)", 
		m.hostname, m.currentLease.FenceToken)
	m.currentLease = nil
	return nil
}

// RenewLease renews the current lease
func (m *Manager) RenewLease(ctx context.Context) error {
	if !m.IsLeaseHolder() {
		return fmt.Errorf("not the lease holder")
	}

	// Renew the session to extend TTL
	entry, _, err := m.client.Session().Renew(m.sessionID, nil)
	if err != nil {
		return fmt.Errorf("failed to renew session: %w", err)
	}

	if entry == nil {
		// Session expired, need to reacquire
		m.sessionID = ""
		m.currentLease = nil
		return fmt.Errorf("session expired")
	}

	// Update lease expiration time and refresh fence token
	oldFenceToken := m.currentLease.FenceToken
	m.currentLease.ExpiresAt = time.Now().Add(m.TTL)
	m.currentLease.FenceToken = atomic.AddInt64(&m.fenceCounter, 1)
	
	// Update the lease in Consul with new fence token
	keyPath := m.getKeyPath()
	leaseData, err := json.Marshal(m.currentLease)
	if err != nil {
		return fmt.Errorf("failed to marshal lease for renewal: %w", err)
	}
	
	pair := &api.KVPair{
		Key:     keyPath,
		Value:   leaseData,
		Session: m.sessionID,
	}
	
	_, err = m.client.KV().Put(pair, nil)
	if err != nil {
		return fmt.Errorf("failed to update lease with new fence token: %w", err)
	}
	
	log.Printf("[DistSQLite Lease] Renewed lease for %s (fence token: %d -> %d)", 
		m.hostname, oldFenceToken, m.currentLease.FenceToken)
	
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

// Helper methods

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

// initializeFenceCounter initializes the fence counter from existing leases
func (m *Manager) initializeFenceCounter() error {
	// Get all existing leases to find the highest fence token
	keyPath := m.getKeyPath()
	pair, _, err := m.client.KV().Get(keyPath, nil)
	if err != nil || pair == nil {
		// No existing lease, start with current time
		return nil
	}
	
	var lease Lease
	if err := json.Unmarshal(pair.Value, &lease); err != nil {
		return err
	}
	
	// Initialize our counter to be higher than any existing fence token
	// Use a much larger gap (100,000) to handle high-concurrency scenarios
	// and prevent potential race conditions
	if lease.FenceToken > m.fenceCounter {
		atomic.StoreInt64(&m.fenceCounter, lease.FenceToken+100000)
	}
	
	m.lastFenceToken = lease.FenceToken
	log.Printf("[DistSQLite Lease] Initialized fence counter to %d (gap: 100k for safety)", m.fenceCounter)
	return nil
}

// validateFenceToken validates that a fence token is legitimate
func (m *Manager) validateFenceToken(lease *Lease) bool {
	// Fence tokens should be monotonically increasing
	if lease.FenceToken <= m.lastFenceToken {
		log.Printf("[DistSQLite Lease] Invalid fence token: %d <= %d", 
			lease.FenceToken, m.lastFenceToken)
		return false
	}
	
	// Update our last seen fence token
	m.lastFenceToken = lease.FenceToken
	return true
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