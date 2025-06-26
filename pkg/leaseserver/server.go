package leaseserver

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// LeaseServer manages centralized lease coordination
type LeaseServer struct {
	mu          sync.RWMutex
	currentLease *Lease
	leaseTimeout time.Duration
	dbPath      string
	
	// HTTP server
	server *http.Server
	port   int
}

// Lease represents an active write lease
type Lease struct {
	NodeID     string    `json:"node_id"`
	FenceToken int64     `json:"fence_token"`
	ExpiresAt  time.Time `json:"expires_at"`
	IssuedAt   time.Time `json:"issued_at"`
}

// LeaseRequest represents a lease acquisition request
type LeaseRequest struct {
	NodeID string `json:"node_id"`
	TTL    int    `json:"ttl"` // seconds
}

// LeaseResponse represents the server's response
type LeaseResponse struct {
	Success    bool   `json:"success"`
	Lease      *Lease `json:"lease,omitempty"`
	Error      string `json:"error,omitempty"`
	FenceToken int64  `json:"fence_token"`
}

// NewLeaseServer creates a new centralized lease server
func NewLeaseServer(port int, dbPath string) *LeaseServer {
	return &LeaseServer{
		leaseTimeout: 30 * time.Second,
		dbPath:       dbPath,
		port:         port,
	}
}

// Start starts the lease server
func (s *LeaseServer) Start() error {
	mux := http.NewServeMux()
	
	// Lease management endpoints
	mux.HandleFunc("/lease/acquire", s.handleAcquireLease)
	mux.HandleFunc("/lease/renew", s.handleRenewLease)
	mux.HandleFunc("/lease/release", s.handleReleaseLease)
	mux.HandleFunc("/lease/status", s.handleLeaseStatus)
	
	// Health check
	mux.HandleFunc("/health", s.handleHealth)
	
	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}
	
	log.Printf("Starting lease server on port %d", s.port)
	return s.server.ListenAndServe()
}

// Stop stops the lease server
func (s *LeaseServer) Stop() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// handleAcquireLease handles lease acquisition requests
func (s *LeaseServer) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	
	if req.NodeID == "" {
		s.sendError(w, "NodeID is required", http.StatusBadRequest)
		return
	}
	
	if req.TTL <= 0 {
		req.TTL = 30 // Default 30 seconds
	}
	
	s.mu.Lock()
	defer s.mu.Unlock()
	
	now := time.Now()
	
	// Check if current lease is expired
	if s.currentLease != nil && now.After(s.currentLease.ExpiresAt) {
		log.Printf("Lease for %s expired, clearing", s.currentLease.NodeID)
		s.currentLease = nil
	}
	
	// If no active lease, grant new one
	if s.currentLease == nil {
		fenceToken := now.UnixNano() // Use timestamp as fence token for uniqueness
		
		lease := &Lease{
			NodeID:     req.NodeID,
			FenceToken: fenceToken,
			ExpiresAt:  now.Add(time.Duration(req.TTL) * time.Second),
			IssuedAt:   now,
		}
		
		s.currentLease = lease
		
		log.Printf("Granted lease to %s (token: %d, expires: %v)", 
			req.NodeID, fenceToken, lease.ExpiresAt)
		
		response := LeaseResponse{
			Success:    true,
			Lease:      lease,
			FenceToken: fenceToken,
		}
		
		s.sendJSON(w, response)
		return
	}
	
	// If same node requesting, renew lease
	if s.currentLease.NodeID == req.NodeID {
		s.currentLease.ExpiresAt = now.Add(time.Duration(req.TTL) * time.Second)
		
		log.Printf("Renewed lease for %s (token: %d, expires: %v)", 
			req.NodeID, s.currentLease.FenceToken, s.currentLease.ExpiresAt)
		
		response := LeaseResponse{
			Success:    true,
			Lease:      s.currentLease,
			FenceToken: s.currentLease.FenceToken,
		}
		
		s.sendJSON(w, response)
		return
	}
	
	// Different node requesting while lease active
	response := LeaseResponse{
		Success:    false,
		Error:      fmt.Sprintf("Lease held by %s until %v", s.currentLease.NodeID, s.currentLease.ExpiresAt),
		FenceToken: s.currentLease.FenceToken,
	}
	
	s.sendJSON(w, response)
}

// handleRenewLease handles lease renewal requests
func (s *LeaseServer) handleRenewLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	
	s.mu.Lock()
	defer s.mu.Unlock()
	
	if s.currentLease == nil {
		response := LeaseResponse{
			Success: false,
			Error:   "No active lease to renew",
		}
		s.sendJSON(w, response)
		return
	}
	
	if s.currentLease.NodeID != req.NodeID {
		response := LeaseResponse{
			Success: false,
			Error:   fmt.Sprintf("Lease held by different node: %s", s.currentLease.NodeID),
		}
		s.sendJSON(w, response)
		return
	}
	
	// Renew the lease
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 30
	}
	
	s.currentLease.ExpiresAt = time.Now().Add(time.Duration(ttl) * time.Second)
	
	log.Printf("Renewed lease for %s (token: %d)", req.NodeID, s.currentLease.FenceToken)
	
	response := LeaseResponse{
		Success:    true,
		Lease:      s.currentLease,
		FenceToken: s.currentLease.FenceToken,
	}
	
	s.sendJSON(w, response)
}

// handleReleaseLease handles lease release requests
func (s *LeaseServer) handleReleaseLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	
	s.mu.Lock()
	defer s.mu.Unlock()
	
	if s.currentLease == nil {
		response := LeaseResponse{
			Success: false,
			Error:   "No active lease to release",
		}
		s.sendJSON(w, response)
		return
	}
	
	if s.currentLease.NodeID != req.NodeID {
		response := LeaseResponse{
			Success: false,
			Error:   fmt.Sprintf("Cannot release lease held by different node: %s", s.currentLease.NodeID),
		}
		s.sendJSON(w, response)
		return
	}
	
	log.Printf("Released lease for %s (token: %d)", req.NodeID, s.currentLease.FenceToken)
	s.currentLease = nil
	
	response := LeaseResponse{
		Success: true,
	}
	
	s.sendJSON(w, response)
}

// handleLeaseStatus returns current lease status
func (s *LeaseServer) handleLeaseStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	now := time.Now()
	
	// Check if lease is expired
	if s.currentLease != nil && now.After(s.currentLease.ExpiresAt) {
		response := LeaseResponse{
			Success: false,
			Error:   "No active lease (expired)",
		}
		s.sendJSON(w, response)
		return
	}
	
	if s.currentLease == nil {
		response := LeaseResponse{
			Success: false,
			Error:   "No active lease",
		}
		s.sendJSON(w, response)
		return
	}
	
	response := LeaseResponse{
		Success:    true,
		Lease:      s.currentLease,
		FenceToken: s.currentLease.FenceToken,
	}
	
	s.sendJSON(w, response)
}

// handleHealth returns server health status
func (s *LeaseServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now(),
		"version":   "1.0.0",
	}
	
	s.sendJSON(w, response)
}

// Helper methods
func (s *LeaseServer) sendJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (s *LeaseServer) sendError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}