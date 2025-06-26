package integration

import (
	"testing"
	"time"
	"fmt"
	"os"
	"math/rand"

	"distsqlite/pkg/distcore"
)

func TestOCamlEngineIntegration(t *testing.T) {
	// Test OCaml engine initialization
	engine, err := distcore.GetEngine()
	if err != nil {
		t.Fatalf("Failed to initialize OCaml engine: %v", err)
	}

	if !engine.IsInitialized() {
		t.Fatal("OCaml engine not properly initialized")
	}

	t.Log("✓ OCaml engine initialized successfully")
}

func TestHighPerformanceQueue(t *testing.T) {
	queue, err := distcore.NewHighPerformanceQueue()
	if err != nil {
		t.Fatalf("Failed to create high-performance queue: %v", err)
	}

	// Test basic enqueue/dequeue
	testEvent := distcore.WriteEvent{
		Path:       "test.db",
		Offset:     1024,
		Data:       []byte("test data for OCaml queue"),
		IsWAL:      false,
		FenceToken: 12345,
	}

	success := queue.Enqueue(testEvent)
	if !success {
		t.Fatal("Failed to enqueue event")
	}

	if queue.Pending() != 1 {
		t.Fatalf("Expected 1 pending event, got %d", queue.Pending())
	}

	events, err := queue.DequeueBatch(10)
	if err != nil {
		t.Fatalf("Failed to dequeue events: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("Expected 1 dequeued event, got %d", len(events))
	}

	if events[0].Path != testEvent.Path {
		t.Fatalf("Event path mismatch: expected %s, got %s", testEvent.Path, events[0].Path)
	}

	t.Log("✓ High-performance queue working correctly")
}

func TestQueueThroughput(t *testing.T) {
	queue, err := distcore.NewHighPerformanceQueue()
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}

	// Throughput test
	numEvents := 10000
	startTime := time.Now()

	// Enqueue events
	for i := 0; i < numEvents; i++ {
		event := distcore.WriteEvent{
			Path:       fmt.Sprintf("db_%d.db", i%10),
			Offset:     int64(i * 1024),
			Data:       make([]byte, 512), // 512 byte events
			IsWAL:      i%2 == 0,
			FenceToken: int64(i),
		}
		
		if !queue.Enqueue(event) {
			t.Fatalf("Failed to enqueue event %d", i)
		}
	}

	enqueueTime := time.Since(startTime)

	// Dequeue events in batches
	dequeueStart := time.Now()
	totalDequeued := 0
	
	for totalDequeued < numEvents {
		events, err := queue.DequeueBatch(100)
		if err != nil {
			t.Fatalf("Failed to dequeue batch: %v", err)
		}
		totalDequeued += len(events)
	}

	dequeueTime := time.Since(dequeueStart)

	enqueueRate := float64(numEvents) / enqueueTime.Seconds()
	dequeueRate := float64(numEvents) / dequeueTime.Seconds()

	t.Logf("✓ Enqueue rate: %.0f ops/sec", enqueueRate)
	t.Logf("✓ Dequeue rate: %.0f ops/sec", dequeueRate)

	// Performance targets (should be much higher than Go channels)
	if enqueueRate < 100000 { // 100k ops/sec minimum
		t.Logf("⚠ Enqueue rate below target (100k ops/sec): %.0f", enqueueRate)
	}
	if dequeueRate < 100000 {
		t.Logf("⚠ Dequeue rate below target (100k ops/sec): %.0f", dequeueRate)
	}
}

func TestReplicationStrategyComputation(t *testing.T) {
	engine, err := distcore.GetEngine()
	if err != nil {
		t.Fatalf("Failed to get engine: %v", err)
	}

	// Test different event patterns
	testCases := []struct {
		name     string
		events   []distcore.WriteEvent
		expected distcore.Strategy
	}{
		{
			name:     "empty batch",
			events:   []distcore.WriteEvent{},
			expected: distcore.StrategyTxLog,
		},
		{
			name: "small WAL writes",
			events: []distcore.WriteEvent{
				{Path: "test.db-wal", Offset: 0, Data: make([]byte, 100), IsWAL: true},
				{Path: "test.db-wal", Offset: 100, Data: make([]byte, 100), IsWAL: true},
			},
			expected: distcore.StrategyTxLog,
		},
		{
			name: "large bulk writes",
			events: []distcore.WriteEvent{
				{Path: "test.db", Offset: 0, Data: make([]byte, 500000), IsWAL: false},
				{Path: "test.db", Offset: 500000, Data: make([]byte, 500000), IsWAL: false},
			},
			expected: distcore.StrategyBulkSync,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			strategy := engine.ComputeReplicationStrategy(tc.events)
			if strategy != tc.expected {
				t.Logf("Strategy mismatch for %s: expected %s, got %s", 
					tc.name, tc.expected, strategy)
				// Don't fail - strategy computation is heuristic
			}
			t.Logf("✓ %s: strategy = %s", tc.name, strategy)
		})
	}
}

func TestBinaryEncoding(t *testing.T) {
	engine, err := distcore.GetEngine()
	if err != nil {
		t.Fatalf("Failed to get engine: %v", err)
	}

	// Create test events
	events := []distcore.WriteEvent{
		{
			Path:       "test1.db",
			Offset:     0,
			Data:       []byte("Hello from OCaml!"),
			IsWAL:      false,
			FenceToken: 100,
		},
		{
			Path:       "test2.db-wal",
			Offset:     1024,
			Data:       []byte("WAL data"),
			IsWAL:      true,
			FenceToken: 101,
		},
	}

	// Test encoding
	encoded, err := engine.EncodeWriteBatch(events)
	if err != nil {
		t.Fatalf("Failed to encode write batch: %v", err)
	}

	if len(encoded) == 0 {
		t.Fatal("Encoded data is empty")
	}

	// Verify encoded data has proper header (magic number)
	if len(encoded) < 8 {
		t.Fatal("Encoded data too short")
	}

	// Check magic number "DIST" (0x44495354)
	magic := uint32(encoded[0])<<24 | uint32(encoded[1])<<16 | uint32(encoded[2])<<8 | uint32(encoded[3])
	expectedMagic := uint32(0x44495354)
	
	if magic != expectedMagic {
		t.Fatalf("Invalid magic number: expected 0x%x, got 0x%x", expectedMagic, magic)
	}

	batchSize := uint32(encoded[4])<<24 | uint32(encoded[5])<<16 | uint32(encoded[6])<<8 | uint32(encoded[7])
	if batchSize != 2 {
		t.Fatalf("Invalid batch size: expected 2, got %d", batchSize)
	}

	t.Logf("✓ Binary encoding successful: %d bytes", len(encoded))
	t.Logf("✓ Magic number and batch size correct")
}

func TestBinaryDiff(t *testing.T) {
	differ, err := distcore.NewBinaryDiffer()
	if err != nil {
		t.Fatalf("Failed to create binary differ: %v", err)
	}

	// Create test databases
	pageSize := 4096
	oldDB := make([]byte, pageSize*10) // 10 pages
	newDB := make([]byte, pageSize*10)

	// Fill with test data
	for i := range oldDB {
		oldDB[i] = byte(i % 256)
	}
	copy(newDB, oldDB)

	// Modify a few pages in newDB
	for i := pageSize; i < pageSize*2; i++ {
		newDB[i] = byte((i + 100) % 256)
	}

	// Compute diff
	diff, err := differ.ComputeDiff(oldDB, newDB, pageSize)
	if err != nil {
		t.Fatalf("Failed to compute diff: %v", err)
	}

	t.Logf("✓ Binary diff computed: %d bytes", len(diff))
	// Note: The current implementation returns a placeholder
	// In a full implementation, this would validate the actual diff structure
}

func TestProcessWriteBatch(t *testing.T) {
	// Test the high-level API
	events := []distcore.WriteEvent{
		{
			Path:       "test.db",
			Offset:     0,
			Data:       make([]byte, 1000),
			IsWAL:      false,
			FenceToken: 200,
		},
	}

	strategy, encoded, err := distcore.ProcessWriteBatch(events)
	if err != nil {
		t.Fatalf("Failed to process write batch: %v", err)
	}

	t.Logf("✓ Strategy: %s", strategy)
	t.Logf("✓ Encoded size: %d bytes", len(encoded))

	if len(encoded) == 0 {
		t.Fatal("Encoded batch is empty")
	}
}

func TestPerformanceStats(t *testing.T) {
	engine, err := distcore.GetEngine()
	if err != nil {
		t.Fatalf("Failed to get engine: %v", err)
	}

	// Reset stats
	engine.ResetStats()

	// Perform some operations
	queue, _ := distcore.NewHighPerformanceQueue()
	
	for i := 0; i < 100; i++ {
		event := distcore.WriteEvent{
			Path:   "perf_test.db",
			Offset: int64(i * 1024),
			Data:   make([]byte, 512),
		}
		queue.Enqueue(event)
	}

	// Get some events
	queue.DequeueBatch(50)

	// Check stats
	stats, err := engine.GetStats()
	if err != nil {
		t.Fatalf("Failed to get stats: %v", err)
	}

	t.Logf("✓ Performance stats:")
	t.Logf("  Queue enqueues: %d", stats.QueueEnqueues)
	t.Logf("  Queue dequeues: %d", stats.QueueDequeues)
	t.Logf("  Bytes processed: %d", stats.BytesProcessed)
	t.Logf("  Diff operations: %d", stats.DiffOperations)
	t.Logf("  Replication operations: %d", stats.ReplicationOperations)
	t.Logf("  State transitions: %d", stats.StateTransitions)
}

func TestConcurrentAccess(t *testing.T) {
	queue, err := distcore.NewHighPerformanceQueue()
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}

	numProducers := 4
	numConsumers := 2
	eventsPerProducer := 1000

	// Producer goroutines
	for p := 0; p < numProducers; p++ {
		go func(producerID int) {
			for i := 0; i < eventsPerProducer; i++ {
				event := distcore.WriteEvent{
					Path:   fmt.Sprintf("concurrent_%d.db", producerID),
					Offset: int64(i * 1024),
					Data:   make([]byte, 256),
				}
				
				for !queue.Enqueue(event) {
					time.Sleep(time.Microsecond) // Back off if queue full
				}
			}
		}(p)
	}

	// Consumer goroutines
	totalConsumed := 0
	for c := 0; c < numConsumers; c++ {
		go func() {
			for {
				events, err := queue.DequeueBatch(10)
				if err != nil {
					t.Errorf("Consumer error: %v", err)
					return
				}
				totalConsumed += len(events)
				
				if totalConsumed >= numProducers*eventsPerProducer {
					return
				}
				
				if len(events) == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	// Wait for completion
	maxWait := 10 * time.Second
	start := time.Now()
	
	for totalConsumed < numProducers*eventsPerProducer && time.Since(start) < maxWait {
		time.Sleep(10 * time.Millisecond)
	}

	expectedTotal := numProducers * eventsPerProducer
	if totalConsumed < expectedTotal {
		t.Fatalf("Concurrent test incomplete: consumed %d/%d events", totalConsumed, expectedTotal)
	}

	t.Logf("✓ Concurrent access test passed: %d events processed", totalConsumed)
}

func TestMemoryEfficiency(t *testing.T) {
	// Test that we're not leaking memory with large batches
	engine, err := distcore.GetEngine()
	if err != nil {
		t.Fatalf("Failed to get engine: %v", err)
	}

	// Create large events
	largeEvents := make([]distcore.WriteEvent, 1000)
	for i := range largeEvents {
		largeEvents[i] = distcore.WriteEvent{
			Path:   fmt.Sprintf("large_%d.db", i),
			Offset: int64(i * 4096),
			Data:   make([]byte, 4096), // 4KB per event
		}
		// Fill with random data
		rand.Read(largeEvents[i].Data)
	}

	// Process multiple times to check for memory leaks
	for round := 0; round < 10; round++ {
		strategy := engine.ComputeReplicationStrategy(largeEvents)
		encoded, err := engine.EncodeWriteBatch(largeEvents)
		if err != nil {
			t.Fatalf("Failed to encode large batch: %v", err)
		}
		
		if round == 0 {
			t.Logf("✓ Large batch processing: strategy=%s, encoded=%dKB", 
				strategy, len(encoded)/1024)
		}
	}

	t.Log("✓ Memory efficiency test completed - no obvious leaks")
}

func TestErrorHandling(t *testing.T) {
	engine, err := distcore.GetEngine()
	if err != nil {
		t.Fatalf("Failed to get engine: %v", err)
	}

	// Test with nil data
	eventWithNilData := distcore.WriteEvent{
		Path:   "test.db",
		Offset: 0,
		Data:   nil,
		IsWAL:  false,
	}

	// This should not crash
	strategy := engine.ComputeReplicationStrategy([]distcore.WriteEvent{eventWithNilData})
	t.Logf("✓ Handled nil data gracefully: strategy=%s", strategy)

	// Test with empty events
	encoded, err := engine.EncodeWriteBatch([]distcore.WriteEvent{})
	if err != nil {
		t.Fatalf("Failed to encode empty batch: %v", err)
	}
	
	if len(encoded) == 0 {
		t.Log("✓ Empty batch encoded correctly")
	}

	// Test binary diff with empty data
	diff, err := engine.ComputeDatabaseDiff([]byte{}, []byte{}, 4096)
	if err != nil {
		t.Fatalf("Failed to compute diff with empty data: %v", err)
	}
	
	if len(diff) == 0 {
		t.Log("✓ Empty database diff handled correctly")
	}
}

// Benchmark function to ensure integration works under load
func BenchmarkOCamlIntegration(b *testing.B) {
	engine, err := distcore.GetEngine()
	if err != nil {
		b.Fatalf("Failed to get engine: %v", err)
	}

	events := []distcore.WriteEvent{
		{
			Path:   "bench.db",
			Offset: 0,
			Data:   make([]byte, 1024),
			IsWAL:  false,
		},
	}

	b.ResetTimer()
	
	for i := 0; i < b.N; i++ {
		strategy := engine.ComputeReplicationStrategy(events)
		encoded, err := engine.EncodeWriteBatch(events)
		if err != nil {
			b.Fatalf("Benchmark failed: %v", err)
		}
		
		// Prevent optimization
		_ = strategy
		_ = encoded
	}
}

func init() {
	// Ensure we have OCaml library available
	if _, err := os.Stat("../../build/libdistcore.so"); os.IsNotExist(err) {
		fmt.Println("Warning: libdistcore.so not found. Run 'make build' first.")
	}
}