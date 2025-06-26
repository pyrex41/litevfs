package zmq

import (
	"context"
	"fmt"
	"log"
	"time"
)

// Lease operations using ZMQ REQ/REP pattern

// AcquireLease requests a write lease for the specified database
func (t *Transport) AcquireLease(ctx context.Context, dbPath string, ttl int64) (*LeaseResponse, error) {
	if t.leaseReq == nil {
		return nil, fmt.Errorf("lease REQ socket not initialized - node not started")
	}

	req := NewLeaseRequest(t.nodeID, dbPath, "acquire", ttl)

	// Send request with timeout
	resp, err := t.sendLeaseRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lease: %w", err)
	}

	if resp.Success {
		log.Printf("[ZMQ Lease] Node %s acquired lease for %s (token: %d)", 
			t.nodeID, dbPath, resp.FenceToken)
	} else {
		log.Printf("[ZMQ Lease] Node %s failed to acquire lease for %s: %s", 
			t.nodeID, dbPath, resp.Error)
	}

	return resp, nil
}

// RenewLease extends the TTL of an existing lease
func (t *Transport) RenewLease(ctx context.Context, dbPath string, ttl int64) (*LeaseResponse, error) {
	if t.leaseReq == nil {
		return nil, fmt.Errorf("lease REQ socket not initialized - node not started")
	}

	req := NewLeaseRequest(t.nodeID, dbPath, "renew", ttl)

	resp, err := t.sendLeaseRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to renew lease: %w", err)
	}

	if resp.Success {
		log.Printf("[ZMQ Lease] Node %s renewed lease for %s (token: %d)", 
			t.nodeID, dbPath, resp.FenceToken)
	} else {
		log.Printf("[ZMQ Lease] Node %s failed to renew lease for %s: %s", 
			t.nodeID, dbPath, resp.Error)
	}

	return resp, nil
}

// ReleaseLease voluntarily releases a held lease
func (t *Transport) ReleaseLease(ctx context.Context, dbPath string) error {
	if t.leaseReq == nil {
		return fmt.Errorf("lease REQ socket not initialized - node not started")
	}

	req := NewLeaseRequest(t.nodeID, dbPath, "release", 0)

	resp, err := t.sendLeaseRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to release lease: %w", err)
	}

	if resp.Success {
		log.Printf("[ZMQ Lease] Node %s released lease for %s", t.nodeID, dbPath)
	} else {
		log.Printf("[ZMQ Lease] Node %s failed to release lease for %s: %s", 
			t.nodeID, dbPath, resp.Error)
	}

	return nil
}

// GetLeaseStatus queries the current lease holder for a database
func (t *Transport) GetLeaseStatus(ctx context.Context, dbPath string) (*LeaseResponse, error) {
	if t.leaseReq == nil {
		return nil, fmt.Errorf("lease REQ socket not initialized - node not started")
	}

	req := NewLeaseRequest(t.nodeID, dbPath, "status", 0)

	resp, err := t.sendLeaseRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get lease status: %w", err)
	}

	return resp, nil
}

// sendLeaseRequest sends a lease request and waits for response with timeout
func (t *Transport) sendLeaseRequest(ctx context.Context, req *LeaseRequest) (*LeaseResponse, error) {
	t.mu.RLock()
	socket := t.leaseReq
	t.mu.RUnlock()

	if socket == nil {
		return nil, fmt.Errorf("lease socket not available")
	}

	// Send request
	if err := t.sendMessage(socket, req); err != nil {
		return nil, fmt.Errorf("failed to send lease request: %w", err)
	}

	// Set receive timeout
	if err := socket.SetRcvtimeo(t.config.RequestTimeout); err != nil {
		return nil, fmt.Errorf("failed to set receive timeout: %w", err)
	}

	// Wait for response
	var resp LeaseResponse
	if err := t.receiveMessage(socket, &resp); err != nil {
		return nil, fmt.Errorf("failed to receive lease response: %w", err)
	}

	return &resp, nil
}

// Coordinator-side lease handling

// ServeLease handles incoming lease requests (coordinator mode)
func (t *Transport) ServeLease(ctx context.Context, leaseManager LeaseManager) error {
	if t.leaseRep == nil {
		return fmt.Errorf("lease REP socket not initialized - coordinator not started")
	}

	log.Printf("[ZMQ Lease] Starting lease service")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Receive lease request
			var req LeaseRequest
			if err := t.receiveMessage(t.leaseRep, &req); err != nil {
				// Handle receive timeout or errors
				if err.Error() == "resource temporarily unavailable" {
					continue // Timeout, keep listening
				}
				log.Printf("[ZMQ Lease] Error receiving lease request: %v", err)
				continue
			}

			// Process lease request
			resp := t.processLeaseRequest(&req, leaseManager)

			// Send response
			if err := t.sendMessage(t.leaseRep, resp); err != nil {
				log.Printf("[ZMQ Lease] Error sending lease response: %v", err)
				// ZMQ REP requires a response, so we need to continue
			}
		}
	}
}

// processLeaseRequest handles a single lease request
func (t *Transport) processLeaseRequest(req *LeaseRequest, manager LeaseManager) *LeaseResponse {
	switch req.Action {
	case "acquire":
		return manager.AcquireLease(req.NodeID, req.DBPath, req.TTL)
	case "renew":
		return manager.RenewLease(req.NodeID, req.DBPath, req.TTL)
	case "release":
		return manager.ReleaseLease(req.NodeID, req.DBPath)
	case "status":
		return manager.GetLeaseStatus(req.DBPath)
	default:
		return NewLeaseResponse(false, 0, 0, "", 
			fmt.Sprintf("unknown lease action: %s", req.Action))
	}
}

// LeaseManager interface for coordinator lease management
type LeaseManager interface {
	AcquireLease(nodeID, dbPath string, ttl int64) *LeaseResponse
	RenewLease(nodeID, dbPath string, ttl int64) *LeaseResponse
	ReleaseLease(nodeID, dbPath string) *LeaseResponse
	GetLeaseStatus(dbPath string) *LeaseResponse
}

// AutoRenewLease starts automatic lease renewal for a database
func (t *Transport) AutoRenewLease(ctx context.Context, dbPath string, renewInterval time.Duration) <-chan error {
	errChan := make(chan error, 1)

	go func() {
		defer close(errChan)

		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Renew lease with same TTL as renewal interval * 3 for safety
				ttl := int64(renewInterval.Seconds() * 3)
				
				_, err := t.RenewLease(ctx, dbPath, ttl)
				if err != nil {
					select {
					case errChan <- err:
					case <-ctx.Done():
					}
					return
				}
			}
		}
	}()

	return errChan
}