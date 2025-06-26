package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// HTTPManager implements the same interface as consul.Manager but uses
// HTTP coordination with file-based leader election
type HTTPManager struct {
	baseURL      string
	hostname     string
	advertiseURL string
	client       *http.Client
	
	// Lease configuration - same as Consul version
	SessionName  string
	Key          string
	KeyPrefix    string
	TTL          time.Duration
	LockDelay    time.Duration
	
	// Current lease information
	currentLease   *Lease
	lastFenceToken int64
	fenceCounter   int64
	
	// File-based locking
	lockDir    string
	lockFile   string
}

// NewHTTPManager creates a new HTTP-based lease manager
func NewHTTPManager(baseURL, key, hostname, advertiseURL string) *HTTPManager {
	return &HTTPManager{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		hostname:     hostname,
		advertiseURL: advertiseURL,
		SessionName:  DefaultSessionName,
		Key:          key,
		TTL:          DefaultTTL,
		LockDelay:    DefaultLockDelay,
		fenceCounter: time.Now().UnixNano(),
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Open initializes the HTTP manager and sets up file-based locking
func (m *HTTPManager) Open() error {
	// Create lock directory based on database key
	m.lockDir = filepath.Join(os.TempDir(), "distsqlite-locks")
	if err := os.MkdirAll(m.lockDir, 0755); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}
	
	// Create lock file path
	safeName := strings.ReplaceAll(m.Key, "/", "_")
	m.lockFile = filepath.Join(m.lockDir, safeName+".lock")
	
	// Initialize fence counter from any existing lock
	if err := m.initializeFenceCounter(); err != nil {
		log.Printf("[DistSQLite HTTP] Warning: Could not initialize fence counter: %v", err)
	}
	
	log.Printf("[DistSQLite HTTP] Initialized with lock file: %s", m.lockFile)
	return nil
}

// Close cleans up the HTTP manager
func (m *HTTPManager) Close() error {
	// Release current lease if we hold it
	if m.IsLeaseHolder() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.ReleaseLease(ctx)
	}
	return nil
}

// AcquireLease attempts to acquire the lease using file-based locking
func (m *HTTPManager) AcquireLease(ctx context.Context) (*Lease, error) {
	// Check if we already hold a valid lease
	if m.currentLease != nil && m.currentLease.Holder == m.hostname && 
		m.currentLease.ExpiresAt.After(time.Now()) {
		return m.currentLease, nil
	}
	
	// Try to acquire the file lock
	lease, err := m.tryAcquireFileLock()
	if err != nil {
		return nil, err
	}
	
	if lease.Holder == m.hostname {
		m.currentLease = lease
		m.lastFenceToken = lease.FenceToken
		log.Printf("[DistSQLite HTTP] Acquired lease for %s (fence token: %d)", 
			m.hostname, lease.FenceToken)
		
		// Notify coordinator about new lease
		if m.baseURL != "" {
			go m.notifyCoordinator("acquired", lease)
		}
	} else {
		log.Printf("[DistSQLite HTTP] Current lease holder: %s (fence token: %d)", 
			lease.Holder, lease.FenceToken)
	}
	
	return lease, nil
}

// tryAcquireFileLock implements atomic file-based leader election
func (m *HTTPManager) tryAcquireFileLock() (*Lease, error) {
	// Try to read existing lock file first
	if data, err := os.ReadFile(m.lockFile); err == nil {
		var existingLease Lease
		if json.Unmarshal(data, &existingLease) == nil {
			// Check if existing lease is still valid
			if existingLease.ExpiresAt.After(time.Now()) {
				return &existingLease, nil
			}
			// Lease expired, we can try to acquire it
		}
	}
	
	// Generate new fence token
	fenceToken := atomic.AddInt64(&m.fenceCounter, 1)
	
	// Create new lease
	lease := &Lease{
		Holder:       m.hostname,
		ExpiresAt:    time.Now().Add(m.TTL),
		Generation:   time.Now().UnixNano(),
		FenceToken:   fenceToken,
		Region:       "default",
		AdvertiseURL: m.advertiseURL,
		AcquiredAt:   time.Now(),
	}
	
	// Try to write the lock file atomically
	leaseData, err := json.Marshal(lease)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal lease: %w", err)
	}
	
	// Write to temporary file first, then atomically rename
	tempFile := m.lockFile + ".tmp." + m.hostname
	if err := os.WriteFile(tempFile, leaseData, 0644); err != nil {
		return nil, fmt.Errorf("failed to write temp lock file: %w", err)
	}
	
	// Atomic rename - this is the critical section
	if err := os.Rename(tempFile, m.lockFile); err != nil {
		os.Remove(tempFile) // Clean up temp file
		
		// If rename failed, someone else might have acquired it
		// Read the current lock file to see who won
		if data, readErr := os.ReadFile(m.lockFile); readErr == nil {
			var currentLease Lease
			if json.Unmarshal(data, &currentLease) == nil {
				return &currentLease, nil
			}
		}
		
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	
	// We successfully acquired the lock
	return lease, nil
}

// GetCurrentLease returns the current lease holder
func (m *HTTPManager) GetCurrentLease(ctx context.Context) (*Lease, error) {
	data, err := os.ReadFile(m.lockFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no active lease")
		}
		return nil, fmt.Errorf("failed to read lease file: %w", err)
	}
	
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		return nil, fmt.Errorf("failed to unmarshal lease: %w", err)
	}
	
	// Check if lease has expired
	if lease.ExpiresAt.Before(time.Now()) {
		// Clean up expired lease
		os.Remove(m.lockFile)
		return nil, fmt.Errorf("lease has expired")
	}
	
	return &lease, nil
}

// IsLeaseHolder returns true if this node holds the lease
func (m *HTTPManager) IsLeaseHolder() bool {
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
func (m *HTTPManager) GetCurrentFenceToken() int64 {
	if m.currentLease == nil {
		return 0
	}
	return m.currentLease.FenceToken
}

// ValidateWrite checks if a write operation is safe based on fence token
func (m *HTTPManager) ValidateWrite(fenceToken int64) error {
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
func (m *HTTPManager) ReleaseLease(ctx context.Context) error {
	if !m.IsLeaseHolder() {
		return fmt.Errorf("not the lease holder")
	}
	
	// Remove the lock file
	if err := os.Remove(m.lockFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove lock file: %w", err)
	}
	
	log.Printf("[DistSQLite HTTP] Released lease for %s (fence token: %d)", 
		m.hostname, m.currentLease.FenceToken)
	
	// Notify coordinator
	if m.baseURL != "" {
		go m.notifyCoordinator("released", m.currentLease)
	}
	
	m.currentLease = nil
	return nil
}

// RenewLease renews the current lease
func (m *HTTPManager) RenewLease(ctx context.Context) error {
	if !m.IsLeaseHolder() {
		return fmt.Errorf("not the lease holder")
	}
	
	// Update lease expiration and generate new fence token
	oldFenceToken := m.currentLease.FenceToken
	m.currentLease.ExpiresAt = time.Now().Add(m.TTL)
	m.currentLease.FenceToken = atomic.AddInt64(&m.fenceCounter, 1)
	
	// Write updated lease to file
	leaseData, err := json.Marshal(m.currentLease)
	if err != nil {
		return fmt.Errorf("failed to marshal renewed lease: %w", err)
	}
	
	// Atomic update using temp file
	tempFile := m.lockFile + ".tmp." + m.hostname
	if err := os.WriteFile(tempFile, leaseData, 0644); err != nil {
		return fmt.Errorf("failed to write renewed lease: %w", err)
	}
	
	if err := os.Rename(tempFile, m.lockFile); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("failed to update lease file: %w", err)
	}
	
	log.Printf("[DistSQLite HTTP] Renewed lease for %s (fence token: %d -> %d)", 
		m.hostname, oldFenceToken, m.currentLease.FenceToken)
	
	return nil
}

// GetCurrentLeaseholder returns the hostname of current lease holder
func (m *HTTPManager) GetCurrentLeaseholder(ctx context.Context) (string, error) {
	lease, err := m.GetCurrentLease(ctx)
	if err != nil {
		return "", err
	}
	return lease.Holder, nil
}

// initializeFenceCounter initializes the fence counter from existing leases
func (m *HTTPManager) initializeFenceCounter() error {
	data, err := os.ReadFile(m.lockFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No existing lease
		}
		return err
	}
	
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		return err
	}
	
	// Initialize our counter to be higher than any existing fence token
	if lease.FenceToken > m.fenceCounter {
		atomic.StoreInt64(&m.fenceCounter, lease.FenceToken+100000)
	}
	
	m.lastFenceToken = lease.FenceToken
	log.Printf("[DistSQLite HTTP] Initialized fence counter to %d", m.fenceCounter)
	return nil
}

// notifyCoordinator sends lease events to the HTTP coordinator (optional)
func (m *HTTPManager) notifyCoordinator(event string, lease *Lease) {
	if m.baseURL == "" {
		return
	}
	
	data := map[string]interface{}{
		"event":    event,
		"hostname": m.hostname,
		"lease":    lease,
	}
	
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("[DistSQLite HTTP] Failed to marshal coordinator notification: %v", err)
		return
	}
	
	req, err := http.NewRequest("POST", m.baseURL+"/notify", strings.NewReader(string(jsonData)))
	if err != nil {
		log.Printf("[DistSQLite HTTP] Failed to create coordinator request: %v", err)
		return
	}
	
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := m.client.Do(req)
	if err != nil {
		log.Printf("[DistSQLite HTTP] Failed to notify coordinator: %v", err)
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[DistSQLite HTTP] Coordinator notification failed: %d %s", 
			resp.StatusCode, string(body))
	}
}