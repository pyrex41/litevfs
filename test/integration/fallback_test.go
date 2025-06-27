package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"testing"
	"time"

	"github.com/pebbe/zmq4"
	"github.com/vmihailenco/msgpack/v5"
)

// LeaseClient interface for testing both ZMQ and HTTP implementations
type LeaseClient interface {
	AcquireLease(id, ttl string) (bool, string, error)
	ReleaseLease(id string) (bool, error)
	Close() error
}

// ZMQLeaseClient implements lease client over ZMQ
type ZMQLeaseClient struct {
	socket   *zmq4.Socket
	endpoint string
}

func NewZMQLeaseClient(endpoint string) (*ZMQLeaseClient, error) {
	socket, err := zmq4.NewSocket(zmq4.REQ)
	if err != nil {
		return nil, err
	}

	if err := socket.Connect(endpoint); err != nil {
		socket.Close()
		return nil, err
	}

	return &ZMQLeaseClient{socket: socket, endpoint: endpoint}, nil
}

func (c *ZMQLeaseClient) AcquireLease(id, ttl string) (bool, string, error) {
	msg := map[string]interface{}{
		"type": "lease_acquire",
		"payload": map[string]interface{}{
			"id":  id,
			"ttl": ttl,
		},
	}

	msgBytes, err := msgpack.Marshal(msg)
	if err != nil {
		return false, "", err
	}

	if _, err := c.socket.SendBytes(msgBytes, 0); err != nil {
		return false, "", err
	}

	respBytes, err := c.socket.RecvBytes(0)
	if err != nil {
		return false, "", err
	}

	var response map[string]interface{}
	if err := msgpack.Unmarshal(respBytes, &response); err != nil {
		return false, "", err
	}

	success, _ := response["success"].(bool)
	token, _ := response["token"].(string)
	return success, token, nil
}

func (c *ZMQLeaseClient) ReleaseLease(id string) (bool, error) {
	msg := map[string]interface{}{
		"type": "lease_release",
		"payload": map[string]interface{}{
			"id": id,
		},
	}

	msgBytes, err := msgpack.Marshal(msg)
	if err != nil {
		return false, err
	}

	if _, err := c.socket.SendBytes(msgBytes, 0); err != nil {
		return false, err
	}

	respBytes, err := c.socket.RecvBytes(0)
	if err != nil {
		return false, err
	}

	var response map[string]interface{}
	if err := msgpack.Unmarshal(respBytes, &response); err != nil {
		return false, err
	}

	success, _ := response["success"].(bool)
	return success, nil
}

func (c *ZMQLeaseClient) Close() error {
	return c.socket.Close()
}

// HTTPLeaseClient implements lease client over HTTP
type HTTPLeaseClient struct {
	baseURL string
	client  *http.Client
}

func NewHTTPLeaseClient(baseURL string) *HTTPLeaseClient {
	return &HTTPLeaseClient{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *HTTPLeaseClient) AcquireLease(id, ttl string) (bool, string, error) {
	payload := map[string]string{"id": id, "ttl": ttl}
	jsonBytes, _ := json.Marshal(payload)

	resp, err := c.client.Post(c.baseURL+"/lease/acquire", "application/json", bytes.NewBuffer(jsonBytes))
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, "", err
	}

	success, _ := result["success"].(bool)
	token, _ := result["token"].(string)
	return success, token, nil
}

func (c *HTTPLeaseClient) ReleaseLease(id string) (bool, error) {
	payload := map[string]string{"id": id}
	jsonBytes, _ := json.Marshal(payload)

	resp, err := c.client.Post(c.baseURL+"/lease/release", "application/json", bytes.NewBuffer(jsonBytes))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}

	success, _ := result["success"].(bool)
	return success, nil
}

func (c *HTTPLeaseClient) Close() error {
	return nil // HTTP client doesn't need cleanup
}

// FallbackLeaseClient implements ZMQ-first with HTTP fallback
type FallbackLeaseClient struct {
	zmqClient  LeaseClient
	httpClient LeaseClient
}

func NewFallbackLeaseClient(zmqEndpoint, httpEndpoint string) (*FallbackLeaseClient, error) {
	zmqClient, err := NewZMQLeaseClient(zmqEndpoint)
	if err != nil {
		return nil, err
	}

	httpClient := NewHTTPLeaseClient(httpEndpoint)

	return &FallbackLeaseClient{
		zmqClient:  zmqClient,
		httpClient: httpClient,
	}, nil
}

func (c *FallbackLeaseClient) AcquireLease(id, ttl string) (bool, string, error) {
	// Try ZMQ first
	success, token, err := c.zmqClient.AcquireLease(id, ttl)
	if err == nil {
		return success, token, nil
	}

	// Fallback to HTTP
	return c.httpClient.AcquireLease(id, ttl)
}

func (c *FallbackLeaseClient) ReleaseLease(id string) (bool, error) {
	// Try ZMQ first
	success, err := c.zmqClient.ReleaseLease(id)
	if err == nil {
		return success, nil
	}

	// Fallback to HTTP
	return c.httpClient.ReleaseLease(id)
}

func (c *FallbackLeaseClient) Close() error {
	c.zmqClient.Close()
	return c.httpClient.Close()
}

// Mock lease server for testing
type MockLeaseServer struct {
	zmqSocket  *zmq4.Socket
	httpServer *http.Server
	leases     map[string]string
	zmqPort    int
	httpPort   int
	running    bool
}

func NewMockLeaseServer(zmqPort, httpPort int) (*MockLeaseServer, error) {
	// Create ZMQ socket
	zmqSocket, err := zmq4.NewSocket(zmq4.REP)
	if err != nil {
		return nil, err
	}

	if err := zmqSocket.Bind(fmt.Sprintf("tcp://127.0.0.1:%d", zmqPort)); err != nil {
		zmqSocket.Close()
		return nil, err
	}

	server := &MockLeaseServer{
		zmqSocket: zmqSocket,
		leases:    make(map[string]string),
		zmqPort:   zmqPort,
		httpPort:  httpPort,
	}

	// Setup HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/lease/acquire", server.handleHTTPAcquire)
	mux.HandleFunc("/lease/release", server.handleHTTPRelease)

	server.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", httpPort),
		Handler: mux,
	}

	return server, nil
}

func (s *MockLeaseServer) Start(ctx context.Context) error {
	s.running = true

	// Start ZMQ server
	go s.runZMQServer(ctx)

	// Start HTTP server
	go func() {
		if err := s.httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	return nil
}

func (s *MockLeaseServer) Stop() error {
	s.running = false
	s.zmqSocket.Close()
	return s.httpServer.Shutdown(context.Background())
}

func (s *MockLeaseServer) DisableZMQ() {
	// Simulate ZMQ failure by closing socket
	s.zmqSocket.Close()
}

func (s *MockLeaseServer) runZMQServer(ctx context.Context) {
	for s.running {
		select {
		case <-ctx.Done():
			return
		default:
			msgBytes, err := s.zmqSocket.RecvBytes(zmq4.DONTWAIT)
			if err != nil {
				continue
			}

			var msg map[string]interface{}
			if err := msgpack.Unmarshal(msgBytes, &msg); err != nil {
				continue
			}

			response := s.processLeaseRequest(msg)
			respBytes, _ := msgpack.Marshal(response)
			s.zmqSocket.SendBytes(respBytes, 0)
		}
	}
}

func (s *MockLeaseServer) processLeaseRequest(msg map[string]interface{}) map[string]interface{} {
	msgType, _ := msg["type"].(string)
	payload, _ := msg["payload"].(map[string]interface{})

	switch msgType {
	case "lease_acquire":
		id, _ := payload["id"].(string)
		if _, exists := s.leases[id]; !exists {
			token := fmt.Sprintf("zmq_token_%s_%d", id, time.Now().Unix())
			s.leases[id] = token
			return map[string]interface{}{"success": true, "token": token}
		}
		return map[string]interface{}{"success": false, "error": "lease already held"}

	case "lease_release":
		id, _ := payload["id"].(string)
		if _, exists := s.leases[id]; exists {
			delete(s.leases, id)
			return map[string]interface{}{"success": true}
		}
		return map[string]interface{}{"success": false, "error": "lease not found"}
	}

	return map[string]interface{}{"success": false, "error": "unknown message type"}
}

func (s *MockLeaseServer) handleHTTPAcquire(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	json.NewDecoder(r.Body).Decode(&req)

	id := req["id"]
	if _, exists := s.leases[id]; !exists {
		token := fmt.Sprintf("http_token_%s_%d", id, time.Now().Unix())
		s.leases[id] = token
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "token": token})
	} else {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "lease already held"})
	}
}

func (s *MockLeaseServer) handleHTTPRelease(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	json.NewDecoder(r.Body).Decode(&req)

	id := req["id"]
	if _, exists := s.leases[id]; exists {
		delete(s.leases, id)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	} else {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "lease not found"})
	}
}

// Test functions

func TestZMQLeaseClientBasic(t *testing.T) {
	server, err := NewMockLeaseServer(5570, 8090)
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer server.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := server.Start(ctx); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // Let server start

	client, err := NewZMQLeaseClient("tcp://127.0.0.1:5570")
	if err != nil {
		t.Fatalf("Failed to create ZMQ client: %v", err)
	}
	defer client.Close()

	// Test lease acquisition
	success, token, err := client.AcquireLease("test_lease", "10s")
	if err != nil {
		t.Fatalf("Failed to acquire lease: %v", err)
	}
	if !success {
		t.Error("Lease acquisition should have succeeded")
	}
	if token == "" {
		t.Error("Token should not be empty")
	}

	t.Logf("✅ ZMQ lease acquired: %s", token)

	// Test lease release
	success, err = client.ReleaseLease("test_lease")
	if err != nil {
		t.Fatalf("Failed to release lease: %v", err)
	}
	if !success {
		t.Error("Lease release should have succeeded")
	}

	t.Logf("✅ ZMQ lease client basic test passed")
}

func TestHTTPLeaseClientBasic(t *testing.T) {
	server, err := NewMockLeaseServer(5571, 8091)
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer server.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := server.Start(ctx); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // Let server start

	client := NewHTTPLeaseClient("http://127.0.0.1:8091")
	defer client.Close()

	// Test lease acquisition
	success, token, err := client.AcquireLease("test_lease", "10s")
	if err != nil {
		t.Fatalf("Failed to acquire lease: %v", err)
	}
	if !success {
		t.Error("Lease acquisition should have succeeded")
	}
	if token == "" {
		t.Error("Token should not be empty")
	}

	t.Logf("✅ HTTP lease acquired: %s", token)

	// Test lease release
	success, err = client.ReleaseLease("test_lease")
	if err != nil {
		t.Fatalf("Failed to release lease: %v", err)
	}
	if !success {
		t.Error("Lease release should have succeeded")
	}

	t.Logf("✅ HTTP lease client basic test passed")
}

func TestFallbackMechanism(t *testing.T) {
	server, err := NewMockLeaseServer(5572, 8092)
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer server.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := server.Start(ctx); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // Let server start

	client, err := NewFallbackLeaseClient("tcp://127.0.0.1:5572", "http://127.0.0.1:8092")
	if err != nil {
		t.Fatalf("Failed to create fallback client: %v", err)
	}
	defer client.Close()

	// Test 1: Normal operation (ZMQ should work)
	success, token, err := client.AcquireLease("test_lease_1", "10s")
	if err != nil {
		t.Fatalf("Failed to acquire lease: %v", err)
	}
	if !success {
		t.Error("Lease acquisition should have succeeded")
	}
	if !bytes.Contains([]byte(token), []byte("zmq_token")) {
		t.Errorf("Expected ZMQ token, got: %s", token)
	}

	t.Logf("✅ ZMQ lease acquired: %s", token)

	// Release lease
	success, err = client.ReleaseLease("test_lease_1")
	if err != nil || !success {
		t.Error("Failed to release ZMQ lease")
	}

	// Test 2: Simulate ZMQ failure, should fallback to HTTP
	server.DisableZMQ()
	time.Sleep(100 * time.Millisecond)

	success, token, err = client.AcquireLease("test_lease_2", "10s")
	if err != nil {
		t.Fatalf("Fallback failed: %v", err)
	}
	if !success {
		t.Error("Fallback lease acquisition should have succeeded")
	}
	if !bytes.Contains([]byte(token), []byte("http_token")) {
		t.Errorf("Expected HTTP token, got: %s", token)
	}

	t.Logf("✅ HTTP fallback lease acquired: %s", token)

	// Release fallback lease
	success, err = client.ReleaseLease("test_lease_2")
	if err != nil || !success {
		t.Error("Failed to release HTTP fallback lease")
	}

	t.Logf("✅ Fallback mechanism test passed")
}

func TestPerformanceComparison(t *testing.T) {
	server, err := NewMockLeaseServer(5573, 8093)
	if err != nil {
		t.Fatalf("Failed to create mock server: %v", err)
	}
	defer server.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := server.Start(ctx); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Create clients
	zmqClient, err := NewZMQLeaseClient("tcp://127.0.0.1:5573")
	if err != nil {
		t.Fatalf("Failed to create ZMQ client: %v", err)
	}
	defer zmqClient.Close()

	httpClient := NewHTTPLeaseClient("http://127.0.0.1:8093")
	defer httpClient.Close()

	const iterations = 100

	// Benchmark ZMQ performance
	start := time.Now()
	for i := 0; i < iterations; i++ {
		leaseID := fmt.Sprintf("zmq_lease_%d", i)
		zmqClient.AcquireLease(leaseID, "10s")
		zmqClient.ReleaseLease(leaseID)
	}
	zmqDuration := time.Since(start)

	// Benchmark HTTP performance
	start = time.Now()
	for i := 0; i < iterations; i++ {
		leaseID := fmt.Sprintf("http_lease_%d", i)
		httpClient.AcquireLease(leaseID, "10s")
		httpClient.ReleaseLease(leaseID)
	}
	httpDuration := time.Since(start)

	speedup := float64(httpDuration) / float64(zmqDuration)

	t.Logf("Performance comparison (%d lease cycles):", iterations)
	t.Logf("  ZMQ:  %v", zmqDuration)
	t.Logf("  HTTP: %v", httpDuration)
	t.Logf("  Speedup: %.1fx", speedup)

	if speedup < 1.5 {
		t.Errorf("ZMQ not significantly faster than HTTP. Expected >1.5x, got %.1fx", speedup)
	}

	t.Logf("✅ Performance comparison passed: ZMQ is %.1fx faster", speedup)
}

func main() {
	tests := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"ZMQ Lease Client Basic", TestZMQLeaseClientBasic},
		{"HTTP Lease Client Basic", TestHTTPLeaseClientBasic},
		{"Fallback Mechanism", TestFallbackMechanism},
		{"Performance Comparison", TestPerformanceComparison},
	}

	for _, test := range tests {
		t := &testing.T{}
		log.Printf("Running %s...", test.name)
		test.fn(t)
		if t.Failed() {
			log.Printf("❌ %s FAILED", test.name)
		} else {
			log.Printf("✅ %s PASSED", test.name)
		}
	}
}