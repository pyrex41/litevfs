package zmq

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ReplicationManager handles WAL diff streaming via ZMQ PUB/SUB
type ReplicationManager struct {
	transport *Transport
	ctx       context.Context
	cancel    context.CancelFunc
	
	// Subscription management
	mu            sync.RWMutex
	subscriptions map[string]*Subscription // dbPath -> subscription
}

// Subscription represents a database replication subscription
type Subscription struct {
	DBPath    string
	StartSeq  int64
	Handler   WALDiffHandler
	Active    bool
	LastSeen  time.Time
}

// WALDiffHandler processes incoming WAL diffs
type WALDiffHandler func(diff *WALDiff) error

// NewReplicationManager creates a new replication manager
func NewReplicationManager(transport *Transport) *ReplicationManager {
	ctx, cancel := context.WithCancel(context.Background())
	
	return &ReplicationManager{
		transport:     transport,
		ctx:           ctx,
		cancel:        cancel,
		subscriptions: make(map[string]*Subscription),
	}
}

// Start begins replication operations
func (r *ReplicationManager) Start() error {
	// Start diff receiver for subscribers
	go r.receiveDiffs()
	
	log.Printf("[ZMQ Replication] Replication manager started")
	return nil
}

// Stop shuts down replication
func (r *ReplicationManager) Stop() error {
	r.cancel()
	return nil
}

// PublishWALDiff publishes a WAL diff to all subscribers (coordinator side)
func (r *ReplicationManager) PublishWALDiff(diff *WALDiff) error {
	if r.transport.diffPub == nil {
		return fmt.Errorf("PUB socket not available - not in coordinator mode")
	}
	
	// Create topic from database path
	topic := r.createTopic(diff.DBPath)
	
	// Send topic first (ZMQ PUB/SUB requires topic prefix)
	if _, err := r.transport.diffPub.Send(topic, 1); err != nil {
		return fmt.Errorf("failed to send topic: %w", err)
	}
	
	// Send the actual diff message
	if err := r.transport.sendMessage(r.transport.diffPub, diff); err != nil {
		return fmt.Errorf("failed to publish WAL diff: %w", err)
	}
	
	log.Printf("[ZMQ Replication] Published WAL diff for %s (seq: %d, page: %d)", 
		diff.DBPath, diff.Sequence, diff.PageNumber)
	
	return nil
}

// SubscribeDatabase subscribes to WAL diffs for a specific database (node side)
func (r *ReplicationManager) SubscribeDatabase(dbPath string, startSeq int64, handler WALDiffHandler) error {
	if r.transport.diffSub == nil {
		return fmt.Errorf("SUB socket not available - not in node mode")
	}
	
	r.mu.Lock()
	defer r.mu.Unlock()
	
	// Check if already subscribed
	if _, exists := r.subscriptions[dbPath]; exists {
		return fmt.Errorf("already subscribed to database: %s", dbPath)
	}
	
	// Create subscription
	subscription := &Subscription{
		DBPath:   dbPath,
		StartSeq: startSeq,
		Handler:  handler,
		Active:   true,
		LastSeen: time.Now(),
	}
	
	r.subscriptions[dbPath] = subscription
	
	// Subscribe to topic for this database
	topic := r.createTopic(dbPath)
	if err := r.transport.diffSub.SetSubscribe(topic); err != nil {
		delete(r.subscriptions, dbPath)
		return fmt.Errorf("failed to subscribe to topic %s: %w", topic, err)
	}
	
	log.Printf("[ZMQ Replication] Subscribed to database: %s (from seq: %d)", dbPath, startSeq)
	return nil
}

// UnsubscribeDatabase stops receiving diffs for a database
func (r *ReplicationManager) UnsubscribeDatabase(dbPath string) error {
	if r.transport.diffSub == nil {
		return fmt.Errorf("SUB socket not available")
	}
	
	r.mu.Lock()
	defer r.mu.Unlock()
	
	subscription, exists := r.subscriptions[dbPath]
	if !exists {
		return fmt.Errorf("not subscribed to database: %s", dbPath)
	}
	
	// Unsubscribe from topic
	topic := r.createTopic(dbPath)
	if err := r.transport.diffSub.SetUnsubscribe(topic); err != nil {
		return fmt.Errorf("failed to unsubscribe from topic %s: %w", topic, err)
	}
	
	subscription.Active = false
	delete(r.subscriptions, dbPath)
	
	log.Printf("[ZMQ Replication] Unsubscribed from database: %s", dbPath)
	return nil
}

// receiveDiffs handles incoming WAL diffs
func (r *ReplicationManager) receiveDiffs() {
	if r.transport.diffSub == nil {
		log.Printf("[ZMQ Replication] No SUB socket available for receiving diffs")
		return
	}
	
	log.Printf("[ZMQ Replication] Starting to receive WAL diffs")
	
	for {
		select {
		case <-r.ctx.Done():
			return
		default:
			// Receive topic first
			topic, err := r.transport.diffSub.Recv(0)
			if err != nil {
				if err.Error() == "resource temporarily unavailable" {
					continue // Timeout, keep listening
				}
				log.Printf("[ZMQ Replication] Error receiving topic: %v", err)
				continue
			}
			
			// Receive WAL diff message
			var diff WALDiff
			if err := r.transport.receiveMessage(r.transport.diffSub, &diff); err != nil {
				log.Printf("[ZMQ Replication] Error receiving WAL diff: %v", err)
				continue
			}
			
			// Process the diff
			r.processDiff(topic, &diff)
		}
	}
}

// processDiff handles a received WAL diff
func (r *ReplicationManager) processDiff(topic string, diff *WALDiff) {
	r.mu.RLock()
	subscription, exists := r.subscriptions[diff.DBPath]
	r.mu.RUnlock()
	
	if !exists || !subscription.Active {
		log.Printf("[ZMQ Replication] Received diff for unsubscribed database: %s", diff.DBPath)
		return
	}
	
	// Check sequence number
	if diff.Sequence < subscription.StartSeq {
		log.Printf("[ZMQ Replication] Ignoring old diff seq %d < %d for %s", 
			diff.Sequence, subscription.StartSeq, diff.DBPath)
		return
	}
	
	// Validate checksum
	if !r.validateDiff(diff) {
		log.Printf("[ZMQ Replication] Invalid checksum for diff seq %d, db %s", 
			diff.Sequence, diff.DBPath)
		return
	}
	
	// Update last seen
	r.mu.Lock()
	subscription.LastSeen = time.Now()
	r.mu.Unlock()
	
	// Call handler
	if err := subscription.Handler(diff); err != nil {
		log.Printf("[ZMQ Replication] Handler error for %s seq %d: %v", 
			diff.DBPath, diff.Sequence, err)
	} else {
		log.Printf("[ZMQ Replication] Applied WAL diff for %s (seq: %d, page: %d, op: %s)", 
			diff.DBPath, diff.Sequence, diff.PageNumber, diff.Operation)
	}
}

// validateDiff validates the integrity of a WAL diff
func (r *ReplicationManager) validateDiff(diff *WALDiff) bool {
	// Recalculate checksum
	expectedChecksum := crc32Checksum(diff.Data)
	return diff.Checksum == expectedChecksum
}

// createTopic creates a ZMQ topic from database path
func (r *ReplicationManager) createTopic(dbPath string) string {
	// Use database name as topic to avoid path separator issues
	topic := strings.ReplaceAll(dbPath, "/", "_")
	topic = strings.ReplaceAll(topic, "\\", "_")
	return fmt.Sprintf("db_%s", topic)
}

// GetSubscriptionStats returns statistics about active subscriptions
func (r *ReplicationManager) GetSubscriptionStats() map[string]SubscriptionStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	stats := make(map[string]SubscriptionStats)
	
	for dbPath, sub := range r.subscriptions {
		stats[dbPath] = SubscriptionStats{
			DBPath:    sub.DBPath,
			StartSeq:  sub.StartSeq,
			Active:    sub.Active,
			LastSeen:  sub.LastSeen,
			Duration:  time.Since(sub.LastSeen),
		}
	}
	
	return stats
}

// SubscriptionStats provides statistics about a subscription
type SubscriptionStats struct {
	DBPath    string        `json:"db_path"`
	StartSeq  int64         `json:"start_seq"`
	Active    bool          `json:"active"`
	LastSeen  time.Time     `json:"last_seen"`
	Duration  time.Duration `json:"duration"`
}

// Batch WAL operations for high-throughput scenarios

// WALBatch represents a batch of WAL diffs
type WALBatch struct {
	DBPath string     `msgpack:"db_path"`
	Diffs  []*WALDiff `msgpack:"diffs"`
	BatchID string    `msgpack:"batch_id"`
	Timestamp int64   `msgpack:"timestamp"`
}

// PublishWALBatch publishes a batch of WAL diffs (for high-throughput scenarios)
func (r *ReplicationManager) PublishWALBatch(batch *WALBatch) error {
	if r.transport.diffPub == nil {
		return fmt.Errorf("PUB socket not available - not in coordinator mode")
	}
	
	// Create topic from database path
	topic := r.createTopic(batch.DBPath)
	
	// Send topic first
	if _, err := r.transport.diffPub.Send(topic+"_batch", 1); err != nil {
		return fmt.Errorf("failed to send batch topic: %w", err)
	}
	
	// Send the batch
	if err := r.transport.sendMessage(r.transport.diffPub, batch); err != nil {
		return fmt.Errorf("failed to publish WAL batch: %w", err)
	}
	
	log.Printf("[ZMQ Replication] Published WAL batch for %s (%d diffs, batch: %s)", 
		batch.DBPath, len(batch.Diffs), batch.BatchID)
	
	return nil
}

// Demo/testing functions

// StartTestPublisher starts a test publisher for demonstration
func (r *ReplicationManager) StartTestPublisher(dbPath string, interval time.Duration) {
	if r.transport.diffPub == nil {
		log.Printf("[ZMQ Replication] No PUB socket for test publisher")
		return
	}
	
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		
		var sequence int64 = 1
		
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-ticker.C:
				// Create test WAL diff
				testData := []byte(fmt.Sprintf("test_data_%d", sequence))
				diff := NewWALDiff(dbPath, sequence, int32(sequence%100), testData, "page_write")
				
				if err := r.PublishWALDiff(diff); err != nil {
					log.Printf("[ZMQ Replication] Test publisher error: %v", err)
				}
				
				sequence++
			}
		}
	}()
	
	log.Printf("[ZMQ Replication] Started test publisher for %s (interval: %v)", dbPath, interval)
}

// TestDiffHandler is a simple handler for testing
func TestDiffHandler(diff *WALDiff) error {
	log.Printf("[ZMQ Replication] Test handler received diff: db=%s, seq=%d, page=%d, size=%d", 
		diff.DBPath, diff.Sequence, diff.PageNumber, len(diff.Data))
	return nil
}