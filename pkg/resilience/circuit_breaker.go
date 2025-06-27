package resilience

import (
	"errors"
	"sync"
	"time"
)

// CircuitBreakerState represents the state of the circuit breaker
type CircuitBreakerState int

const (
	StateClosed CircuitBreakerState = iota
	StateOpen
	StateHalfOpen
)

// CircuitBreaker implements a simple circuit breaker pattern
// for improving resilience in distributed systems
type CircuitBreaker struct {
	mu               sync.RWMutex
	state            CircuitBreakerState
	failureCount     int
	lastFailureTime  time.Time
	successCount     int
	
	// Configuration
	failureThreshold int
	recoveryTimeout  time.Duration
	halfOpenRequests int
}

// NewCircuitBreaker creates a new circuit breaker with the given configuration
func NewCircuitBreaker(failureThreshold int, recoveryTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: failureThreshold,
		recoveryTimeout:  recoveryTimeout,
		halfOpenRequests: 3, // Allow 3 requests in half-open state
	}
}

// Execute runs the given function with circuit breaker protection
func (cb *CircuitBreaker) Execute(fn func() error) error {
	if !cb.canExecute() {
		return errors.New("circuit breaker is open")
	}
	
	err := fn()
	cb.recordResult(err == nil)
	return err
}

// canExecute checks if the request can be executed based on circuit breaker state
func (cb *CircuitBreaker) canExecute() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	
	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		// Check if recovery timeout has passed
		if time.Since(cb.lastFailureTime) > cb.recoveryTimeout {
			cb.state = StateHalfOpen
			cb.successCount = 0
			return true
		}
		return false
	case StateHalfOpen:
		// Allow limited requests in half-open state
		return cb.successCount < cb.halfOpenRequests
	default:
		return false
	}
}

// recordResult updates the circuit breaker state based on the operation result
func (cb *CircuitBreaker) recordResult(success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	
	if success {
		cb.onSuccess()
	} else {
		cb.onFailure()
	}
}

func (cb *CircuitBreaker) onSuccess() {
	cb.failureCount = 0
	
	switch cb.state {
	case StateHalfOpen:
		cb.successCount++
		if cb.successCount >= cb.halfOpenRequests {
			cb.state = StateClosed
		}
	}
}

func (cb *CircuitBreaker) onFailure() {
	cb.failureCount++
	cb.lastFailureTime = time.Now()
	
	switch cb.state {
	case StateClosed:
		if cb.failureCount >= cb.failureThreshold {
			cb.state = StateOpen
		}
	case StateHalfOpen:
		cb.state = StateOpen
	}
}

// GetState returns the current state of the circuit breaker
func (cb *CircuitBreaker) GetState() CircuitBreakerState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// GetMetrics returns current metrics for monitoring
func (cb *CircuitBreaker) GetMetrics() map[string]interface{} {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	
	return map[string]interface{}{
		"state":           cb.state,
		"failure_count":   cb.failureCount,
		"success_count":   cb.successCount,
		"last_failure":    cb.lastFailureTime,
	}
}

// Reset resets the circuit breaker to closed state
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	
	cb.state = StateClosed
	cb.failureCount = 0
	cb.successCount = 0
}

// Example usage for ZMQ lease coordination with circuit breaker
type ResilientLeaseClient struct {
	zmqClient     LeaseClient
	httpClient    LeaseClient
	circuitBreaker *CircuitBreaker
}

// Interface for lease clients (ZMQ and HTTP)
type LeaseClient interface {
	AcquireLease(id, ttl string) (bool, string, error)
	ReleaseLease(id string) (bool, error)
}

// NewResilientLeaseClient creates a lease client with circuit breaker protection
func NewResilientLeaseClient(zmqClient, httpClient LeaseClient) *ResilientLeaseClient {
	return &ResilientLeaseClient{
		zmqClient:      zmqClient,
		httpClient:     httpClient,
		circuitBreaker: NewCircuitBreaker(3, 30*time.Second), // 3 failures, 30s recovery
	}
}

// AcquireLease attempts to acquire a lease with circuit breaker protection
func (r *ResilientLeaseClient) AcquireLease(id, ttl string) (bool, string, error) {
	// Try ZMQ with circuit breaker protection
	var success bool
	var token string
	var err error
	
	cbErr := r.circuitBreaker.Execute(func() error {
		success, token, err = r.zmqClient.AcquireLease(id, ttl)
		return err
	})
	
	// If circuit breaker allows and operation succeeds, return ZMQ result
	if cbErr == nil && err == nil {
		return success, token, nil
	}
	
	// Fallback to HTTP client
	return r.httpClient.AcquireLease(id, ttl)
}

// ReleaseLease attempts to release a lease with circuit breaker protection
func (r *ResilientLeaseClient) ReleaseLease(id string) (bool, error) {
	// Try ZMQ with circuit breaker protection
	var success bool
	var err error
	
	cbErr := r.circuitBreaker.Execute(func() error {
		success, err = r.zmqClient.ReleaseLease(id)
		return err
	})
	
	// If circuit breaker allows and operation succeeds, return ZMQ result
	if cbErr == nil && err == nil {
		return success, nil
	}
	
	// Fallback to HTTP client
	return r.httpClient.ReleaseLease(id)
}