package leaseserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LeaseClient handles communication with the lease server
type LeaseClient struct {
	serverURL string
	nodeID    string
	client    *http.Client
	
	// Current lease state
	currentLease *Lease
}

// NewLeaseClient creates a new lease client
func NewLeaseClient(serverURL, nodeID string) *LeaseClient {
	return &LeaseClient{
		serverURL: serverURL,
		nodeID:    nodeID,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// AcquireLease attempts to acquire a write lease
func (c *LeaseClient) AcquireLease(ctx context.Context, ttl int) (*Lease, error) {
	req := LeaseRequest{
		NodeID: c.nodeID,
		TTL:    ttl,
	}
	
	var response LeaseResponse
	err := c.makeRequest(ctx, "POST", "/lease/acquire", req, &response)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lease: %w", err)
	}
	
	if !response.Success {
		return nil, fmt.Errorf("lease acquisition failed: %s", response.Error)
	}
	
	c.currentLease = response.Lease
	return response.Lease, nil
}

// RenewLease renews the current lease
func (c *LeaseClient) RenewLease(ctx context.Context, ttl int) (*Lease, error) {
	req := LeaseRequest{
		NodeID: c.nodeID,
		TTL:    ttl,
	}
	
	var response LeaseResponse
	err := c.makeRequest(ctx, "POST", "/lease/renew", req, &response)
	if err != nil {
		return nil, fmt.Errorf("failed to renew lease: %w", err)
	}
	
	if !response.Success {
		return nil, fmt.Errorf("lease renewal failed: %s", response.Error)
	}
	
	c.currentLease = response.Lease
	return response.Lease, nil
}

// ReleaseLease releases the current lease
func (c *LeaseClient) ReleaseLease(ctx context.Context) error {
	req := LeaseRequest{
		NodeID: c.nodeID,
	}
	
	var response LeaseResponse
	err := c.makeRequest(ctx, "POST", "/lease/release", req, &response)
	if err != nil {
		return fmt.Errorf("failed to release lease: %w", err)
	}
	
	if !response.Success {
		return fmt.Errorf("lease release failed: %s", response.Error)
	}
	
	c.currentLease = nil
	return nil
}

// GetLeaseStatus gets the current lease status from server
func (c *LeaseClient) GetLeaseStatus(ctx context.Context) (*Lease, error) {
	var response LeaseResponse
	err := c.makeRequest(ctx, "GET", "/lease/status", nil, &response)
	if err != nil {
		return nil, fmt.Errorf("failed to get lease status: %w", err)
	}
	
	if !response.Success {
		return nil, fmt.Errorf("no active lease: %s", response.Error)
	}
	
	return response.Lease, nil
}

// IsLeaseHolder checks if this client holds the current lease
func (c *LeaseClient) IsLeaseHolder() bool {
	if c.currentLease == nil {
		return false
	}
	
	// Check if lease is expired
	if time.Now().After(c.currentLease.ExpiresAt) {
		c.currentLease = nil
		return false
	}
	
	return true
}

// GetCurrentLease returns the current lease if held
func (c *LeaseClient) GetCurrentLease() *Lease {
	if !c.IsLeaseHolder() {
		return nil
	}
	return c.currentLease
}

// StartLeaseRenewal starts automatic lease renewal in a goroutine
func (c *LeaseClient) StartLeaseRenewal(ctx context.Context, renewInterval time.Duration) <-chan error {
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
				if c.IsLeaseHolder() {
					_, err := c.RenewLease(ctx, 30) // Renew for 30 seconds
					if err != nil {
						errChan <- err
						return
					}
				}
			}
		}
	}()
	
	return errChan
}

// HealthCheck checks if the lease server is healthy
func (c *LeaseClient) HealthCheck(ctx context.Context) error {
	var response map[string]interface{}
	err := c.makeRequest(ctx, "GET", "/health", nil, &response)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	
	status, ok := response["status"].(string)
	if !ok || status != "healthy" {
		return fmt.Errorf("server not healthy: %v", response)
	}
	
	return nil
}

// makeRequest is a helper method for making HTTP requests
func (c *LeaseClient) makeRequest(ctx context.Context, method, path string, body interface{}, response interface{}) error {
	url := c.serverURL + path
	
	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonData)
	}
	
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server error %d: %s", resp.StatusCode, string(bodyBytes))
	}
	
	if response != nil {
		if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}
	
	return nil
}