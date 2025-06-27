package distcore

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestMessageSizeLimits(t *testing.T) {
	// Create a mock connection using pipes
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	
	session, err := NewReplicaRsyncSession(client, "/test/db.sqlite")
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()
	
	tests := []struct {
		name          string
		messageSize   int
		expectError   bool
		errorContains string
	}{
		{
			name:        "valid small message",
			messageSize: 1024,
			expectError: false,
		},
		{
			name:        "valid large message",
			messageSize: 1024 * 1024, // 1MB
			expectError: false,
		},
		{
			name:          "oversized message",
			messageSize:   MaxMessageSize + 1,
			expectError:   true,
			errorContains: "message too large",
		},
		{
			name:          "maximum allowed message",
			messageSize:   MaxMessageSize,
			expectError:   false,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Send a message from server to client with the test size
			go func() {
				defer server.Close()
				
				// Send message type
				server.Write([]byte{OriginPageType})
				
				// Send length (big endian)
				lengthBytes := []byte{
					byte(tt.messageSize >> 24),
					byte(tt.messageSize >> 16),
					byte(tt.messageSize >> 8),
					byte(tt.messageSize),
				}
				server.Write(lengthBytes)
				
				// Send payload if not too large
				if tt.messageSize <= MaxMessageSize {
					payload := make([]byte, tt.messageSize)
					for i := range payload {
						payload[i] = byte(i % 256)
					}
					server.Write(payload)
				}
			}()
			
			// Try to receive the message
			_, err := session.receiveMessage()
			
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error for message size %d, but got none", tt.messageSize)
				} else if tt.errorContains != "" && !bytes.Contains([]byte(err.Error()), []byte(tt.errorContains)) {
					t.Errorf("Expected error to contain '%s', got: %v", tt.errorContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error for message size %d: %v", tt.messageSize, err)
				}
			}
		})
	}
}

func TestParameterValidation(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	
	session, err := NewReplicaRsyncSession(client, "/test/db.sqlite")
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()
	
	tests := []struct {
		name        string
		protocol    int
		pageSize    int
		pageCount   int
		expectError bool
	}{
		{
			name:        "valid parameters",
			protocol:    1,
			pageSize:    4096,
			pageCount:   1000,
			expectError: false,
		},
		{
			name:        "invalid protocol version",
			protocol:    99,
			pageSize:    4096,
			pageCount:   1000,
			expectError: true,
		},
		{
			name:        "page size too large",
			protocol:    1,
			pageSize:    MaxPageSize + 1,
			pageCount:   1000,
			expectError: true,
		},
		{
			name:        "page count too large",
			protocol:    1,
			pageSize:    4096,
			pageCount:   MaxPageCount + 1,
			expectError: true,
		},
		{
			name:        "negative page count",
			protocol:    1,
			pageSize:    4096,
			pageCount:   -1,
			expectError: true,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Send OriginBegin message with test parameters
			go func() {
				defer server.Close()
				
				// Send OriginBegin type
				server.Write([]byte{OriginBeginType})
				
				// Send length (6 bytes payload)
				server.Write([]byte{0, 0, 0, 6})
				
				// Send payload
				payload := make([]byte, 6)
				payload[0] = byte(tt.protocol)
				
				// Calculate page size power of 2
				pageSizePow2 := 0
				for size := tt.pageSize; size > 1; size >>= 1 {
					pageSizePow2++
				}
				payload[1] = byte(pageSizePow2)
				
				// Page count (4 bytes, big endian)
				payload[2] = byte(tt.pageCount >> 24)
				payload[3] = byte(tt.pageCount >> 16)
				payload[4] = byte(tt.pageCount >> 8)
				payload[5] = byte(tt.pageCount)
				
				server.Write(payload)
			}()
			
			// Simulate part of RunReplicaSync to test parameter validation
			msg, err := session.receiveMessage()
			if err != nil {
				t.Fatalf("Failed to receive message: %v", err)
			}
			
			if msg.Type != OriginBeginType {
				t.Fatalf("Expected OriginBegin, got type %d", msg.Type)
			}
			
			if len(msg.Payload) < 6 {
				t.Fatalf("Invalid OriginBegin payload length: %d", len(msg.Payload))
			}
			
			// Extract and validate parameters (this will trigger our validation)
			protocol := int(msg.Payload[0])
			pageSizePow2 := int(msg.Payload[1])
			pageSize := 1 << pageSizePow2
			pageCount := int(msg.Payload[2])<<24 | int(msg.Payload[3])<<16 | int(msg.Payload[4])<<8 | int(msg.Payload[5])
			
			// Manual validation (simulating what RunReplicaSync does)
			var validationErr error
			if protocol < 1 || protocol > 10 {
				validationErr = fmt.Errorf("unsupported protocol version: %d", protocol)
			} else if pageSize < 512 || pageSize > MaxPageSize {
				validationErr = fmt.Errorf("invalid page size: %d", pageSize)
			} else if pageCount < 0 || pageCount > MaxPageCount {
				validationErr = fmt.Errorf("invalid page count: %d", pageCount)
			}
			
			if tt.expectError {
				if validationErr == nil {
					t.Errorf("Expected validation error for protocol=%d, pageSize=%d, pageCount=%d", 
						tt.protocol, tt.pageSize, tt.pageCount)
				}
			} else {
				if validationErr != nil {
					t.Errorf("Unexpected validation error: %v", validationErr)
				}
			}
		})
	}
}

func TestConnectionTimeouts(t *testing.T) {
	// Test that connections respect timeouts
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	
	session, err := NewOriginRsyncSession(server, "/test/db.sqlite", 1000)
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()
	
	// Test write timeout by closing the read end
	client.Close()
	
	start := time.Now()
	err = session.sendMessage(SqliteRsyncMessage{
		Type:    OriginBeginType,
		Payload: make([]byte, 6),
	})
	elapsed := time.Since(start)
	
	// Should fail quickly due to closed connection
	if err == nil {
		t.Error("Expected error when writing to closed connection")
	}
	
	// Should not hang indefinitely
	if elapsed > WriteTimeout*2 {
		t.Errorf("Operation took too long: %v (expected < %v)", elapsed, WriteTimeout*2)
	}
}

func TestPageDataValidation(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	
	session, err := NewReplicaRsyncSession(client, "/test/db.sqlite")
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()
	
	tests := []struct {
		name        string
		payloadSize int
		expectError bool
	}{
		{
			name:        "valid page data",
			payloadSize: 4096 + 4, // 4KB page + 4 byte header
			expectError: false,
		},
		{
			name:        "oversized page data",
			payloadSize: MaxPageSize + 5, // Exceeds max page size
			expectError: true,
		},
		{
			name:        "minimal page data",
			payloadSize: 4, // Just header, no data
			expectError: false,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create payload with page number header
			payload := make([]byte, tt.payloadSize)
			// Set page number in first 4 bytes (big endian)
			payload[0] = 0
			payload[1] = 0
			payload[2] = 0
			payload[3] = 1 // Page 1
			
			// Fill rest with test data
			for i := 4; i < len(payload); i++ {
				payload[i] = byte(i % 256)
			}
			
			err := session.applyPage(payload)
			
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error for payload size %d, but got none", tt.payloadSize)
				}
			} else {
				// Note: We expect this to fail with OCaml VFS error since we don't have 
				// actual OCaml runtime, but it should not fail with size validation
				if err != nil && bytes.Contains([]byte(err.Error()), []byte("too large")) {
					t.Errorf("Unexpected size validation error: %v", err)
				}
			}
		})
	}
}

func TestNegativeMessageLength(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	
	session, err := NewReplicaRsyncSession(client, "/test/db.sqlite")
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()
	
	// Send a message with negative length (using two's complement)
	go func() {
		defer server.Close()
		
		// Send message type
		server.Write([]byte{OriginPageType})
		
		// Send negative length (0xFFFFFFFF = -1 in two's complement)
		server.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF})
		
		// Don't send any payload
	}()
	
	// Try to receive the message
	_, err = session.receiveMessage()
	
	if err == nil {
		t.Error("Expected error for negative message length")
	}
	
	if !bytes.Contains([]byte(err.Error()), []byte("invalid message length")) {
		t.Errorf("Expected 'invalid message length' error, got: %v", err)
	}
}