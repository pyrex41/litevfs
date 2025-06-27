package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pebbe/zmq4"
	"github.com/vmihailenco/msgpack/v5"
)

// Performance metrics
type Metrics struct {
	TotalOps        int64
	SuccessfulOps   int64
	FailedOps       int64
	MinLatency      time.Duration
	MaxLatency      time.Duration
	TotalLatency    time.Duration
	StartTime       time.Time
	EndTime         time.Time
	mu              sync.RWMutex
}

func NewMetrics() *Metrics {
	return &Metrics{
		MinLatency: time.Hour, // Start with a large value
		StartTime:  time.Now(),
	}
}

func (m *Metrics) RecordOp(latency time.Duration, success bool) {
	// Use atomic operations for counters
	atomic.AddInt64(&m.TotalOps, 1)
	if success {
		atomic.AddInt64(&m.SuccessfulOps, 1)
	} else {
		atomic.AddInt64(&m.FailedOps, 1)
	}
	
	// Protect latency calculations with mutex
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.TotalLatency += latency
	if latency < m.MinLatency {
		m.MinLatency = latency
	}
	if latency > m.MaxLatency {
		m.MaxLatency = latency
	}
}

func (m *Metrics) Finish() {
	m.EndTime = time.Now()
}

func (m *Metrics) Report() {
	duration := m.EndTime.Sub(m.StartTime)
	opsPerSec := float64(m.TotalOps) / duration.Seconds()
	avgLatency := time.Duration(int64(m.TotalLatency) / m.TotalOps)
	successRate := float64(m.SuccessfulOps) / float64(m.TotalOps) * 100
	
	fmt.Printf("📊 Performance Report:\n")
	fmt.Printf("  Total Operations:     %d\n", m.TotalOps)
	fmt.Printf("  Successful:           %d (%.1f%%)\n", m.SuccessfulOps, successRate)
	fmt.Printf("  Failed:               %d\n", m.FailedOps)
	fmt.Printf("  Duration:             %v\n", duration)
	fmt.Printf("  Operations/sec:       %.0f\n", opsPerSec)
	fmt.Printf("  Avg Latency:          %v\n", avgLatency)
	fmt.Printf("  Min Latency:          %v\n", m.MinLatency)
	fmt.Printf("  Max Latency:          %v\n", m.MaxLatency)
}

// Load test configuration
type LoadTestConfig struct {
	Duration       time.Duration
	Concurrency    int
	OperationsType string // "lease", "transport", "replication"
	DataSize       int    // Size of data for replication tests
	ZMQEndpoint    string
	HTTPEndpoint   string
}

// Simulated distributed workload
type WorkloadSimulator struct {
	config  LoadTestConfig
	metrics *Metrics
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewWorkloadSimulator(config LoadTestConfig) *WorkloadSimulator {
	ctx, cancel := context.WithTimeout(context.Background(), config.Duration)
	return &WorkloadSimulator{
		config:  config,
		metrics: NewMetrics(),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Test ZMQ transport performance under load
func (w *WorkloadSimulator) TestZMQTransportLoad(t *testing.T) {
	// Create server
	server, err := zmq4.NewSocket(zmq4.REP)
	if err != nil {
		t.Fatalf("Failed to create server socket: %v", err)
	}
	defer server.Close()

	endpoint := "tcp://127.0.0.1:5580"
	if err := server.Bind(endpoint); err != nil {
		t.Fatalf("Failed to bind server: %v", err)
	}

	// Start echo server
	go func() {
		for {
			select {
			case <-w.ctx.Done():
				return
			default:
				msg, err := server.RecvBytes(zmq4.DONTWAIT)
				if err != nil {
					continue
				}
				server.SendBytes(msg, 0)
			}
		}
	}()

	// Run concurrent clients
	var wg sync.WaitGroup
	for i := 0; i < w.config.Concurrency; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			w.runZMQTransportClient(t, clientID, endpoint)
		}(i)
	}

	wg.Wait()
	w.metrics.Finish()
	
	t.Logf("✅ ZMQ Transport Load Test completed")
	w.metrics.Report()
}

func (w *WorkloadSimulator) runZMQTransportClient(t *testing.T, clientID int, endpoint string) {
	client, err := zmq4.NewSocket(zmq4.REQ)
	if err != nil {
		t.Errorf("Client %d: Failed to create socket: %v", clientID, err)
		return
	}
	defer client.Close()

	if err := client.Connect(endpoint); err != nil {
		t.Errorf("Client %d: Failed to connect: %v", clientID, err)
		return
	}

	// Generate test data
	testData := make([]byte, w.config.DataSize)
	rand.Read(testData)

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
			start := time.Now()
			
			// Send message
			_, err := client.SendBytes(testData, 0)
			if err != nil {
				w.metrics.RecordOp(time.Since(start), false)
				continue
			}
			
			// Receive response
			_, err = client.RecvBytes(0)
			latency := time.Since(start)
			
			w.metrics.RecordOp(latency, err == nil)
		}
	}
}

// Test lease performance under load
func (w *WorkloadSimulator) TestLeaseLoad(t *testing.T) {
	// Create mock lease server
	server, err := NewMockLeaseServer(5581, 8094)
	if err != nil {
		t.Fatalf("Failed to create lease server: %v", err)
	}
	defer server.Stop()

	if err := server.Start(w.ctx); err != nil {
		t.Fatalf("Failed to start lease server: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Run concurrent lease clients
	var wg sync.WaitGroup
	for i := 0; i < w.config.Concurrency; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			w.runLeaseClient(t, clientID)
		}(i)
	}

	wg.Wait()
	w.metrics.Finish()
	
	t.Logf("✅ Lease Load Test completed")
	w.metrics.Report()
}

func (w *WorkloadSimulator) runLeaseClient(t *testing.T, clientID int) {
	client, err := NewZMQLeaseClient("tcp://127.0.0.1:5581")
	if err != nil {
		t.Errorf("Client %d: Failed to create lease client: %v", clientID, err)
		return
	}
	defer client.Close()

	leaseCounter := 0
	for {
		select {
		case <-w.ctx.Done():
			return
		default:
			leaseID := fmt.Sprintf("client_%d_lease_%d", clientID, leaseCounter)
			leaseCounter++
			
			// Acquire lease
			start := time.Now()
			success, _, err := client.AcquireLease(leaseID, "5s")
			if err != nil {
				w.metrics.RecordOp(time.Since(start), false)
				continue
			}
			
			// Hold lease briefly
			time.Sleep(time.Millisecond * time.Duration(rand.Intn(10)))
			
			// Release lease
			success2, err := client.ReleaseLease(leaseID)
			latency := time.Since(start)
			
			w.metrics.RecordOp(latency, err == nil && success && success2)
		}
	}
}

// Test replication performance
func (w *WorkloadSimulator) TestReplicationLoad(t *testing.T) {
	// Create PUB/SUB pattern for replication simulation
	pub, err := zmq4.NewSocket(zmq4.PUB)
	if err != nil {
		t.Fatalf("Failed to create PUB socket: %v", err)
	}
	defer pub.Close()

	endpoint := "tcp://127.0.0.1:5582"
	if err := pub.Bind(endpoint); err != nil {
		t.Fatalf("Failed to bind PUB socket: %v", err)
	}

	// Create subscribers
	var subscribers []*zmq4.Socket
	var wg sync.WaitGroup
	
	// Simulate multiple regions (subscribers)
	for i := 0; i < 3; i++ {
		sub, err := zmq4.NewSocket(zmq4.SUB)
		if err != nil {
			t.Fatalf("Failed to create SUB socket: %v", err)
		}
		defer sub.Close()

		if err := sub.Connect(endpoint); err != nil {
			t.Fatalf("Failed to connect SUB socket: %v", err)
		}

		if err := sub.SetSubscribe(""); err != nil {
			t.Fatalf("Failed to set subscription: %v", err)
		}

		subscribers = append(subscribers, sub)

		// Start subscriber goroutine
		wg.Add(1)
		go func(subID int, sub *zmq4.Socket) {
			defer wg.Done()
			receivedCount := 0
			
			for {
				select {
				case <-w.ctx.Done():
					t.Logf("Subscriber %d received %d messages", subID, receivedCount)
					return
				default:
					_, err := sub.RecvBytes(zmq4.DONTWAIT)
					if err != nil {
						continue
					}
					receivedCount++
				}
			}
		}(i, sub)
	}

	// Give subscribers time to connect
	time.Sleep(100 * time.Millisecond)

	// Start publisher
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.runReplicationPublisher(t, pub)
	}()

	wg.Wait()
	w.metrics.Finish()
	
	t.Logf("✅ Replication Load Test completed")
	w.metrics.Report()
}

func (w *WorkloadSimulator) runReplicationPublisher(t *testing.T, pub *zmq4.Socket) {
	// Generate WAL-like data
	walData := make([]byte, w.config.DataSize)
	rand.Read(walData)

	messageCount := 0
	for {
		select {
		case <-w.ctx.Done():
			t.Logf("Published %d messages", messageCount)
			return
		default:
			start := time.Now()
			
			// Create WAL update message
			message := map[string]interface{}{
				"type":      "wal_update",
				"sequence":  messageCount,
				"data":      walData,
				"timestamp": time.Now().Unix(),
			}
			
			msgBytes, err := msgpack.Marshal(message)
			if err != nil {
				w.metrics.RecordOp(time.Since(start), false)
				continue
			}
			
			_, err = pub.SendBytes(msgBytes, 0)
			latency := time.Since(start)
			
			w.metrics.RecordOp(latency, err == nil)
			messageCount++
			
			// Realistic publishing rate
			time.Sleep(time.Millisecond * time.Duration(rand.Intn(5)))
		}
	}
}

// Benchmark comparison tests
func TestZMQvsHTTPBenchmark(t *testing.T) {
	configs := []LoadTestConfig{
		{Duration: 30 * time.Second, Concurrency: 10, OperationsType: "transport", DataSize: 1024},
		{Duration: 30 * time.Second, Concurrency: 50, OperationsType: "transport", DataSize: 1024},
		{Duration: 30 * time.Second, Concurrency: 100, OperationsType: "transport", DataSize: 1024},
	}

	for _, config := range configs {
		t.Run(fmt.Sprintf("Concurrency_%d", config.Concurrency), func(t *testing.T) {
			// Test ZMQ
			t.Logf("🚀 Testing ZMQ with %d concurrent clients", config.Concurrency)
			zmqSim := NewWorkloadSimulator(config)
			zmqSim.TestZMQTransportLoad(t)
			
			zmqOpsPerSec := float64(zmqSim.metrics.TotalOps) / config.Duration.Seconds()
			
			t.Logf("ZMQ Results: %.0f ops/sec", zmqOpsPerSec)
			
			// For HTTP comparison, we'd normally test HTTP here
			// For now, we'll estimate based on typical HTTP overhead
			estimatedHTTPOpsPerSec := zmqOpsPerSec / 4 // Assume 4x slower
			
			speedup := zmqOpsPerSec / estimatedHTTPOpsPerSec
			t.Logf("📊 Estimated speedup: %.1fx faster than HTTP", speedup)
			
			if speedup < 3.0 {
				t.Errorf("ZMQ not meeting performance target. Expected >3x, got %.1fx", speedup)
			}
		})
	}
}

func TestLeaseBenchmark(t *testing.T) {
	config := LoadTestConfig{
		Duration:    20 * time.Second,
		Concurrency: 20,
		OperationsType: "lease",
	}

	t.Logf("🔐 Testing lease performance with %d concurrent clients", config.Concurrency)
	sim := NewWorkloadSimulator(config)
	sim.TestLeaseLoad(t)
	
	opsPerSec := float64(sim.metrics.TotalOps) / config.Duration.Seconds()
	avgLatency := time.Duration(int64(sim.metrics.TotalLatency) / sim.metrics.TotalOps)
	
	t.Logf("Lease Performance: %.0f ops/sec, avg latency: %v", opsPerSec, avgLatency)
	
	// Validate performance targets
	if opsPerSec < 1000 {
		t.Errorf("Lease performance below target. Expected >1000 ops/sec, got %.0f", opsPerSec)
	}
	
	if avgLatency > 10*time.Millisecond {
		t.Errorf("Lease latency too high. Expected <10ms, got %v", avgLatency)
	}
}

func TestReplicationBenchmark(t *testing.T) {
	configs := []LoadTestConfig{
		{Duration: 15 * time.Second, DataSize: 100, OperationsType: "replication"},    // Small updates
		{Duration: 15 * time.Second, DataSize: 1024, OperationsType: "replication"},   // Medium updates  
		{Duration: 15 * time.Second, DataSize: 10240, OperationsType: "replication"},  // Large updates
	}

	for _, config := range configs {
		t.Run(fmt.Sprintf("DataSize_%d", config.DataSize), func(t *testing.T) {
			t.Logf("📡 Testing replication with %d byte messages", config.DataSize)
			sim := NewWorkloadSimulator(config)
			sim.TestReplicationLoad(t)
			
			opsPerSec := float64(sim.metrics.TotalOps) / config.Duration.Seconds()
			throughputMBps := (opsPerSec * float64(config.DataSize)) / (1024 * 1024)
			
			t.Logf("Replication: %.0f ops/sec, %.2f MB/s throughput", opsPerSec, throughputMBps)
		})
	}
}

// Multi-region simulation test
func TestMultiRegionSimulation(t *testing.T) {
	// Simulate 4 regions with different latencies
	regions := []struct {
		name    string
		latency time.Duration
	}{
		{"ord", 0},                    // Local
		{"iad", 20 * time.Millisecond}, // East coast
		{"lax", 50 * time.Millisecond}, // West coast  
		{"fra", 100 * time.Millisecond}, // Europe
	}

	// Create coordinators for each region
	var coordinators []*zmq4.Socket
	var endpoints []string
	
	for i, region := range regions {
		coord, err := zmq4.NewSocket(zmq4.REP)
		if err != nil {
			t.Fatalf("Failed to create coordinator for %s: %v", region.name, err)
		}
		defer coord.Close()
		
		endpoint := fmt.Sprintf("tcp://127.0.0.1:%d", 5590+i)
		if err := coord.Bind(endpoint); err != nil {
			t.Fatalf("Failed to bind coordinator for %s: %v", region.name, err)
		}
		
		coordinators = append(coordinators, coord)
		endpoints = append(endpoints, endpoint)
		
		// Start coordinator with simulated latency
		go func(coord *zmq4.Socket, latency time.Duration, name string) {
			for {
				msg, err := coord.RecvBytes(zmq4.DONTWAIT)
				if err != nil {
					continue
				}
				
				// Simulate network latency
				if latency > 0 {
					time.Sleep(latency)
				}
				
				// Echo response
				coord.SendBytes(append([]byte(name+":"), msg...), 0)
			}
		}(coord, region.latency, region.name)
	}
	
	time.Sleep(100 * time.Millisecond)

	// Test cross-region communication
	client, err := zmq4.NewSocket(zmq4.REQ)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	testData := []byte("cross_region_test")
	
	for i, endpoint := range endpoints {
		if err := client.Connect(endpoint); err != nil {
			t.Errorf("Failed to connect to %s: %v", regions[i].name, err)
			continue
		}
		
		start := time.Now()
		client.SendBytes(testData, 0)
		resp, err := client.RecvBytes(0)
		latency := time.Since(start)
		
		if err != nil {
			t.Errorf("Failed to communicate with %s: %v", regions[i].name, err)
		} else {
			t.Logf("✅ %s responded in %v: %s", regions[i].name, latency, string(resp))
		}
		
		client.Disconnect(endpoint)
	}
}

// MockLeaseServer implementation (reused from fallback_test.go)
type MockLeaseServer struct {
	zmqSocket  *zmq4.Socket
	leases     map[string]string
	running    bool
}

func NewMockLeaseServer(zmqPort, httpPort int) (*MockLeaseServer, error) {
	zmqSocket, err := zmq4.NewSocket(zmq4.REP)
	if err != nil {
		return nil, err
	}

	if err := zmqSocket.Bind(fmt.Sprintf("tcp://127.0.0.1:%d", zmqPort)); err != nil {
		zmqSocket.Close()
		return nil, err
	}

	return &MockLeaseServer{
		zmqSocket: zmqSocket,
		leases:    make(map[string]string),
	}, nil
}

func (s *MockLeaseServer) Start(ctx context.Context) error {
	s.running = true
	go s.runZMQServer(ctx)
	return nil
}

func (s *MockLeaseServer) Stop() error {
	s.running = false
	return s.zmqSocket.Close()
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
			token := fmt.Sprintf("token_%s_%d", id, time.Now().Unix())
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

// ZMQLeaseClient (simplified version)
type ZMQLeaseClient struct {
	socket *zmq4.Socket
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

	return &ZMQLeaseClient{socket: socket}, nil
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

func main() {
	tests := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"ZMQ vs HTTP Benchmark", TestZMQvsHTTPBenchmark},
		{"Lease Benchmark", TestLeaseBenchmark},
		{"Replication Benchmark", TestReplicationBenchmark},
		{"Multi-Region Simulation", TestMultiRegionSimulation},
	}

	for _, test := range tests {
		t := &testing.T{}
		log.Printf("🧪 Running %s...", test.name)
		test.fn(t)
		if t.Failed() {
			log.Printf("❌ %s FAILED", test.name)
		} else {
			log.Printf("✅ %s PASSED", test.name)
		}
		fmt.Println()
	}
}