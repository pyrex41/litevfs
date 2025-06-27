package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	
	"distsqlite/pkg/zmq" 
)

func TestZMQCurveAuthentication(t *testing.T) {
	t.Run("CurveDisabled", testCurveDisabled)
	t.Run("CurveEnabled", testCurveEnabled)
	t.Run("CurveMismatchedKeys", testCurveMismatchedKeys)
	t.Run("CurveInvalidKeys", testCurveInvalidKeys)
}

func testCurveDisabled(t *testing.T) {
	// Test normal operation with CURVE disabled
	coordinatorConfig := &zmq.TransportConfig{
		BindAddr: "127.0.0.1:15555",
		Security: zmq.SecurityConfig{
			Enabled:      false,
			CurveEnabled: false,
		},
		RequestTimeout: 2 * time.Second,
		RetryAttempts:  1,
	}

	nodeConfig := &zmq.TransportConfig{
		Peers: []string{"127.0.0.1:15555"},
		Security: zmq.SecurityConfig{
			Enabled:      false,
			CurveEnabled: false,
		},
		RequestTimeout: 2 * time.Second,
		RetryAttempts:  1,
	}

	// Start coordinator
	coordinator, err := zmq.NewTransport(coordinatorConfig, "coord-1")
	require.NoError(t, err)
	defer coordinator.Close()

	err = coordinator.StartCoordinator()
	require.NoError(t, err)

	// Give coordinator time to bind
	time.Sleep(100 * time.Millisecond)

	// Start node
	node, err := zmq.NewTransport(nodeConfig, "node-1")
	require.NoError(t, err)
	defer node.Close()

	err = node.StartNode()
	require.NoError(t, err)

	// Test basic lease operation
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := node.AcquireLease(ctx, "/test/db1", 30)
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.NotEmpty(t, resp.FenceToken)

	t.Logf("CURVE disabled test passed - lease acquired with token %d", resp.FenceToken)
}

func testCurveEnabled(t *testing.T) {
	// Test operation with CURVE authentication enabled using test keys
	coordinatorConfig := &zmq.TransportConfig{
		BindAddr: "127.0.0.1:15556",
		Security: zmq.SecurityConfig{
			Enabled:         true,
			CurveEnabled:    true,
			ServerPublicKey: zmq.TestServerPublicKey,
			ServerSecretKey: zmq.TestServerSecretKey,
		},
		RequestTimeout: 2 * time.Second,
		RetryAttempts:  1,
	}

	nodeConfig := &zmq.TransportConfig{
		Peers: []string{"127.0.0.1:15556"},
		Security: zmq.SecurityConfig{
			Enabled:         true,
			CurveEnabled:    true,
			ServerPublicKey: zmq.TestServerPublicKey,
			ClientPublicKey: zmq.TestClientPublicKey,
			ClientSecretKey: zmq.TestClientSecretKey,
		},
		RequestTimeout: 2 * time.Second,
		RetryAttempts:  1,
	}

	// Start coordinator with CURVE enabled
	coordinator, err := zmq.NewTransport(coordinatorConfig, "coord-2")
	require.NoError(t, err)
	defer coordinator.Close()

	err = coordinator.StartCoordinator()
	require.NoError(t, err)

	// Give coordinator time to bind
	time.Sleep(100 * time.Millisecond)

	// Start node with CURVE enabled
	node, err := zmq.NewTransport(nodeConfig, "node-2")
	require.NoError(t, err)
	defer node.Close()

	err = node.StartNode()
	require.NoError(t, err)

	// Test authenticated lease operation
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := node.AcquireLease(ctx, "/test/db2", 30)
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.NotEmpty(t, resp.FenceToken)

	t.Logf("CURVE enabled test passed - authenticated lease acquired with token %d", resp.FenceToken)
}

func testCurveMismatchedKeys(t *testing.T) {
	// Test that mismatched keys result in authentication failure
	coordinatorConfig := &zmq.TransportConfig{
		BindAddr: "127.0.0.1:15557",
		Security: zmq.SecurityConfig{
			Enabled:         true,
			CurveEnabled:    true,
			ServerPublicKey: zmq.TestServerPublicKey,
			ServerSecretKey: zmq.TestServerSecretKey,
		},
		RequestTimeout: 2 * time.Second,
		RetryAttempts:  1,
	}

	// Use wrong server public key for client
	nodeConfig := &zmq.TransportConfig{
		Peers: []string{"127.0.0.1:15557"},
		Security: zmq.SecurityConfig{
			Enabled:         true,
			CurveEnabled:    true,
			ServerPublicKey: "WrongKeyXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", // Wrong key
			ClientPublicKey: zmq.TestClientPublicKey,
			ClientSecretKey: zmq.TestClientSecretKey,
		},
		RequestTimeout: 2 * time.Second,
		RetryAttempts:  1,
	}

	// Start coordinator
	coordinator, err := zmq.NewTransport(coordinatorConfig, "coord-3")
	require.NoError(t, err)
	defer coordinator.Close()

	err = coordinator.StartCoordinator()
	require.NoError(t, err)

	// Give coordinator time to bind
	time.Sleep(100 * time.Millisecond)

	// Start node with wrong key
	node, err := zmq.NewTransport(nodeConfig, "node-3")
	require.NoError(t, err)
	defer node.Close()

	// This should fail due to wrong server key
	err = node.StartNode()
	// Note: Connection errors might not surface until first message attempt
	if err == nil {
		// If StartNode succeeds, the authentication failure will occur on first message
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err = node.AcquireLease(ctx, "/test/db3", 30)
		assert.Error(t, err, "Expected authentication failure with mismatched keys")
		t.Logf("CURVE mismatched keys test passed - authentication failed as expected: %v", err)
	} else {
		// StartNode failed due to key mismatch
		assert.Error(t, err)
		t.Logf("CURVE mismatched keys test passed - connection failed as expected: %v", err)
	}
}

func testCurveInvalidKeys(t *testing.T) {
	// Test that invalid key formats are handled gracefully
	coordinatorConfig := &zmq.TransportConfig{
		BindAddr: "127.0.0.1:15558",
		Security: zmq.SecurityConfig{
			Enabled:         true,
			CurveEnabled:    true,
			ServerPublicKey: "InvalidKey",  // Invalid Z85 format
			ServerSecretKey: "InvalidKey",  // Invalid Z85 format
		},
		RequestTimeout: 2 * time.Second,
		RetryAttempts:  1,
	}

	// Start coordinator with invalid keys
	coordinator, err := zmq.NewTransport(coordinatorConfig, "coord-4")
	require.NoError(t, err)
	defer coordinator.Close()

	// This should fail due to invalid key format
	err = coordinator.StartCoordinator()
	assert.Error(t, err, "Expected error with invalid key format")
	t.Logf("CURVE invalid keys test passed - rejected invalid keys as expected: %v", err)
}

func TestCurveKeyGeneration(t *testing.T) {
	// Test that the provided test keys are valid Z85 format
	assert.Len(t, zmq.TestServerPublicKey, 40, "Server public key should be 40 Z85 characters")
	assert.Len(t, zmq.TestServerSecretKey, 40, "Server secret key should be 40 Z85 characters")
	assert.Len(t, zmq.TestClientPublicKey, 40, "Client public key should be 40 Z85 characters")
	assert.Len(t, zmq.TestClientSecretKey, 40, "Client secret key should be 40 Z85 characters")

	// Verify they contain only valid Z85 characters
	validZ85 := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ.-:+=^!/*?&<>()[]{}@%$#"
	
	keys := []string{
		zmq.TestServerPublicKey,
		zmq.TestServerSecretKey,
		zmq.TestClientPublicKey,
		zmq.TestClientSecretKey,
	}

	for _, key := range keys {
		for _, char := range key {
			assert.Contains(t, validZ85, string(char), "Key contains invalid Z85 character: %c", char)
		}
	}

	t.Log("CURVE key validation passed - all test keys are valid Z85 format")
}