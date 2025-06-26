package rsync

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestHTTPRsyncClient(t *testing.T) {
	// Simple test to verify the HTTP rsync client can be created
	client := NewHTTPRsyncClient()
	if client == nil {
		t.Fatal("Failed to create HTTP rsync client")
	}
	
	if client.userAgent != "DistSQLite-HTTPRsync/1.0" {
		t.Errorf("Unexpected user agent: %s", client.userAgent)
	}
	
	if client.timeout != 30*time.Second {
		t.Errorf("Unexpected timeout: %v", client.timeout)
	}
}

func TestHTTPRsyncServer(t *testing.T) {
	// Simple test to verify the HTTP rsync server can be created
	server := NewHTTPRsyncServer("test.db")
	if server == nil {
		t.Fatal("Failed to create HTTP rsync server")
	}
	
	if server.dbPath != "test.db" {
		t.Errorf("Unexpected database path: %s", server.dbPath)
	}
	
	if server.pageHashes == nil {
		t.Error("Page hashes map not initialized")
	}
}

func TestHTTPRsyncMessage(t *testing.T) {
	// Test message creation
	msg := &HTTPRsyncMessage{
		Type:      "ORIGIN_BEGIN",
		Protocol:  1,
		PageSize:  4096,
		PageCount: 100,
	}
	
	if msg.Type != "ORIGIN_BEGIN" {
		t.Errorf("Unexpected message type: %s", msg.Type)
	}
	
	if msg.PageSize != 4096 {
		t.Errorf("Unexpected page size: %d", msg.PageSize)
	}
}

func ExampleHTTPRsyncClient_usage() {
	// Example of how to use the HTTP rsync client
	client := NewHTTPRsyncClient()
	
	ctx := context.Background()
	
	// This would fail without a real database and server, but shows the API
	_, err := client.HTTPRsyncSync(ctx, "origin.db", "http://replica:8080")
	if err != nil {
		fmt.Printf("Sync failed (expected in test): %v\n", err)
	}
	
	// Output: Sync failed (expected in test): failed to get database info: database must be in WAL mode, got: delete
}