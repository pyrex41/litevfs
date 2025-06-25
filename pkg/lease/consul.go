package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
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
	Holder     string    `json:"holder"`
	ExpiresAt  time.Time `json:"expires_at"`
	Generation int64     `json:"generation"`
	Region     string    `json:"region"`
	AdvertiseURL string  `json:"advertise_url"`
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
	currentLease *Lease
	sessionID    string
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

	// Attempt to acquire the lock
	keyPath := m.getKeyPath()
	
	lease := &Lease{
		Holder:       m.hostname,
		ExpiresAt:    time.Now().Add(m.TTL),
		Generation:   time.Now().UnixNano(),
		Region:       "default", // TODO: Make configurable
		AdvertiseURL: m.advertiseURL,
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
		log.Printf("[DistSQLite] Acquired lease for %s", m.hostname)
		return lease, nil
	}

	// Failed to acquire, get current lease holder
	currentLease, err := m.GetCurrentLease(ctx)
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
	return m.currentLease != nil && m.currentLease.Holder == m.hostname
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

	m.currentLease = nil
	log.Printf("[DistSQLite] Released lease for %s", m.hostname)
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

	// Update lease expiration time
	m.currentLease.ExpiresAt = time.Now().Add(m.TTL)
	
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