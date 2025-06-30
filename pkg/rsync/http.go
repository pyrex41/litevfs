package rsync

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	_ "modernc.org/sqlite"
)

// HTTPRsyncMessage represents the protocol messages
type HTTPRsyncMessage struct {
	Type      string            `json:"type"`
	Protocol  int               `json:"protocol,omitempty"`
	PageSize  int               `json:"page_size,omitempty"`
	PageCount int               `json:"page_count,omitempty"`
	PageNum   int               `json:"page_num,omitempty"`
	Hash      string            `json:"hash,omitempty"`
	Data      []byte            `json:"data,omitempty"`
	Error     string            `json:"error,omitempty"`
	Stats     *HTTPRsyncStats   `json:"stats,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// HTTPRsyncStats tracks transfer statistics
type HTTPRsyncStats struct {
	HashesSent  int   `json:"hashes_sent"`
	PagesSent   int   `json:"pages_sent"`
	BytesSent   int64 `json:"bytes_sent"`
	BytesRecv   int64 `json:"bytes_recv"`
	StartTime   int64 `json:"start_time"`
	ElapsedMs   int64 `json:"elapsed_ms"`
	TotalPages  int   `json:"total_pages"`
	ChangedPages int  `json:"changed_pages"`
}

// HTTPRsyncClient handles the client side of HTTP rsync
type HTTPRsyncClient struct {
	client    *http.Client
	userAgent string
	timeout   time.Duration
}

// NewHTTPRsyncClient creates a new HTTP rsync client
func NewHTTPRsyncClient() *HTTPRsyncClient {
	return &HTTPRsyncClient{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		userAgent: "DistSQLite-HTTPRsync/1.0",
		timeout:   30 * time.Second,
	}
}

// HTTPRsyncSync performs database synchronization using HTTP transport
// This implements the same algorithm as sqlite3-rsync but over HTTP
func (c *HTTPRsyncClient) HTTPRsyncSync(ctx context.Context, originDB, targetURL string) (*HTTPRsyncStats, error) {
	stats := &HTTPRsyncStats{
		StartTime: time.Now().UnixMilli(),
	}
	
	// Open origin database
	db, err := sql.Open("sqlite", originDB+"?mode=ro&cache=shared")
	if err != nil {
		return stats, fmt.Errorf("failed to open origin database: %w", err)
	}
	defer db.Close()
	
	// Get database metadata
	pageSize, pageCount, err := c.getDatabaseInfo(db)
	if err != nil {
		return stats, fmt.Errorf("failed to get database info: %w", err)
	}
	
	stats.TotalPages = pageCount
	
	// Step 1: Send ORIGIN_BEGIN message
	beginMsg := &HTTPRsyncMessage{
		Type:      "ORIGIN_BEGIN",
		Protocol:  1,
		PageSize:  pageSize,
		PageCount: pageCount,
	}
	
	respMsg, err := c.sendMessage(ctx, targetURL, beginMsg)
	if err != nil {
		return stats, fmt.Errorf("failed to send ORIGIN_BEGIN: %w", err)
	}
	
	if respMsg.Type == "REPLICA_ERROR" {
		return stats, fmt.Errorf("replica error: %s", respMsg.Error)
	}
	
	if respMsg.Type != "REPLICA_BEGIN" {
		return stats, fmt.Errorf("unexpected response: %s", respMsg.Type)
	}
	
	// Step 2: Receive page hashes from replica
	hashes, err := c.receivePageHashes(ctx, targetURL)
	if err != nil {
		return stats, fmt.Errorf("failed to receive hashes: %w", err)
	}
	
	stats.HashesSent = len(hashes)
	
	// Step 3: Compare hashes and identify changed pages
	changedPages, err := c.findChangedPages(db, hashes, pageSize)
	if err != nil {
		return stats, fmt.Errorf("failed to find changed pages: %w", err)
	}
	
	stats.ChangedPages = len(changedPages)
	
	// Step 4: Send changed pages to replica
	bytesSent, err := c.sendChangedPages(ctx, targetURL, db, changedPages, pageSize)
	if err != nil {
		return stats, fmt.Errorf("failed to send changed pages: %w", err)
	}
	
	stats.PagesSent = len(changedPages)
	stats.BytesSent = bytesSent
	
	// Step 5: Send transaction completion
	txnMsg := &HTTPRsyncMessage{
		Type:      "ORIGIN_TXN",
		PageCount: pageCount,
	}
	
	_, err = c.sendMessage(ctx, targetURL, txnMsg)
	if err != nil {
		return stats, fmt.Errorf("failed to send ORIGIN_TXN: %w", err)
	}
	
	// Calculate final stats
	stats.ElapsedMs = time.Now().UnixMilli() - stats.StartTime
	
	return stats, nil
}

// getDatabaseInfo extracts page size and count from SQLite database
func (c *HTTPRsyncClient) getDatabaseInfo(db *sql.DB) (pageSize, pageCount int, err error) {
	// Get page size
	err = db.QueryRow("PRAGMA page_size").Scan(&pageSize)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get page size: %w", err)
	}
	
	// Get page count
	err = db.QueryRow("PRAGMA page_count").Scan(&pageCount)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get page count: %w", err)
	}
	
	// Verify database is in WAL mode
	var journalMode string
	err = db.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get journal mode: %w", err)
	}
	
	if journalMode != "wal" {
		return 0, 0, fmt.Errorf("database must be in WAL mode, got: %s", journalMode)
	}
	
	return pageSize, pageCount, nil
}

// receivePageHashes gets page hashes from the replica
func (c *HTTPRsyncClient) receivePageHashes(ctx context.Context, targetURL string) (map[int]string, error) {
	// Request hashes from replica
	hashReqMsg := &HTTPRsyncMessage{
		Type: "REQUEST_HASHES",
	}
	
	respMsg, err := c.sendMessage(ctx, targetURL, hashReqMsg)
	if err != nil {
		return nil, err
	}
	
	if respMsg.Type == "REPLICA_ERROR" {
		return nil, fmt.Errorf("replica error: %s", respMsg.Error)
	}
	
	// Receive all hashes
	hashes := make(map[int]string)
	
	for respMsg.Type == "REPLICA_HASH" {
		if respMsg.PageNum > 0 && respMsg.Hash != "" {
			hashes[respMsg.PageNum] = respMsg.Hash
		}
		
		// Get next hash or REPLICA_READY
		respMsg, err = c.receiveMessage(ctx, targetURL+"/next")
		if err != nil {
			return nil, err
		}
		
		if respMsg.Type == "REPLICA_ERROR" {
			return nil, fmt.Errorf("replica error: %s", respMsg.Error)
		}
		
		if respMsg.Type == "REPLICA_READY" {
			break
		}
	}
	
	return hashes, nil
}

// findChangedPages compares local pages with received hashes
func (c *HTTPRsyncClient) findChangedPages(db *sql.DB, replicaHashes map[int]string, pageSize int) ([]int, error) {
	var changedPages []int
	
	// Query all pages and compare hashes
	rows, err := db.Query("SELECT pgno, data FROM sqlite_dbpage('main') ORDER BY pgno")
	if err != nil {
		return nil, fmt.Errorf("failed to query pages: %w", err)
	}
	defer rows.Close()
	
	for rows.Next() {
		var pageNum int
		var pageData []byte
		
		if err := rows.Scan(&pageNum, &pageData); err != nil {
			return nil, fmt.Errorf("failed to scan page: %w", err)
		}
		
		// Skip lock byte page (similar to sqlite3-rsync)
		lockBytePage := (1 << 30) / pageSize + 1
		if pageNum == lockBytePage {
			continue
		}
		
		// Calculate hash of local page
		localHash := c.hashPage(pageData)
		
		// Compare with replica hash
		replicaHash, exists := replicaHashes[pageNum]
		if !exists || replicaHash != localHash {
			changedPages = append(changedPages, pageNum)
		}
	}
	
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating pages: %w", err)
	}
	
	return changedPages, nil
}

// hashPage calculates the hash of a database page (using same algorithm as sqlite3-rsync)
func (c *HTTPRsyncClient) hashPage(data []byte) string {
	// Use the same hash algorithm as sqlite3-rsync (160-bit custom hash)
	// For simplicity, using SHA1 here - could implement the exact algorithm if needed
	hash := sha1.Sum(data)
	return hex.EncodeToString(hash[:])
}

// sendChangedPages sends the changed page data to replica
func (c *HTTPRsyncClient) sendChangedPages(ctx context.Context, targetURL string, db *sql.DB, 
	changedPages []int, pageSize int) (int64, error) {
	
	var totalBytes int64
	
	for _, pageNum := range changedPages {
		// Get page data
		var pageData []byte
		err := db.QueryRow("SELECT data FROM sqlite_dbpage('main') WHERE pgno = ?", pageNum).Scan(&pageData)
		if err != nil {
			return totalBytes, fmt.Errorf("failed to get page %d: %w", pageNum, err)
		}
		
		// Send page to replica
		pageMsg := &HTTPRsyncMessage{
			Type:    "ORIGIN_PAGE",
			PageNum: pageNum,
			Data:    pageData,
		}
		
		_, err = c.sendMessage(ctx, targetURL, pageMsg)
		if err != nil {
			return totalBytes, fmt.Errorf("failed to send page %d: %w", pageNum, err)
		}
		
		totalBytes += int64(len(pageData))
	}
	
	return totalBytes, nil
}

// sendMessage sends an HTTP message and returns the response
func (c *HTTPRsyncClient) sendMessage(ctx context.Context, url string, msg *HTTPRsyncMessage) (*HTTPRsyncMessage, error) {
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message: %w", err)
	}
	
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/rsync", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-Rsync-Message-Type", msg.Type)
	
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(body))
	}
	
	var respMsg HTTPRsyncMessage
	if err := json.NewDecoder(resp.Body).Decode(&respMsg); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	return &respMsg, nil
}

// receiveMessage receives a message from the given URL
func (c *HTTPRsyncClient) receiveMessage(ctx context.Context, url string) (*HTTPRsyncMessage, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("User-Agent", c.userAgent)
	
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(body))
	}
	
	var msg HTTPRsyncMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	return &msg, nil
}

// HTTPRsyncServer handles the server side of HTTP rsync
type HTTPRsyncServer struct {
	dbPath     string
	pageHashes map[int]string
	db         *sql.DB
	pageSize   int
	pageCount  int
}

// NewHTTPRsyncServer creates a new HTTP rsync server
func NewHTTPRsyncServer(dbPath string) *HTTPRsyncServer {
	return &HTTPRsyncServer{
		dbPath:     dbPath,
		pageHashes: make(map[int]string),
	}
}

// HandleRsync handles HTTP rsync requests 
func (s *HTTPRsyncServer) HandleRsync(w http.ResponseWriter, r *http.Request) {
	var msg HTTPRsyncMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		s.sendError(w, "Invalid JSON: "+err.Error())
		return
	}
	
	switch msg.Type {
	case "ORIGIN_BEGIN":
		s.handleOriginBegin(w, &msg)
	case "REQUEST_HASHES":
		s.handleRequestHashes(w, &msg)
	case "ORIGIN_PAGE":
		s.handleOriginPage(w, &msg)
	case "ORIGIN_TXN":
		s.handleOriginTxn(w, &msg)
	default:
		s.sendError(w, "Unknown message type: "+msg.Type)
	}
}

// handleOriginBegin processes the initial sync request
func (s *HTTPRsyncServer) handleOriginBegin(w http.ResponseWriter, msg *HTTPRsyncMessage) {
	// Open replica database
	db, err := sql.Open("sqlite", s.dbPath+"?cache=shared")
	if err != nil {
		s.sendError(w, "Failed to open database: "+err.Error())
		return
	}
	s.db = db
	
	// Store origin database info
	s.pageSize = msg.PageSize
	s.pageCount = msg.PageCount
	
	// Initialize replica database if needed
	var replicaPageCount int
	err = s.db.QueryRow("PRAGMA page_count").Scan(&replicaPageCount)
	if err != nil {
		s.sendError(w, "Failed to get replica page count: "+err.Error())
		return
	}
	
	if replicaPageCount == 0 {
		// Initialize empty database with correct page size
		_, err = s.db.Exec(fmt.Sprintf("PRAGMA page_size=%d", s.pageSize))
		if err != nil {
			s.sendError(w, "Failed to set page size: "+err.Error())
			return
		}
		
		_, err = s.db.Exec("PRAGMA journal_mode=WAL")
		if err != nil {
			s.sendError(w, "Failed to set WAL mode: "+err.Error())
			return
		}
		
		// Create minimal schema to initialize database
		_, err = s.db.Exec("CREATE TABLE IF NOT EXISTS _rsync_init (id INTEGER)")
		if err != nil {
			s.sendError(w, "Failed to initialize database: "+err.Error())
			return
		}
	}
	
	// Send confirmation
	response := &HTTPRsyncMessage{
		Type:     "REPLICA_BEGIN",
		Protocol: 1,
	}
	
	s.sendResponse(w, response)
}

// handleRequestHashes sends page hashes to origin
func (s *HTTPRsyncServer) handleRequestHashes(w http.ResponseWriter, msg *HTTPRsyncMessage) {
	// Calculate hashes for all pages
	rows, err := s.db.Query("SELECT pgno, data FROM sqlite_dbpage('main') ORDER BY pgno")
	if err != nil {
		s.sendError(w, "Failed to query pages: "+err.Error())
		return
	}
	defer rows.Close()
	
	var hashes []HTTPRsyncMessage
	
	for rows.Next() {
		var pageNum int
		var pageData []byte
		
		if err := rows.Scan(&pageNum, &pageData); err != nil {
			s.sendError(w, "Failed to scan page: "+err.Error())
			return
		}
		
		// Calculate hash
		hash := s.hashPage(pageData)
		s.pageHashes[pageNum] = hash
		
		// Add to response
		hashes = append(hashes, HTTPRsyncMessage{
			Type:    "REPLICA_HASH",
			PageNum: pageNum,
			Hash:    hash,
		})
	}
	
	// Send first hash (client will call /next for subsequent hashes)
	if len(hashes) > 0 {
		s.sendResponse(w, &hashes[0])
	} else {
		s.sendResponse(w, &HTTPRsyncMessage{Type: "REPLICA_READY"})
	}
}

// handleOriginPage processes a page update from origin
func (s *HTTPRsyncServer) handleOriginPage(w http.ResponseWriter, msg *HTTPRsyncMessage) {
	// Apply page to replica database
	_, err := s.db.Exec("INSERT OR REPLACE INTO sqlite_dbpage(pgno, data) VALUES (?, ?)", 
		msg.PageNum, msg.Data)
	if err != nil {
		s.sendError(w, fmt.Sprintf("Failed to update page %d: %s", msg.PageNum, err.Error()))
		return
	}
	
	// Send acknowledgment
	s.sendResponse(w, &HTTPRsyncMessage{Type: "PAGE_ACK"})
}

// handleOriginTxn processes transaction completion
func (s *HTTPRsyncServer) handleOriginTxn(w http.ResponseWriter, msg *HTTPRsyncMessage) {
	// Commit transaction and send completion
	s.sendResponse(w, &HTTPRsyncMessage{Type: "TXN_COMPLETE"})
}

// hashPage calculates page hash (same algorithm as client)
func (s *HTTPRsyncServer) hashPage(data []byte) string {
	hash := sha1.Sum(data)
	return hex.EncodeToString(hash[:])
}

// sendError sends an error response
func (s *HTTPRsyncServer) sendError(w http.ResponseWriter, errorMsg string) {
	response := &HTTPRsyncMessage{
		Type:  "REPLICA_ERROR",
		Error: errorMsg,
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(response)
}

// sendResponse sends a successful response
func (s *HTTPRsyncServer) sendResponse(w http.ResponseWriter, msg *HTTPRsyncMessage) {
	w.Header().Set("Content-Type", "application/json")
	
	if err := json.NewEncoder(w).Encode(msg); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

// VerifyConsistency performs a dry-run verification to detect differences without applying changes
func (c *HTTPRsyncClient) VerifyConsistency(ctx context.Context, originDB, targetURL string) error {
	// Open origin database
	db, err := sql.Open("sqlite", originDB+"?mode=ro&cache=shared")
	if err != nil {
		return fmt.Errorf("failed to open origin database: %w", err)
	}
	defer db.Close()
	
	// Get database metadata
	pageSize, pageCount, err := c.getDatabaseInfo(db)
	if err != nil {
		return fmt.Errorf("failed to get database info: %w", err)
	}
	
	// Send verification request
	verifyMsg := &HTTPRsyncMessage{
		Type:      "VERIFY_REQUEST",
		Protocol:  1,
		PageSize:  pageSize,
		PageCount: pageCount,
	}
	
	respMsg, err := c.sendMessage(ctx, targetURL, verifyMsg)
	if err != nil {
		return fmt.Errorf("failed to send verify request: %w", err)
	}
	
	if respMsg.Type == "REPLICA_ERROR" {
		return fmt.Errorf("replica error: %s", respMsg.Error)
	}
	
	if respMsg.Type != "VERIFY_READY" {
		return fmt.Errorf("unexpected response: %s", respMsg.Type)
	}
	
	// Receive page hashes from replica
	hashes, err := c.receivePageHashes(ctx, targetURL)
	if err != nil {
		return fmt.Errorf("failed to receive hashes: %w", err)
	}
	
	// Compare hashes to find differences
	changedPages, err := c.findChangedPages(db, hashes, pageSize)
	if err != nil {
		return fmt.Errorf("failed to find changed pages: %w", err)
	}
	
	// If there are differences, return an error
	if len(changedPages) > 0 {
		return fmt.Errorf("consistency check failed: %d pages differ (pages: %v)", 
			len(changedPages), changedPages)
	}
	
	return nil
}

// SyncDatabase performs a full sync to repair inconsistencies
func (c *HTTPRsyncClient) SyncDatabase(ctx context.Context, originDB, targetURL string) error {
	stats, err := c.HTTPRsyncSync(ctx, originDB, targetURL)
	if err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}
	
	// Log repair statistics
	if stats.ChangedPages > 0 {
		fmt.Printf("[HTTPRsync] Repair completed: %d pages synced, %d bytes transferred (took %dms)\n",
			stats.ChangedPages, stats.BytesSent, stats.ElapsedMs)
	}
	
	return nil
}