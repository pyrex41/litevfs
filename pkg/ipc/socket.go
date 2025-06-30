package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// WriteEvent represents a database write event from the VFS layer
type WriteEvent struct {
	Path        string    `json:"path"`
	Offset      int64     `json:"offset"`
	Length      int32     `json:"length"`
	IsWAL       bool      `json:"is_wal"`
	TimestampNs int64     `json:"timestamp_ns"`
	Data        []byte    `json:"data"`
	FenceToken  int64     `json:"fence_token"`
}

// BatchCompleteEvent represents completion of a write batch
type BatchCompleteEvent struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// SyncEvent represents a file sync operation
type SyncEvent struct {
	Path  string `json:"path"`
	Flags int    `json:"flags"`
}

// TruncateEvent represents a file truncation
type TruncateEvent struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// StatusRequest requests current status
type StatusRequest struct{}

// StatusResponse contains current status
type StatusResponse struct {
	OpenFiles     int   `json:"open_files"`
	PendingWrites int   `json:"pending_writes"`
	TotalWrites   int64 `json:"total_writes"`
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// Message types for IPC communication
type Message struct {
	Type      string      `json:"type"`
	Payload   interface{} `json:"payload"`
	Timestamp int64       `json:"timestamp"`
	ID        string      `json:"id,omitempty"`
}

// Message type constants
const (
	MsgTypeWriteEvent     = "write_event"
	MsgTypeBatchComplete  = "batch_complete"
	MsgTypeSync           = "sync"
	MsgTypeTruncate       = "truncate"
	MsgTypeStatusRequest  = "status_request"
	MsgTypeStatusResponse = "status_response"
	MsgTypeShutdown       = "shutdown"
)

// SocketConfig configuration for socket-based IPC
type SocketConfig struct {
	SocketPath      string
	MaxConnections  int
	BufferSize      int
	TimeoutSeconds  int
	RetryAttempts   int
	RetryDelayMs    int
}

// DefaultSocketConfig returns sensible defaults
func DefaultSocketConfig() SocketConfig {
	return SocketConfig{
		SocketPath:      "/tmp/distsqlite_ipc.sock",
		MaxConnections:  10,
		BufferSize:      64 * 1024, // 64KB
		TimeoutSeconds:  5,
		RetryAttempts:   3,
		RetryDelayMs:    100,
	}
}

// SocketServer handles multiple client connections
type SocketServer struct {
	config    SocketConfig
	listener  net.Listener
	clients   map[string]net.Conn
	clientsMu sync.RWMutex
	
	// Callbacks
	writeCallback    func(WriteEvent) error
	batchCallback    func(BatchCompleteEvent) error
	syncCallback     func(SyncEvent) error
	truncateCallback func(TruncateEvent) error
	
	// Control
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	running    int32
	startTime  time.Time
	
	// Statistics
	messagesReceived int64
	messagesSent     int64
	clientsConnected int64
	clientsTotal     int64
}

// NewSocketServer creates a new socket server
func NewSocketServer(config SocketConfig) *SocketServer {
	ctx, cancel := context.WithCancel(context.Background())
	
	return &SocketServer{
		config:    config,
		clients:   make(map[string]net.Conn),
		ctx:       ctx,
		cancel:    cancel,
		startTime: time.Now(),
	}
}

// SetWriteCallback sets the callback for write events
func (s *SocketServer) SetWriteCallback(cb func(WriteEvent) error) {
	s.writeCallback = cb
}

// SetBatchCallback sets the callback for batch completion events
func (s *SocketServer) SetBatchCallback(cb func(BatchCompleteEvent) error) {
	s.batchCallback = cb
}

// SetSyncCallback sets the callback for sync events
func (s *SocketServer) SetSyncCallback(cb func(SyncEvent) error) {
	s.syncCallback = cb
}

// SetTruncateCallback sets the callback for truncate events
func (s *SocketServer) SetTruncateCallback(cb func(TruncateEvent) error) {
	s.truncateCallback = cb
}

// Start starts the socket server
func (s *SocketServer) Start() error {
	// Remove existing socket file
	if err := os.Remove(s.config.SocketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove socket file: %w", err)
	}
	
	// Create Unix domain socket
	listener, err := net.Listen("unix", s.config.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to create socket: %w", err)
	}
	
	s.listener = listener
	atomic.StoreInt32(&s.running, 1)
	
	log.Printf("[IPC] Socket server listening on %s", s.config.SocketPath)
	
	// Start accepting connections
	s.wg.Add(1)
	go s.acceptLoop()
	
	return nil
}

// Stop stops the socket server
func (s *SocketServer) Stop() error {
	if !atomic.CompareAndSwapInt32(&s.running, 1, 0) {
		return nil // Already stopped
	}
	
	log.Printf("[IPC] Stopping socket server...")
	
	// Cancel context to signal shutdown
	s.cancel()
	
	// Close listener
	if s.listener != nil {
		s.listener.Close()
	}
	
	// Close all client connections
	s.clientsMu.Lock()
	for clientID, conn := range s.clients {
		log.Printf("[IPC] Closing connection to client %s", clientID)
		conn.Close()
		delete(s.clients, clientID)
	}
	s.clientsMu.Unlock()
	
	// Wait for all goroutines to finish
	s.wg.Wait()
	
	// Remove socket file
	os.Remove(s.config.SocketPath)
	
	log.Printf("[IPC] Socket server stopped")
	return nil
}

// acceptLoop accepts new client connections
func (s *SocketServer) acceptLoop() {
	defer s.wg.Done()
	
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		
		// Set accept timeout
		if tcpListener, ok := s.listener.(*net.UnixListener); ok {
			tcpListener.SetDeadline(time.Now().Add(time.Second))
		}
		
		conn, err := s.listener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				continue // Timeout is expected
			}
			if atomic.LoadInt32(&s.running) == 0 {
				return // Server is shutting down
			}
			log.Printf("[IPC] Accept error: %v", err)
			continue
		}
		
		clientID := fmt.Sprintf("client_%d", atomic.AddInt64(&s.clientsTotal, 1))
		
		// Add client
		s.clientsMu.Lock()
		s.clients[clientID] = conn
		s.clientsMu.Unlock()
		
		atomic.AddInt64(&s.clientsConnected, 1)
		log.Printf("[IPC] Client connected: %s", clientID)
		
		// Handle client in separate goroutine
		s.wg.Add(1)
		go s.handleClient(clientID, conn)
	}
}

// handleClient handles a single client connection
func (s *SocketServer) handleClient(clientID string, conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		conn.Close()
		
		s.clientsMu.Lock()
		delete(s.clients, clientID)
		s.clientsMu.Unlock()
		
		atomic.AddInt64(&s.clientsConnected, -1)
		log.Printf("[IPC] Client disconnected: %s", clientID)
	}()
	
	// Set up JSON decoder
	decoder := json.NewDecoder(conn)
	
	// Handle messages
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		
		// Set read timeout
		conn.SetReadDeadline(time.Now().Add(time.Duration(s.config.TimeoutSeconds) * time.Second))
		
		var msg Message
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				return // Client disconnected
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // Timeout, try again
			}
			log.Printf("[IPC] Decode error from %s: %v", clientID, err)
			return
		}
		
		atomic.AddInt64(&s.messagesReceived, 1)
		
		// Handle message
		if err := s.handleMessage(clientID, conn, msg); err != nil {
			log.Printf("[IPC] Message handling error from %s: %v", clientID, err)
			return
		}
	}
}

// handleMessage processes a single message
func (s *SocketServer) handleMessage(clientID string, conn net.Conn, msg Message) error {
	switch msg.Type {
	case MsgTypeWriteEvent:
		if s.writeCallback != nil {
			var event WriteEvent
			if err := decodePayload(msg.Payload, &event); err != nil {
				return fmt.Errorf("decode write event: %w", err)
			}
			
			if err := s.writeCallback(event); err != nil {
				log.Printf("[IPC] Write callback error: %v", err)
			}
		}
		
	case MsgTypeBatchComplete:
		if s.batchCallback != nil {
			var event BatchCompleteEvent
			if err := decodePayload(msg.Payload, &event); err != nil {
				return fmt.Errorf("decode batch complete event: %w", err)
			}
			
			if err := s.batchCallback(event); err != nil {
				log.Printf("[IPC] Batch callback error: %v", err)
			}
		}
		
	case MsgTypeSync:
		if s.syncCallback != nil {
			var event SyncEvent
			if err := decodePayload(msg.Payload, &event); err != nil {
				return fmt.Errorf("decode sync event: %w", err)
			}
			
			if err := s.syncCallback(event); err != nil {
				log.Printf("[IPC] Sync callback error: %v", err)
			}
		}
		
	case MsgTypeTruncate:
		if s.truncateCallback != nil {
			var event TruncateEvent
			if err := decodePayload(msg.Payload, &event); err != nil {
				return fmt.Errorf("decode truncate event: %w", err)
			}
			
			if err := s.truncateCallback(event); err != nil {
				log.Printf("[IPC] Truncate callback error: %v", err)
			}
		}
		
	case MsgTypeStatusRequest:
		// Send status response
		status := StatusResponse{
			OpenFiles:     int(atomic.LoadInt64(&s.clientsConnected)),
			PendingWrites: 0, // Would be calculated from actual state
			TotalWrites:   atomic.LoadInt64(&s.messagesReceived),
			UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
		}
		
		response := Message{
			Type:      MsgTypeStatusResponse,
			Payload:   status,
			Timestamp: time.Now().UnixNano(),
			ID:        msg.ID,
		}
		
		return s.sendMessage(conn, response)
		
	case MsgTypeShutdown:
		log.Printf("[IPC] Shutdown requested by %s", clientID)
		go func() {
			// Graceful shutdown after a short delay
			time.Sleep(100 * time.Millisecond)
			s.Stop()
		}()
		
	default:
		log.Printf("[IPC] Unknown message type: %s", msg.Type)
	}
	
	return nil
}

// sendMessage sends a message to a client
func (s *SocketServer) sendMessage(conn net.Conn, msg Message) error {
	encoder := json.NewEncoder(conn)
	
	conn.SetWriteDeadline(time.Now().Add(time.Duration(s.config.TimeoutSeconds) * time.Second))
	
	if err := encoder.Encode(msg); err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	
	atomic.AddInt64(&s.messagesSent, 1)
	return nil
}

// Broadcast sends a message to all connected clients
func (s *SocketServer) Broadcast(msgType string, payload interface{}) error {
	msg := Message{
		Type:      msgType,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}
	
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	
	var errors []error
	for clientID, conn := range s.clients {
		if err := s.sendMessage(conn, msg); err != nil {
			log.Printf("[IPC] Broadcast error to %s: %v", clientID, err)
			errors = append(errors, err)
		}
	}
	
	if len(errors) > 0 {
		return fmt.Errorf("broadcast failed to %d clients", len(errors))
	}
	
	return nil
}

// GetStats returns server statistics
func (s *SocketServer) GetStats() map[string]interface{} {
	s.clientsMu.RLock()
	connectedClients := len(s.clients)
	s.clientsMu.RUnlock()
	
	return map[string]interface{}{
		"socket_path":        s.config.SocketPath,
		"connected_clients":  connectedClients,
		"total_clients":      atomic.LoadInt64(&s.clientsTotal),
		"messages_received":  atomic.LoadInt64(&s.messagesReceived),
		"messages_sent":      atomic.LoadInt64(&s.messagesSent),
		"uptime_seconds":     int64(time.Since(s.startTime).Seconds()),
		"running":            atomic.LoadInt32(&s.running) == 1,
	}
}

// SocketClient represents a client connection
type SocketClient struct {
	config     SocketConfig
	conn       net.Conn
	encoder    *json.Encoder
	decoder    *json.Decoder
	connected  bool
	connMu     sync.RWMutex
	lastMsgID  int64
}

// NewSocketClient creates a new socket client
func NewSocketClient(config SocketConfig) *SocketClient {
	return &SocketClient{
		config: config,
	}
}

// Connect connects to the socket server
func (c *SocketClient) Connect() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	
	if c.connected {
		return nil
	}
	
	var conn net.Conn
	var err error
	
	// Retry connection with exponential backoff
	for attempt := 0; attempt < c.config.RetryAttempts; attempt++ {
		conn, err = net.Dial("unix", c.config.SocketPath)
		if err == nil {
			break
		}
		
		if attempt < c.config.RetryAttempts-1 {
			delay := time.Duration(c.config.RetryDelayMs*(1<<attempt)) * time.Millisecond
			log.Printf("[IPC] Connection attempt %d failed, retrying in %v: %v", attempt+1, delay, err)
			time.Sleep(delay)
		}
	}
	
	if err != nil {
		return fmt.Errorf("failed to connect after %d attempts: %w", c.config.RetryAttempts, err)
	}
	
	c.conn = conn
	c.encoder = json.NewEncoder(conn)
	c.decoder = json.NewDecoder(conn)
	c.connected = true
	
	log.Printf("[IPC] Connected to socket server at %s", c.config.SocketPath)
	return nil
}

// Disconnect disconnects from the socket server
func (c *SocketClient) Disconnect() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	
	if !c.connected {
		return nil
	}
	
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	
	c.connected = false
	c.encoder = nil
	c.decoder = nil
	
	log.Printf("[IPC] Disconnected from socket server")
	return nil
}

// SendMessage sends a message to the server
func (c *SocketClient) SendMessage(msgType string, payload interface{}) error {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	
	if !c.connected || c.encoder == nil {
		return fmt.Errorf("not connected")
	}
	
	msg := Message{
		Type:      msgType,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
		ID:        fmt.Sprintf("msg_%d", atomic.AddInt64(&c.lastMsgID, 1)),
	}
	
	c.conn.SetWriteDeadline(time.Now().Add(time.Duration(c.config.TimeoutSeconds) * time.Second))
	
	if err := c.encoder.Encode(msg); err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	
	return nil
}

// ReceiveMessage receives a message from the server
func (c *SocketClient) ReceiveMessage() (Message, error) {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	
	if !c.connected || c.decoder == nil {
		return Message{}, fmt.Errorf("not connected")
	}
	
	c.conn.SetReadDeadline(time.Now().Add(time.Duration(c.config.TimeoutSeconds) * time.Second))
	
	var msg Message
	if err := c.decoder.Decode(&msg); err != nil {
		return Message{}, fmt.Errorf("decode message: %w", err)
	}
	
	return msg, nil
}

// SendWriteEvent sends a write event to the server
func (c *SocketClient) SendWriteEvent(event WriteEvent) error {
	return c.SendMessage(MsgTypeWriteEvent, event)
}

// SendBatchComplete sends a batch completion event
func (c *SocketClient) SendBatchComplete(event BatchCompleteEvent) error {
	return c.SendMessage(MsgTypeBatchComplete, event)
}

// SendSync sends a sync event
func (c *SocketClient) SendSync(event SyncEvent) error {
	return c.SendMessage(MsgTypeSync, event)
}

// SendTruncate sends a truncate event
func (c *SocketClient) SendTruncate(event TruncateEvent) error {
	return c.SendMessage(MsgTypeTruncate, event)
}

// RequestStatus requests server status
func (c *SocketClient) RequestStatus() (StatusResponse, error) {
	if err := c.SendMessage(MsgTypeStatusRequest, StatusRequest{}); err != nil {
		return StatusResponse{}, err
	}
	
	msg, err := c.ReceiveMessage()
	if err != nil {
		return StatusResponse{}, err
	}
	
	if msg.Type != MsgTypeStatusResponse {
		return StatusResponse{}, fmt.Errorf("expected status response, got %s", msg.Type)
	}
	
	var status StatusResponse
	if err := decodePayload(msg.Payload, &status); err != nil {
		return StatusResponse{}, fmt.Errorf("decode status response: %w", err)
	}
	
	return status, nil
}

// Helper function to decode message payload
func decodePayload(payload interface{}, target interface{}) error {
	// Convert via JSON for type safety
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	
	return json.Unmarshal(data, target)
}

// IsSocketAccessible checks if a socket path is accessible
func IsSocketAccessible(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	
	// Try to connect
	conn, err := net.Dial("unix", path)
	if err != nil {
		return false
	}
	defer conn.Close()
	
	return true
}

// CleanupSocket removes a socket file if it exists
func CleanupSocket(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CreateSocketClient creates and connects a new socket client
func CreateSocketClient(socketPath string) (*SocketClient, error) {
	config := DefaultSocketConfig()
	config.SocketPath = socketPath
	
	client := NewSocketClient(config)
	if err := client.Connect(); err != nil {
		return nil, err
	}
	
	return client, nil
}

// CreateSocketServer creates and starts a new socket server
func CreateSocketServer(socketPath string) (*SocketServer, error) {
	config := DefaultSocketConfig()
	config.SocketPath = socketPath
	
	server := NewSocketServer(config)
	if err := server.Start(); err != nil {
		return nil, err
	}
	
	return server, nil
}