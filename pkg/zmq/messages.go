package zmq

import "time"

// Message types for ZMQ communication

// LeaseRequest represents a lease operation request
type LeaseRequest struct {
	NodeID    string `msgpack:"node_id"`
	DBPath    string `msgpack:"db_path"`
	TTL       int64  `msgpack:"ttl"`
	Action    string `msgpack:"action"` // "acquire", "renew", "release"
	Timestamp int64  `msgpack:"timestamp"`
}

// LeaseResponse represents a lease operation response
type LeaseResponse struct {
	Success     bool   `msgpack:"success"`
	FenceToken  int64  `msgpack:"fence_token"`
	ExpiresAt   int64  `msgpack:"expires_at"`
	LeaseHolder string `msgpack:"lease_holder"`
	Error       string `msgpack:"error,omitempty"`
	Timestamp   int64  `msgpack:"timestamp"`
}

// WALDiff represents a WAL page change for replication
type WALDiff struct {
	DBPath     string `msgpack:"db_path"`
	Sequence   int64  `msgpack:"sequence"`
	PageNumber int32  `msgpack:"page_number"`
	Data       []byte `msgpack:"data"`
	Checksum   uint32 `msgpack:"checksum"`
	Timestamp  int64  `msgpack:"timestamp"`
	Operation  string `msgpack:"operation"` // "page_write", "commit", "checkpoint"
}

// SyncRequest represents a bulk synchronization request
type SyncRequest struct {
	ID         string `msgpack:"id"`
	SourceNode string `msgpack:"source_node"`
	TargetNode string `msgpack:"target_node"`
	DBPath     string `msgpack:"db_path"`
	Strategy   string `msgpack:"strategy"` // "rsync", "full_copy"
	Priority   int    `msgpack:"priority"`
	Timestamp  int64  `msgpack:"timestamp"`
}

// SyncResponse represents a bulk synchronization response
type SyncResponse struct {
	ID        string `msgpack:"id"`
	Success   bool   `msgpack:"success"`
	BytesSent int64  `msgpack:"bytes_sent"`
	Duration  int64  `msgpack:"duration_ms"`
	Error     string `msgpack:"error,omitempty"`
	Timestamp int64  `msgpack:"timestamp"`
}

// NodeRegistration represents a node joining the cluster
type NodeRegistration struct {
	NodeID      string            `msgpack:"node_id"`
	Hostname    string            `msgpack:"hostname"`
	Endpoints   map[string]string `msgpack:"endpoints"` // service -> endpoint
	Capabilities []string          `msgpack:"capabilities"`
	Metadata    map[string]string `msgpack:"metadata"`
	Timestamp   int64             `msgpack:"timestamp"`
}

// NodeHeartbeat represents a node health check
type NodeHeartbeat struct {
	NodeID    string            `msgpack:"node_id"`
	Status    string            `msgpack:"status"` // "healthy", "degraded", "unhealthy"
	Metrics   map[string]int64  `msgpack:"metrics"`
	Databases []string          `msgpack:"databases"`
	Timestamp int64             `msgpack:"timestamp"`
}

// ClusterEvent represents a cluster-wide state change
type ClusterEvent struct {
	Type      string                 `msgpack:"type"` // "lease_acquired", "node_joined", "node_failed"
	NodeID    string                 `msgpack:"node_id"`
	DBPath    string                 `msgpack:"db_path,omitempty"`
	Data      map[string]interface{} `msgpack:"data,omitempty"`
	Timestamp int64                  `msgpack:"timestamp"`
}

// Helper functions for message creation

// NewLeaseRequest creates a new lease request
func NewLeaseRequest(nodeID, dbPath, action string, ttl int64) *LeaseRequest {
	return &LeaseRequest{
		NodeID:    nodeID,
		DBPath:    dbPath,
		TTL:       ttl,
		Action:    action,
		Timestamp: time.Now().UnixNano(),
	}
}

// NewLeaseResponse creates a new lease response
func NewLeaseResponse(success bool, fenceToken, expiresAt int64, leaseHolder, errorMsg string) *LeaseResponse {
	return &LeaseResponse{
		Success:     success,
		FenceToken:  fenceToken,
		ExpiresAt:   expiresAt,
		LeaseHolder: leaseHolder,
		Error:       errorMsg,
		Timestamp:   time.Now().UnixNano(),
	}
}

// NewWALDiff creates a new WAL diff message
func NewWALDiff(dbPath string, sequence int64, pageNumber int32, data []byte, operation string) *WALDiff {
	return &WALDiff{
		DBPath:     dbPath,
		Sequence:   sequence,
		PageNumber: pageNumber,
		Data:       data,
		Operation:  operation,
		Timestamp:  time.Now().UnixNano(),
		Checksum:   crc32Checksum(data),
	}
}

// NewNodeRegistration creates a new node registration message
func NewNodeRegistration(nodeID, hostname string) *NodeRegistration {
	return &NodeRegistration{
		NodeID:       nodeID,
		Hostname:     hostname,
		Endpoints:    make(map[string]string),
		Capabilities: []string{},
		Metadata:     make(map[string]string),
		Timestamp:    time.Now().UnixNano(),
	}
}

// NewNodeHeartbeat creates a new node heartbeat message
func NewNodeHeartbeat(nodeID, status string, databases []string) *NodeHeartbeat {
	return &NodeHeartbeat{
		NodeID:    nodeID,
		Status:    status,
		Metrics:   make(map[string]int64),
		Databases: databases,
		Timestamp: time.Now().UnixNano(),
	}
}

// NewClusterEvent creates a new cluster event message
func NewClusterEvent(eventType, nodeID, dbPath string) *ClusterEvent {
	return &ClusterEvent{
		Type:      eventType,
		NodeID:    nodeID,
		DBPath:    dbPath,
		Data:      make(map[string]interface{}),
		Timestamp: time.Now().UnixNano(),
	}
}

// crc32Checksum calculates a simple CRC32 checksum
func crc32Checksum(data []byte) uint32 {
	// Simple checksum for now - in production use hash/crc32
	var checksum uint32
	for _, b := range data {
		checksum = checksum*31 + uint32(b)
	}
	return checksum
}