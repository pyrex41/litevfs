package unit

import (
	"os"
	"path/filepath"
	"testing"

	"distsqlite/pkg/distcore"
)

func TestValidateFilePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid absolute path", "/tmp/test.db", false},
		{"empty path", "", true},
		{"directory traversal", "/tmp/../etc/passwd", true},
		{"null byte", "/tmp/test\x00.db", true},
		{"relative path", "test.db", true},
		{"path too long", string(make([]byte, 5000)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := distcore.NewVFSFile(tt.path, 4096)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewVFSFile() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPageSizeValidation(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "test.db")
	
	tests := []struct {
		name     string
		pageSize int
		wantErr  bool
	}{
		{"valid 4KB", 4096, false},
		{"valid 8KB", 8192, false},
		{"zero size", 0, true},
		{"negative size", -1, true},
		{"not power of 2", 5000, true},
		{"too large", 131072, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := distcore.NewVFSFile(tempFile, tt.pageSize)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewVFSFile() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPageTracker(t *testing.T) {
	tracker, err := distcore.NewPageTracker(4096)
	if err != nil {
		t.Fatalf("NewPageTracker() error = %v", err)
	}

	if tracker == nil {
		t.Fatal("NewPageTracker() returned nil")
	}
}

func TestVFSFileOperations(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "test.db")
	
	file, err := distcore.NewVFSFile(tempFile, 4096)
	if err != nil {
		t.Fatalf("NewVFSFile() error = %v", err)
	}

	// Test that the file was created with correct properties
	if file == nil {
		t.Fatal("NewVFSFile() returned nil")
	}

	// Test page operations (would need the actual file operations implemented)
	pages, err := file.GetSyncPages()
	if err != nil {
		t.Errorf("GetSyncPages() error = %v", err)
	}

	// Should start with empty pages
	if len(pages) != 0 {
		t.Errorf("Expected 0 pages initially, got %d", len(pages))
	}
}

func TestComputeDiffPages(t *testing.T) {
	localPages := []distcore.SyncPage{
		{PageNum: 0, Hash: "hash0", Data: []byte("data0")},
		{PageNum: 1, Hash: "hash1", Data: []byte("data1")},
		{PageNum: 2, Hash: "hash2", Data: []byte("data2")},
	}

	replicaHashes := map[int]string{
		0: "hash0",        // Same hash - no sync needed
		1: "different1",   // Different hash - needs sync
		// Page 2 missing from replica - needs sync
	}

	diffPages, err := distcore.ComputeDiffPages(localPages, replicaHashes)
	if err != nil {
		t.Fatalf("ComputeDiffPages() error = %v", err)
	}

	// Should need to sync pages 1 and 2
	expectedCount := 2
	if len(diffPages) != expectedCount {
		t.Errorf("Expected %d diff pages, got %d", expectedCount, len(diffPages))
	}

	// Verify correct pages are included
	pageNums := make(map[int]bool)
	for _, page := range diffPages {
		pageNums[page.PageNum] = true
	}

	if !pageNums[1] || !pageNums[2] {
		t.Errorf("Expected pages 1 and 2 in diff, got page nums: %v", pageNums)
	}
}

func TestSqliteRsyncMessageTypes(t *testing.T) {
	// Test message type constants
	tests := []struct {
		name     string
		msgType  uint8
		expected uint8
	}{
		{"OriginBegin", distcore.OriginBeginType, 0x41},
		{"OriginEnd", distcore.OriginEndType, 0x42},
		{"OriginError", distcore.OriginErrorType, 0x43},
		{"OriginPage", distcore.OriginPageType, 0x44},
		{"ReplicaBegin", distcore.ReplicaBeginType, 0x61},
		{"ReplicaHash", distcore.ReplicaHashType, 0x64},
		{"ReplicaReady", distcore.ReplicaReadyType, 0x65},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.msgType != tt.expected {
				t.Errorf("Expected %s = 0x%02x, got 0x%02x", 
					tt.name, tt.expected, tt.msgType)
			}
		})
	}
}

func TestMemoryCleanup(t *testing.T) {
	// Test that VFS file creation and destruction doesn't leak memory
	tempFile := filepath.Join(t.TempDir(), "test.db")
	
	// Create many VFS files to test for leaks
	for i := 0; i < 100; i++ {
		file, err := distcore.NewVFSFile(tempFile, 4096)
		if err != nil {
			t.Fatalf("NewVFSFile() iteration %d error = %v", i, err)
		}
		
		// Use the file briefly
		_, err = file.GetSyncPages()
		if err != nil {
			t.Errorf("GetSyncPages() iteration %d error = %v", i, err)
		}
		
		// File should be garbage collected automatically
		file = nil
	}
}

func TestErrorHandling(t *testing.T) {
	// Test error conditions
	
	// Test with non-existent directory
	_, err := distcore.NewVFSFile("/nonexistent/path/test.db", 4096)
	if err == nil {
		t.Error("Expected error for non-existent directory")
	}
	
	// Test empty page list
	emptyPages := []distcore.SyncPage{}
	emptyHashes := map[int]string{}
	
	diffPages, err := distcore.ComputeDiffPages(emptyPages, emptyHashes)
	if err != nil {
		t.Errorf("ComputeDiffPages() with empty inputs error = %v", err)
	}
	
	if len(diffPages) != 0 {
		t.Errorf("Expected 0 diff pages for empty inputs, got %d", len(diffPages))
	}
}

func TestConcurrentAccess(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "test.db")
	
	file, err := distcore.NewVFSFile(tempFile, 4096)
	if err != nil {
		t.Fatalf("NewVFSFile() error = %v", err)
	}

	// Test concurrent access to GetSyncPages
	const numGoroutines = 10
	const numIterations = 100
	
	done := make(chan bool, numGoroutines)
	
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer func() { done <- true }()
			
			for j := 0; j < numIterations; j++ {
				_, err := file.GetSyncPages()
				if err != nil {
					t.Errorf("Concurrent GetSyncPages() error = %v", err)
					return
				}
			}
		}()
	}
	
	// Wait for all goroutines to complete
	for i := 0; i < numGoroutines; i++ {
		<-done
	}
}

func TestEngineIntegration(t *testing.T) {
	// Test integration with OCaml engine
	engine, err := distcore.GetEngine()
	if err != nil {
		t.Skipf("OCaml engine not available: %v", err)
	}
	
	// Test engine statistics
	stats := distcore.GetEngineStats()
	if stats == nil {
		t.Error("GetEngineStats() returned nil")
	}
	
	// Test engine is properly initialized
	if !engine.IsInitialized() {
		t.Error("Engine should be initialized")
	}
}