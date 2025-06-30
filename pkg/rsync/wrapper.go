package rsync

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Wrapper wraps sqlite3_rsync for database synchronization
type Wrapper struct {
	// Configuration
	compression     string
	bandwidthLimit  string
	timeout         time.Duration
	maxRetries      int
	
	// Progress tracking
	progressWriter io.Writer
}

// NewWrapper creates a new sqlite3_rsync wrapper
func NewWrapper() *Wrapper {
	return &Wrapper{
		compression:    "zstd",
		bandwidthLimit: "100M", // Default bandwidth limit
		timeout:        30 * time.Minute,
		maxRetries:     3,
		progressWriter: os.Stdout,
	}
}

// SetCompression sets the compression algorithm (zstd, gzip, none)
func (w *Wrapper) SetCompression(compression string) {
	w.compression = compression
}

// SetBandwidthLimit sets the bandwidth limit (e.g., "10M", "1G")
func (w *Wrapper) SetBandwidthLimit(limit string) {
	w.bandwidthLimit = limit
}

// SetTimeout sets the operation timeout
func (w *Wrapper) SetTimeout(timeout time.Duration) {
	w.timeout = timeout
}

// SetProgressWriter sets where progress output should go
func (w *Wrapper) SetProgressWriter(writer io.Writer) {
	w.progressWriter = writer
}

// Sync synchronizes a database file to a remote location
func (w *Wrapper) Sync(source, dest string) error {
	// Validate source file exists
	if _, err := os.Stat(source); os.IsNotExist(err) {
		return fmt.Errorf("source database does not exist: %s", source)
	}
	
	// Find sqlite3_rsync binary
	rsyncPath, err := w.findRsyncBinary()
	if err != nil {
		return fmt.Errorf("sqlite3_rsync not found: %w", err)
	}
	
	// Build command arguments
	args := w.buildArgs(source, dest)
	
	log.Printf("[DistSQLite] Executing sqlite3_rsync: %s %s", rsyncPath, strings.Join(args, " "))
	
	// Execute with retries
	return w.executeWithRetries(rsyncPath, args)
}

// SyncLocal synchronizes between two local database files
func (w *Wrapper) SyncLocal(source, dest string) error {
	// For local sync, we can use a simpler approach
	return w.Sync(source, dest)
}

// findRsyncBinary locates the sqlite3_rsync binary
func (w *Wrapper) findRsyncBinary() (string, error) {
	// Try common locations
	locations := []string{
		"./sqlite3_rsync",           // Current directory
		"./build/sqlite3_rsync",     // Build directory
		"/usr/local/bin/sqlite3_rsync", // System installation
		"sqlite3_rsync",             // In PATH
	}
	
	for _, location := range locations {
		if path, err := exec.LookPath(location); err == nil {
			return path, nil
		}
	}
	
	return "", fmt.Errorf("sqlite3_rsync binary not found in any of: %v", locations)
}

// buildArgs constructs the command line arguments for sqlite3_rsync
func (w *Wrapper) buildArgs(source, dest string) []string {
	args := []string{}
	
	// Add compression if specified
	if w.compression != "" && w.compression != "none" {
		args = append(args, "--compression", w.compression)
	}
	
	// Add bandwidth limit if specified
	if w.bandwidthLimit != "" {
		args = append(args, "--bandwidth-limit", w.bandwidthLimit)
	}
	
	// Add progress reporting
	args = append(args, "--progress")
	
	// Add verbose output for debugging
	args = append(args, "--verbose")
	
	// Add source and destination
	args = append(args, source, dest)
	
	return args
}

// executeWithRetries executes the command with retries
func (w *Wrapper) executeWithRetries(command string, args []string) error {
	var lastErr error
	
	for attempt := 1; attempt <= w.maxRetries; attempt++ {
		if attempt > 1 {
			log.Printf("[DistSQLite] Retry attempt %d/%d for sqlite3_rsync", attempt, w.maxRetries)
			time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
		}
		
		err := w.executeCommand(command, args)
		if err == nil {
			return nil // Success
		}
		
		lastErr = err
		log.Printf("[DistSQLite] sqlite3_rsync attempt %d failed: %v", attempt, err)
	}
	
	return fmt.Errorf("sqlite3_rsync failed after %d attempts: %w", w.maxRetries, lastErr)
}

// executeCommand executes a single command attempt
func (w *Wrapper) executeCommand(command string, args []string) error {
	cmd := exec.Command(command, args...)
	
	// Set up progress tracking
	if w.progressWriter != nil {
		cmd.Stdout = w.progressWriter
		cmd.Stderr = w.progressWriter
	}
	
	// Set timeout
	if w.timeout > 0 {
		// For Go 1.20+, we can use CommandContext
		// For older versions, we'll use a simple timeout
		done := make(chan error, 1)
		go func() {
			done <- cmd.Run()
		}()
		
		select {
		case err := <-done:
			return err
		case <-time.After(w.timeout):
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			return fmt.Errorf("sqlite3_rsync timed out after %v", w.timeout)
		}
	}
	
	return cmd.Run()
}

// CheckBinary verifies that sqlite3_rsync is available and working
func (w *Wrapper) CheckBinary() error {
	rsyncPath, err := w.findRsyncBinary()
	if err != nil {
		return err
	}
	
	// Try to run with --version to verify it works
	cmd := exec.Command(rsyncPath, "--version")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("sqlite3_rsync appears broken: %w", err)
	}
	
	log.Printf("[DistSQLite] Found sqlite3_rsync: %s", strings.TrimSpace(string(output)))
	return nil
}

// EstimateTransferTime estimates how long a sync might take
func (w *Wrapper) EstimateTransferTime(source string) (time.Duration, error) {
	stat, err := os.Stat(source)
	if err != nil {
		return 0, err
	}
	
	fileSize := stat.Size()
	
	// Parse bandwidth limit to estimate transfer time
	// This is a rough estimate - actual performance depends on many factors
	var bytesPerSecond int64 = 10 * 1024 * 1024 // Default: 10MB/s
	
	if w.bandwidthLimit != "" {
		// Parse bandwidth limit (simplified)
		limit := strings.ToUpper(w.bandwidthLimit)
		if strings.HasSuffix(limit, "M") {
			if mbps := strings.TrimSuffix(limit, "M"); mbps != "" {
				// Convert Mbps to bytes per second (rough estimate)
				bytesPerSecond = 1024 * 1024 // Will be multiplied by parsed number
			}
		}
	}
	
	// Add overhead for compression and network latency
	estimatedSeconds := float64(fileSize) / float64(bytesPerSecond) * 1.5 // 50% overhead
	
	return time.Duration(estimatedSeconds) * time.Second, nil
}

// GetStats returns statistics about recent sync operations
func (w *Wrapper) GetStats() map[string]interface{} {
	// TODO: Implement statistics tracking
	// This would track things like:
	// - Number of syncs performed
	// - Average sync time
	// - Bytes transferred
	// - Success/failure rates
	
	return map[string]interface{}{
		"compression":     w.compression,
		"bandwidth_limit": w.bandwidthLimit,
		"timeout":         w.timeout.String(),
		"max_retries":     w.maxRetries,
	}
}

// ValidateDestination checks if a destination is reachable and writable
func (w *Wrapper) ValidateDestination(dest string) error {
	// For local destinations, check if directory exists and is writable
	if !strings.Contains(dest, ":") {
		// Local path
		dir := filepath.Dir(dest)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("cannot create destination directory: %w", err)
		}
		
		// Try to create a test file
		testFile := filepath.Join(dir, ".distsqlite-test")
		if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
			return fmt.Errorf("destination directory not writable: %w", err)
		}
		os.Remove(testFile) // Clean up
		
		return nil
	}
	
	// For remote destinations, we'd need to implement SSH connectivity check
	// For now, assume it's valid
	log.Printf("[DistSQLite] Assuming remote destination %s is valid", dest)
	return nil
}

// VerifyFile performs a dry-run verification to check if files differ
func (w *Wrapper) VerifyFile(ctx interface{}, source, dest string) error {
	// Use rsync's dry-run mode to check for differences
	rsyncPath, err := w.findRsyncBinary()
	if err != nil {
		// Fall back to regular rsync if sqlite3_rsync not available
		return w.verifyWithRegularRsync(source, dest)
	}
	
	// Build dry-run arguments
	args := []string{"--dry-run", "--itemize-changes"}
	
	// Add compression if specified
	if w.compression != "" && w.compression != "none" {
		args = append(args, "--compression", w.compression)
	}
	
	// Add source and destination
	args = append(args, source, dest)
	
	log.Printf("[DistSQLite] Verifying consistency: %s %s", rsyncPath, strings.Join(args, " "))
	
	// Execute dry-run
	cmd := exec.Command(rsyncPath, args...)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}
	
	// Check if any differences were found
	outputStr := string(output)
	if len(strings.TrimSpace(outputStr)) > 0 {
		return fmt.Errorf("files differ: %s", outputStr)
	}
	
	return nil
}

// verifyWithRegularRsync falls back to regular rsync for verification
func (w *Wrapper) verifyWithRegularRsync(source, dest string) error {
	// Find regular rsync
	rsyncPath, err := exec.LookPath("rsync")
	if err != nil {
		return fmt.Errorf("neither sqlite3_rsync nor rsync available for verification")
	}
	
	// Use rsync's checksum mode for verification
	args := []string{
		"--dry-run",
		"--checksum",
		"--itemize-changes",
		source,
		dest,
	}
	
	log.Printf("[DistSQLite] Verifying with rsync: %s %s", rsyncPath, strings.Join(args, " "))
	
	cmd := exec.Command(rsyncPath, args...)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("rsync verification failed: %w", err)
	}
	
	// Check if any differences were found
	outputStr := string(output)
	if len(strings.TrimSpace(outputStr)) > 0 {
		return fmt.Errorf("files differ: %s", outputStr)
	}
	
	return nil
}

// SyncFile is an alias for Sync to match the interface used by the hybrid replicator
func (w *Wrapper) SyncFile(ctx interface{}, source, dest string) error {
	return w.Sync(source, dest)
}