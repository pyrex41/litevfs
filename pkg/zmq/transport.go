package zmq

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pebbe/zmq4"
	"github.com/vmihailenco/msgpack/v5"
)

// Transport provides ZMQ-based communication for DistSQLite
type Transport struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex

	// ZMQ context
	zmqCtx *zmq4.Context

	// Lease coordination sockets
	leaseReq *zmq4.Socket // REQ socket for lease requests (client)
	leaseRep *zmq4.Socket // REP socket for lease serving (server)

	// Data replication sockets
	diffPub *zmq4.Socket // PUB for WAL diff broadcasting
	diffSub *zmq4.Socket // SUB for WAL diff receiving

	// Bulk operations sockets
	syncPush *zmq4.Socket // PUSH for sync job distribution
	syncPull *zmq4.Socket // PULL for sync job processing

	// Discovery & health sockets
	routerSock *zmq4.Socket // ROUTER for cluster management
	dealerSock *zmq4.Socket // DEALER for node communication

	// Configuration
	config *TransportConfig
	nodeID string
}

// TransportConfig defines transport configuration
type TransportConfig struct {
	// IPC settings (fastest for same-machine)
	IPCBasePath string `yaml:"ipc_base_path"`

	// TCP settings for network communication
	BindAddr string   `yaml:"bind_addr"`
	Peers    []string `yaml:"peers"`

	// Fly.io optimizations
	UsePrivateNetwork bool   `yaml:"use_private_network"`
	FlyIOAppName      string `yaml:"flyio_app_name"`

	// Timeouts
	RequestTimeout time.Duration `yaml:"request_timeout"`
	RetryAttempts  int           `yaml:"retry_attempts"`
}

// DefaultTransportConfig returns sensible defaults
func DefaultTransportConfig() *TransportConfig {
	return &TransportConfig{
		IPCBasePath:       "/tmp/distsqlite",
		BindAddr:          "0.0.0.0:5555",
		Peers:             []string{},
		UsePrivateNetwork: false,
		RequestTimeout:    5 * time.Second,
		RetryAttempts:     3,
	}
}

// NewTransport creates a new ZMQ transport
func NewTransport(config *TransportConfig, nodeID string) (*Transport, error) {
	if config == nil {
		config = DefaultTransportConfig()
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Create ZMQ context
	zmqCtx, err := zmq4.NewContext()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create ZMQ context: %w", err)
	}

	t := &Transport{
		ctx:    ctx,
		cancel: cancel,
		zmqCtx: zmqCtx,
		config: config,
		nodeID: nodeID,
	}

	return t, nil
}

// StartCoordinator initializes transport in coordinator mode
func (t *Transport) StartCoordinator() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var err error

	// Create REP socket for handling lease requests
	t.leaseRep, err = t.zmqCtx.NewSocket(zmq4.REP)
	if err != nil {
		return fmt.Errorf("failed to create lease REP socket: %w", err)
	}

	// Bind to address for lease requests
	bindAddr := fmt.Sprintf("tcp://%s", t.config.BindAddr)
	if err := t.leaseRep.Bind(bindAddr); err != nil {
		return fmt.Errorf("failed to bind lease REP to %s: %w", bindAddr, err)
	}

	// Create PUB socket for broadcasting cluster state
	t.diffPub, err = t.zmqCtx.NewSocket(zmq4.PUB)
	if err != nil {
		return fmt.Errorf("failed to create diff PUB socket: %w", err)
	}

	// Bind PUB socket for diff broadcasting
	pubAddr := fmt.Sprintf("tcp://%s", incrementPort(t.config.BindAddr, 1))
	if err := t.diffPub.Bind(pubAddr); err != nil {
		return fmt.Errorf("failed to bind diff PUB to %s: %w", pubAddr, err)
	}

	// Create ROUTER socket for cluster management
	t.routerSock, err = t.zmqCtx.NewSocket(zmq4.ROUTER)
	if err != nil {
		return fmt.Errorf("failed to create ROUTER socket: %w", err)
	}

	// Bind ROUTER socket
	routerAddr := fmt.Sprintf("tcp://%s", incrementPort(t.config.BindAddr, 2))
	if err := t.routerSock.Bind(routerAddr); err != nil {
		return fmt.Errorf("failed to bind ROUTER to %s: %w", routerAddr, err)
	}

	log.Printf("[ZMQ Transport] Coordinator started:")
	log.Printf("  Lease REP: %s", bindAddr)
	log.Printf("  Diff PUB:  %s", pubAddr)
	log.Printf("  Router:    %s", routerAddr)

	return nil
}

// StartNode initializes transport in node mode
func (t *Transport) StartNode() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var err error

	// Determine coordinator endpoints
	coordinatorEndpoints := t.getCoordinatorEndpoints()
	if len(coordinatorEndpoints) == 0 {
		return fmt.Errorf("no coordinator endpoints available")
	}

	// Create REQ socket for lease requests
	t.leaseReq, err = t.zmqCtx.NewSocket(zmq4.REQ)
	if err != nil {
		return fmt.Errorf("failed to create lease REQ socket: %w", err)
	}

	// Connect to coordinator lease endpoint
	leaseEndpoint := coordinatorEndpoints[0]
	if err := t.leaseReq.Connect(leaseEndpoint); err != nil {
		return fmt.Errorf("failed to connect lease REQ to %s: %w", leaseEndpoint, err)
	}

	// Create SUB socket for receiving WAL diffs
	t.diffSub, err = t.zmqCtx.NewSocket(zmq4.SUB)
	if err != nil {
		return fmt.Errorf("failed to create diff SUB socket: %w", err)
	}

	// Connect to coordinator diff endpoint
	diffEndpoint := incrementPort(leaseEndpoint, 1)
	if err := t.diffSub.Connect(diffEndpoint); err != nil {
		return fmt.Errorf("failed to connect diff SUB to %s: %w", diffEndpoint, err)
	}

	// Subscribe to all topics initially (can filter later)
	if err := t.diffSub.SetSubscribe(""); err != nil {
		return fmt.Errorf("failed to set SUB subscription: %w", err)
	}

	// Create DEALER socket for cluster communication
	t.dealerSock, err = t.zmqCtx.NewSocket(zmq4.DEALER)
	if err != nil {
		return fmt.Errorf("failed to create DEALER socket: %w", err)
	}

	// Set dealer identity to node ID
	if err := t.dealerSock.SetIdentity(t.nodeID); err != nil {
		return fmt.Errorf("failed to set DEALER identity: %w", err)
	}

	// Connect to coordinator router endpoint
	routerEndpoint := incrementPort(leaseEndpoint, 2)
	if err := t.dealerSock.Connect(routerEndpoint); err != nil {
		return fmt.Errorf("failed to connect DEALER to %s: %w", routerEndpoint, err)
	}

	log.Printf("[ZMQ Transport] Node %s connected:", t.nodeID)
	log.Printf("  Lease REQ: %s", leaseEndpoint)
	log.Printf("  Diff SUB:  %s", diffEndpoint)
	log.Printf("  Dealer:    %s", routerEndpoint)

	return nil
}

// getCoordinatorEndpoints determines coordinator endpoints based on config
func (t *Transport) getCoordinatorEndpoints() []string {
	var endpoints []string

	// Add configured peers first (most reliable)
	for _, peer := range t.config.Peers {
		if peer != "" {
			endpoint := fmt.Sprintf("tcp://%s", peer)
			endpoints = append(endpoints, endpoint)
		}
	}

	// Fly.io private network
	if t.config.UsePrivateNetwork && t.config.FlyIOAppName != "" {
		flyEndpoint := fmt.Sprintf("tcp://%s.internal:5555", t.config.FlyIOAppName)
		endpoints = append(endpoints, flyEndpoint)
	}

	// Try IPC last (only works if coordinator is on same machine)
	if t.config.IPCBasePath != "" {
		ipcEndpoint := fmt.Sprintf("ipc://%s/coordinator.sock", t.config.IPCBasePath)
		endpoints = append(endpoints, ipcEndpoint)
	}

	return endpoints
}

// Close shuts down the transport
func (t *Transport) Close() error {
	t.cancel()

	t.mu.Lock()
	defer t.mu.Unlock()

	var errors []error

	// Close all sockets
	sockets := []*zmq4.Socket{
		t.leaseReq, t.leaseRep,
		t.diffPub, t.diffSub,
		t.syncPush, t.syncPull,
		t.routerSock, t.dealerSock,
	}

	for _, sock := range sockets {
		if sock != nil {
			if err := sock.Close(); err != nil {
				errors = append(errors, err)
			}
		}
	}

	// Terminate ZMQ context
	if t.zmqCtx != nil {
		t.zmqCtx.Term()
	}

	if len(errors) > 0 {
		return fmt.Errorf("transport close errors: %v", errors)
	}

	log.Printf("[ZMQ Transport] Transport closed")
	return nil
}

// Helper functions

// incrementPort increments a host:port address by the given offset
func incrementPort(addr string, offset int) string {
	// Parse host:port
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if portNum, err := strconv.Atoi(port); err == nil {
			newPort := portNum + offset
			return net.JoinHostPort(host, strconv.Itoa(newPort))
		}
	}
	// Fallback: assume it's a URL and modify
	if len(addr) > 6 && addr[:6] == "tcp://" {
		baseAddr := addr[6:] // Remove tcp:// prefix
		if host, port, err := net.SplitHostPort(baseAddr); err == nil {
			if portNum, err := strconv.Atoi(port); err == nil {
				newPort := portNum + offset
				return "tcp://" + net.JoinHostPort(host, strconv.Itoa(newPort))
			}
		}
	}
	return addr // Return unchanged if parsing fails
}

// sendMessage sends a msgpack-encoded message on a socket
func (t *Transport) sendMessage(socket *zmq4.Socket, msg interface{}) error {
	data, err := msgpack.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	_, err = socket.SendBytes(data, 0)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	return nil
}

// receiveMessage receives and decodes a msgpack message from a socket
func (t *Transport) receiveMessage(socket *zmq4.Socket, msg interface{}) error {
	data, err := socket.RecvBytes(0)
	if err != nil {
		return fmt.Errorf("failed to receive message: %w", err)
	}

	err = msgpack.Unmarshal(data, msg)
	if err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	return nil
}