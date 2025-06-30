package replication

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"distsqlite/pkg/zmq"
)

// ZMQIntegratedReplicator combines the layered replication with ZMQ transport
type ZMQIntegratedReplicator struct {
	*HybridReplicator
	transport     *zmq.Transport
	isCoordinator bool
	leaseManager  LeaseManager
}

// LeaseManager interface for primary election
type LeaseManager interface {
	AcquireLease(ctx context.Context, dbPath string, ttl int64) (*zmq.LeaseResponse, error)
	RenewLease(ctx context.Context, dbPath string, ttl int64) (*zmq.LeaseResponse, error)
	ReleaseLease(ctx context.Context, dbPath string) error
	GetLeaseStatus(ctx context.Context, dbPath string) (*zmq.LeaseResponse, error)
}

// NewZMQIntegratedReplicator creates a replicator with ZMQ transport integration
func NewZMQIntegratedReplicator(config HybridReplicatorConfig, zmqConfig *zmq.TransportConfig, nodeID string) (*ZMQIntegratedReplicator, error) {
	// Create ZMQ transport
	transport, err := zmq.NewTransport(zmqConfig, nodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to create ZMQ transport: %w", err)
	}

	// Create hybrid replicator with ZMQ-aware targets
	zmqTargets := make([]string, 0, len(config.Targets))
	for _, target := range config.Targets {
		// Convert targets to ZMQ format if needed
		zmqTargets = append(zmqTargets, target)
	}
	
	config.Targets = zmqTargets
	hybridReplicator := NewHybridReplicatorWithConfig(config)

	return &ZMQIntegratedReplicator{
		HybridReplicator: hybridReplicator,
		transport:        transport,
		isCoordinator:    false,
		leaseManager:     &ZMQLeaseManager{transport: transport},
	}, nil
}

// StartAsCoordinator starts the replicator as a coordinator (can hold leases)
func (z *ZMQIntegratedReplicator) StartAsCoordinator() error {
	z.isCoordinator = true
	
	// Start ZMQ transport as coordinator
	if err := z.transport.StartCoordinator(); err != nil {
		return fmt.Errorf("failed to start ZMQ coordinator: %w", err)
	}
	
	// Start the layered replication system
	if err := z.HybridReplicator.Start(); err != nil {
		return fmt.Errorf("failed to start hybrid replicator: %w", err)
	}
	
	log.Printf("[ZMQ Integration] Started as coordinator")
	return nil
}

// StartAsNode starts the replicator as a regular node (replica)
func (z *ZMQIntegratedReplicator) StartAsNode() error {
	z.isCoordinator = false
	
	// Start ZMQ transport as node
	if err := z.transport.StartNode(); err != nil {
		return fmt.Errorf("failed to start ZMQ node: %w", err)
	}
	
	// Start the layered replication system
	if err := z.HybridReplicator.Start(); err != nil {
		return fmt.Errorf("failed to start hybrid replicator: %w", err)
	}
	
	log.Printf("[ZMQ Integration] Started as node")
	return nil
}

// AcquirePrimaryLease attempts to become primary for a database
func (z *ZMQIntegratedReplicator) AcquirePrimaryLease(ctx context.Context, dbPath string, ttl time.Duration) (*zmq.LeaseResponse, error) {
	if !z.isCoordinator {
		return nil, fmt.Errorf("only coordinators can acquire primary leases")
	}
	
	resp, err := z.leaseManager.AcquireLease(ctx, dbPath, int64(ttl.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("failed to acquire primary lease: %w", err)
	}
	
	if resp.Success {
		log.Printf("[ZMQ Integration] Acquired primary lease for %s (token: %d)", 
			dbPath, resp.FenceToken)
	}
	
	return resp, nil
}

// Override replication methods to use ZMQ transport

// replicateViaTxLog uses ZMQ PUB/SUB for SQL replay
func (z *ZMQIntegratedReplicator) replicateViaTxLog(dbPath string, changes []Change) error {
	z.writeTracker.mu.Lock()
	stats := z.writeTracker.databases[dbPath]
	if stats != nil {
		stats.SQLReplayAttempts++
	}
	z.writeTracker.mu.Unlock()
	
	// Extract SQL statements
	statements, err := z.sqlReplay.ExtractSQLFromChanges(changes)
	if err != nil {
		log.Printf("[ZMQ Integration] Failed to extract SQL: %v, falling back to page sync", err)
		return z.fallbackToZMQPageSync(dbPath, changes)
	}
	
	if len(statements) == 0 {
		return nil
	}
	
	// Create SQL replay batch
	batch := SQLReplayBatch{
		Database:   dbPath,
		Statements: statements,
		BatchID:    fmt.Sprintf("zmq_batch_%d_%s", time.Now().Unix(), filepath.Base(dbPath)),
		CreatedAt:  time.Now(),
		FenceToken: time.Now().Unix(),
	}
	
	// Publish via ZMQ PUB/SUB
	topic := fmt.Sprintf("sql_replay:%s", dbPath)
	if err := z.publishSQLBatch(topic, batch); err != nil {
		log.Printf("[ZMQ Integration] SQL replay publish failed: %v, falling back to page sync", err)
		return z.fallbackToZMQPageSync(dbPath, changes)
	}
	
	// Update stats
	z.writeTracker.mu.Lock()
	if stats != nil {
		// Success - no failure increment
	}
	z.writeTracker.mu.Unlock()
	
	log.Printf("[ZMQ Integration] SQL replay published: %d statements via ZMQ PUB", len(statements))
	return nil
}

// publishSQLBatch publishes an SQL batch via ZMQ PUB/SUB
func (z *ZMQIntegratedReplicator) publishSQLBatch(topic string, batch SQLReplayBatch) error {
	// Create ZMQ message
	msg := zmq.WALDiff{
		DBPath:     batch.Database,
		Sequence:   time.Now().Unix(),
		Operation:  "sql_replay",
		Data:       z.serializeSQLBatch(batch),
		Timestamp:  time.Now().Unix(),
	}
	
	// Publish to subscribers
	return z.transport.PublishWALDiff(topic, &msg)
}

// serializeSQLBatch converts SQL batch to bytes for ZMQ transport
func (z *ZMQIntegratedReplicator) serializeSQLBatch(batch SQLReplayBatch) []byte {
	// Simple JSON serialization - could use msgpack for efficiency
	data, _ := json.Marshal(batch)
	return data
}

// fallbackToZMQPageSync falls back to page sync using ZMQ coordination
func (z *ZMQIntegratedReplicator) fallbackToZMQPageSync(dbPath string, changes []Change) error {
	z.writeTracker.mu.Lock()
	stats := z.writeTracker.databases[dbPath]
	if stats != nil {
		stats.PageSyncFallbacks++
	}
	z.writeTracker.mu.Unlock()
	
	// Use ZMQ REQ/REP for sync coordination
	syncRequest := zmq.SyncRequest{
		ID:         fmt.Sprintf("sync_%d_%s", time.Now().Unix(), filepath.Base(dbPath)),
		SourceNode: z.transport.GetNodeID(),
		DBPath:     dbPath,
		Strategy:   "page_sync",
		Priority:   1,
		Timestamp:  time.Now().Unix(),
	}
	
	// Send sync request to all targets
	for _, target := range z.targets {
		if err := z.requestPageSync(target, syncRequest); err != nil {
			log.Printf("[ZMQ Integration] Page sync request failed to %s: %v", target, err)
		} else {
			log.Printf("[ZMQ Integration] Page sync requested to %s via ZMQ REQ/REP", target)
		}
	}
	
	return nil
}

// requestPageSync sends a page sync request via ZMQ REQ/REP
func (z *ZMQIntegratedReplicator) requestPageSync(target string, request zmq.SyncRequest) error {
	// Send via ZMQ REQ/REP for reliability
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	return z.transport.RequestSync(ctx, target, &request)
}

// Start ZMQ subscribers for receiving replication

// StartSQLReplaySubscriber starts listening for SQL replay messages
func (z *ZMQIntegratedReplicator) StartSQLReplaySubscriber(dbPath string) error {
	topic := fmt.Sprintf("sql_replay:%s", dbPath)
	
	return z.transport.SubscribeWALDiff(topic, func(diff *zmq.WALDiff) error {
		if diff.Operation != "sql_replay" {
			return nil // Not a SQL replay message
		}
		
		// Deserialize SQL batch
		var batch SQLReplayBatch
		if err := json.Unmarshal(diff.Data, &batch); err != nil {
			return fmt.Errorf("failed to deserialize SQL batch: %w", err)
		}
		
		// Apply SQL batch locally
		return z.applySQLBatchLocally(batch)
	})
}

// applySQLBatchLocally applies a received SQL batch to the local database
func (z *ZMQIntegratedReplicator) applySQLBatchLocally(batch SQLReplayBatch) error {
	log.Printf("[ZMQ Integration] Applying SQL batch: %d statements from %s", 
		len(batch.Statements), batch.BatchID)
	
	// Open local database connection
	db, err := sql.Open("sqlite", batch.Database)
	if err != nil {
		return fmt.Errorf("failed to open local database: %w", err)
	}
	defer db.Close()
	
	// Apply in transaction
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	
	// Execute all statements
	for i, stmt := range batch.Statements {
		if err := z.executeStatement(tx, stmt); err != nil {
			return fmt.Errorf("failed to execute statement %d: %w", i, err)
		}
	}
	
	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	
	log.Printf("[ZMQ Integration] Successfully applied %d SQL statements", len(batch.Statements))
	return nil
}

// executeStatement executes a single SQL statement in a transaction
func (z *ZMQIntegratedReplicator) executeStatement(tx *sql.Tx, stmt SQLStatement) error {
	if len(stmt.Params) > 0 {
		_, err := tx.Exec(stmt.SQL, stmt.Params...)
		return err
	} else {
		_, err := tx.Exec(stmt.SQL)
		return err
	}
}

// Stop gracefully shuts down the integrated replicator
func (z *ZMQIntegratedReplicator) Stop() error {
	// Stop hybrid replicator first
	if err := z.HybridReplicator.Stop(); err != nil {
		log.Printf("[ZMQ Integration] Error stopping hybrid replicator: %v", err)
	}
	
	// Stop ZMQ transport
	if err := z.transport.Close(); err != nil {
		log.Printf("[ZMQ Integration] Error stopping ZMQ transport: %v", err)
	}
	
	log.Printf("[ZMQ Integration] Stopped successfully")
	return nil
}

// ZMQLeaseManager implements LeaseManager using ZMQ transport
type ZMQLeaseManager struct {
	transport *zmq.Transport
}

func (z *ZMQLeaseManager) AcquireLease(ctx context.Context, dbPath string, ttl int64) (*zmq.LeaseResponse, error) {
	return z.transport.AcquireLease(ctx, dbPath, ttl)
}

func (z *ZMQLeaseManager) RenewLease(ctx context.Context, dbPath string, ttl int64) (*zmq.LeaseResponse, error) {
	return z.transport.RenewLease(ctx, dbPath, ttl)
}

func (z *ZMQLeaseManager) ReleaseLease(ctx context.Context, dbPath string) error {
	return z.transport.ReleaseLease(ctx, dbPath)
}

func (z *ZMQLeaseManager) GetLeaseStatus(ctx context.Context, dbPath string) (*zmq.LeaseResponse, error) {
	return z.transport.GetLeaseStatus(ctx, dbPath)
}

// Helper methods for transport integration

// GetTransport returns the underlying ZMQ transport for advanced usage
func (z *ZMQIntegratedReplicator) GetTransport() *zmq.Transport {
	return z.transport
}

// IsCoordinator returns whether this instance is running as a coordinator
func (z *ZMQIntegratedReplicator) IsCoordinator() bool {
	return z.isCoordinator
}

// GetNodeID returns the ZMQ node ID
func (z *ZMQIntegratedReplicator) GetNodeID() string {
	return z.transport.GetNodeID()
}