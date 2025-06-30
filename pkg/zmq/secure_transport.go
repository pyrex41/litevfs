package zmq

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pebbe/zmq4"
	"github.com/vmihailenco/msgpack/v5"
)

// SecureTransport implements secure ZMQ transport with CURVE authentication
type SecureTransport struct {
	*Transport // Embed base transport
	
	// Secure WAL replication sockets
	walPub *zmq4.Socket // PUB socket for WAL page broadcasting
	walSub *zmq4.Socket // SUB socket for WAL page receiving
	
	// Security state
	authenticated bool
	curveEnabled  bool
	
	// WAL subscription callback
	walCallback func(*WALPage)
	walMutex    sync.RWMutex
}

// WALPage represents a WAL page for replication (matches the interface)
type WALPage struct {
	DatabasePath string    `msgpack:"db_path"`
	PageNumber   uint32    `msgpack:"page_num"`
	WALSequence  uint64    `msgpack:"wal_seq"`
	Timestamp    time.Time `msgpack:"timestamp"`
	FenceToken   int64     `msgpack:"fence_token"`
	NodeID       string    `msgpack:"node_id"`
	Checksum     [32]byte  `msgpack:"checksum"`
	WALData      []byte    `msgpack:"wal_data"`
	PageSize     int       `msgpack:"page_size"`
	IsCommit     bool      `msgpack:"is_commit"`
	TxnID        uint64    `msgpack:"txn_id"`
}

// NewSecureTransport creates a new secure transport with CURVE enforcement
func NewSecureTransport(config *TransportConfig, nodeID string) (*SecureTransport, error) {
	// Ensure CURVE is enabled for security
	if !config.Security.Enabled || !config.Security.CurveEnabled {
		return nil, fmt.Errorf("CURVE authentication is required for secure transport")
	}
	
	// Create base transport
	baseTransport, err := NewTransport(config, nodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to create base transport: %w", err)
	}
	
	return &SecureTransport{
		Transport:     baseTransport,
		authenticated: false,
		curveEnabled:  config.Security.CurveEnabled,
	}, nil
}

// StartSecureCoordinator starts the transport in secure coordinator mode
func (st *SecureTransport) StartSecureCoordinator() error {
	// Start base coordinator
	if err := st.Transport.StartCoordinator(); err != nil {
		return fmt.Errorf("failed to start base coordinator: %w", err)
	}
	
	// Create secure WAL PUB socket
	var err error
	st.walPub, err = st.zmqCtx.NewSocket(zmq4.PUB)
	if err != nil {
		return fmt.Errorf("failed to create WAL PUB socket: %w", err)
	}
	
	// Configure CURVE authentication for WAL PUB
	if err := st.configureCurveServer(st.walPub); err != nil {
		return fmt.Errorf("failed to configure CURVE for WAL PUB: %w", err)
	}
	
	// Bind WAL PUB socket
	walPubAddr := fmt.Sprintf("tcp://%s", incrementPort(st.config.BindAddr, 10))
	if err := st.walPub.Bind(walPubAddr); err != nil {
		return fmt.Errorf("failed to bind WAL PUB to %s: %w", walPubAddr, err)
	}
	
	st.authenticated = true
	
	log.Printf("[Secure Transport] Coordinator started with CURVE authentication:")
	log.Printf("  WAL PUB: %s (CURVE enabled)", walPubAddr)
	
	return nil
}

// StartSecureNode starts the transport in secure node mode
func (st *SecureTransport) StartSecureNode() error {
	// Start base node
	if err := st.Transport.StartNode(); err != nil {
		return fmt.Errorf("failed to start base node: %w", err)
	}
	
	// Create secure WAL SUB socket
	var err error
	st.walSub, err = st.zmqCtx.NewSocket(zmq4.SUB)
	if err != nil {
		return fmt.Errorf("failed to create WAL SUB socket: %w", err)
	}
	
	// Configure CURVE authentication for WAL SUB
	if err := st.configureCurveClient(st.walSub); err != nil {
		return fmt.Errorf("failed to configure CURVE for WAL SUB: %w", err)
	}
	
	// Connect to coordinator WAL PUB
	coordinatorEndpoints := st.getCoordinatorEndpoints()
	if len(coordinatorEndpoints) == 0 {
		return fmt.Errorf("no coordinator endpoints available")
	}
	
	walSubAddr := incrementPort(coordinatorEndpoints[0], 10)
	if err := st.walSub.Connect(walSubAddr); err != nil {
		return fmt.Errorf("failed to connect WAL SUB to %s: %w", walSubAddr, err)
	}
	
	// Subscribe to all WAL pages initially
	if err := st.walSub.SetSubscribe(""); err != nil {
		return fmt.Errorf("failed to set WAL subscription: %w", err)
	}
	
	st.authenticated = true
	
	log.Printf("[Secure Transport] Node started with CURVE authentication:")
	log.Printf("  WAL SUB: %s (CURVE enabled)", walSubAddr)
	
	// Start WAL receiver
	go st.walReceiver()
	
	return nil
}

// PublishWALPage publishes a WAL page via secure ZMQ PUB socket
func (st *SecureTransport) PublishWALPage(ctx context.Context, page *WALPage) error {
	if !st.authenticated {
		return fmt.Errorf("transport not authenticated")
	}
	
	if st.walPub == nil {
		return fmt.Errorf("WAL PUB socket not initialized")
	}
	
	// Serialize page
	data, err := msgpack.Marshal(page)
	if err != nil {
		return fmt.Errorf("failed to serialize WAL page: %w", err)
	}
	
	// Create topic for database-specific filtering
	topic := fmt.Sprintf("wal:%s", page.DatabasePath)
	
	// Send topic and data
	if _, err := st.walPub.SendBytes([]byte(topic), zmq4.SNDMORE); err != nil {
		return fmt.Errorf("failed to send WAL topic: %w", err)
	}
	
	if _, err := st.walPub.SendBytes(data, 0); err != nil {
		return fmt.Errorf("failed to send WAL page: %w", err)
	}
	
	log.Printf("[Secure Transport] Published WAL page %d for %s (seq: %d)", 
		page.PageNumber, page.DatabasePath, page.WALSequence)
	
	return nil
}

// SubscribeWALPages subscribes to WAL pages and calls callback for each page
func (st *SecureTransport) SubscribeWALPages(ctx context.Context, callback func(*WALPage)) error {
	if !st.authenticated {
		return fmt.Errorf("transport not authenticated")
	}
	
	st.walMutex.Lock()
	st.walCallback = callback
	st.walMutex.Unlock()
	
	log.Printf("[Secure Transport] WAL subscription active")
	return nil
}

// IsAuthenticated returns true if transport is authenticated
func (st *SecureTransport) IsAuthenticated() bool {
	return st.authenticated && st.curveEnabled
}

// GetNodeID returns the node ID
func (st *SecureTransport) GetNodeID() string {
	return st.nodeID
}

// walReceiver handles incoming WAL pages
func (st *SecureTransport) walReceiver() {
	log.Printf("[Secure Transport] WAL receiver started")
	
	for {
		// Receive topic
		topic, err := st.walSub.RecvBytes(0)
		if err != nil {
			log.Printf("[Secure Transport] Failed to receive WAL topic: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		
		// Receive data
		data, err := st.walSub.RecvBytes(0)
		if err != nil {
			log.Printf("[Secure Transport] Failed to receive WAL data: %v", err)
			continue
		}
		
		// Deserialize page
		var page WALPage
		if err := msgpack.Unmarshal(data, &page); err != nil {
			log.Printf("[Secure Transport] Failed to deserialize WAL page: %v", err)
			continue
		}
		
		// Call callback if set
		st.walMutex.RLock()
		callback := st.walCallback
		st.walMutex.RUnlock()
		
		if callback != nil {
			callback(&page)
		}
		
		log.Printf("[Secure Transport] Received WAL page %d for %s (topic: %s)", 
			page.PageNumber, page.DatabasePath, string(topic))
	}
}

// SubscribeToDatabase subscribes to WAL pages for a specific database
func (st *SecureTransport) SubscribeToDatabase(dbPath string) error {
	if st.walSub == nil {
		return fmt.Errorf("WAL SUB socket not initialized")
	}
	
	topic := fmt.Sprintf("wal:%s", dbPath)
	if err := st.walSub.SetSubscribe(topic); err != nil {
		return fmt.Errorf("failed to subscribe to database %s: %w", dbPath, err)
	}
	
	log.Printf("[Secure Transport] Subscribed to WAL pages for database: %s", dbPath)
	return nil
}

// UnsubscribeFromDatabase unsubscribes from WAL pages for a specific database
func (st *SecureTransport) UnsubscribeFromDatabase(dbPath string) error {
	if st.walSub == nil {
		return fmt.Errorf("WAL SUB socket not initialized")
	}
	
	topic := fmt.Sprintf("wal:%s", dbPath)
	if err := st.walSub.SetUnsubscribe(topic); err != nil {
		return fmt.Errorf("failed to unsubscribe from database %s: %w", dbPath, err)
	}
	
	log.Printf("[Secure Transport] Unsubscribed from WAL pages for database: %s", dbPath)
	return nil
}

// Close closes the secure transport
func (st *SecureTransport) Close() error {
	// Close WAL sockets
	if st.walPub != nil {
		st.walPub.Close()
	}
	if st.walSub != nil {
		st.walSub.Close()
	}
	
	// Close base transport
	return st.Transport.Close()
}

// GetSecurityInfo returns security information
func (st *SecureTransport) GetSecurityInfo() map[string]interface{} {
	return map[string]interface{}{
		"authenticated": st.authenticated,
		"curve_enabled": st.curveEnabled,
		"node_id":       st.nodeID,
		"security_protocol": "ZMQ_CURVE",
		"transport_encryption": "ChaCha20-Poly1305",
		"key_exchange": "Curve25519",
	}
}

// ValidateSecurityConfiguration validates that security is properly configured
func ValidateSecurityConfiguration(config *TransportConfig) error {
	if !config.Security.Enabled {
		return fmt.Errorf("security must be enabled for production use")
	}
	
	if !config.Security.CurveEnabled {
		return fmt.Errorf("CURVE authentication must be enabled")
	}
	
	// Check for production vs test keys
	if config.Security.ServerPublicKey == TestServerPublicKey {
		log.Printf("[Security Warning] Using test keys - generate production keys for deployment")
	}
	
	if config.Security.ServerPublicKey == "" || config.Security.ServerSecretKey == "" {
		return fmt.Errorf("server CURVE keys must be provided")
	}
	
	if len(config.Security.ServerPublicKey) != 40 || len(config.Security.ServerSecretKey) != 40 {
		return fmt.Errorf("CURVE keys must be 40 characters (Z85 encoded)")
	}
	
	return nil
}

// GenerateSecureConfig generates a secure transport configuration
func GenerateSecureConfig(nodeID string, coordinatorEndpoints []string) *TransportConfig {
	return &TransportConfig{
		IPCBasePath:       "/tmp/distsqlite-secure",
		BindAddr:          "0.0.0.0:5555",
		Peers:             coordinatorEndpoints,
		UsePrivateNetwork: true,
		Security: SecurityConfig{
			Enabled:           true,
			CurveEnabled:      true,
			ServerPublicKey:   "", // Must be set by operator
			ServerSecretKey:   "", // Must be set by operator
			ClientPublicKey:   "", // Must be set by operator
			ClientSecretKey:   "", // Must be set by operator
			AllowedClients:    []string{}, // Restrict to specific clients
		},
		RequestTimeout: 5 * time.Second,
		RetryAttempts:  3,
	}
}