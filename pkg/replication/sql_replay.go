package replication

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// SQLStatement represents a single SQL operation for replay
type SQLStatement struct {
	SQL       string                 `json:"sql"`
	Params    []interface{}          `json:"params,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
	TxnID     string                 `json:"txn_id,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// SQLReplayBatch represents a batch of SQL statements for replication
type SQLReplayBatch struct {
	Database   string         `json:"database"`
	Statements []SQLStatement `json:"statements"`
	BatchID    string         `json:"batch_id"`
	CreatedAt  time.Time      `json:"created_at"`
	FenceToken int64          `json:"fence_token"`
}

// SQLReplayClient handles sending SQL replay batches to target nodes
type SQLReplayClient struct {
	httpClient *http.Client
	timeout    time.Duration
}

// NewSQLReplayClient creates a new SQL replay client
func NewSQLReplayClient() *SQLReplayClient {
	return &SQLReplayClient{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		timeout: 10 * time.Second,
	}
}

// ExtractSQLFromChanges converts Change objects to SQL statements
func (c *SQLReplayClient) ExtractSQLFromChanges(changes []Change) ([]SQLStatement, error) {
	var statements []SQLStatement
	
	for _, change := range changes {
		// For now, we'll extract SQL from the change data
		// In a real implementation, this would parse the actual SQLite WAL entries
		// or use captured SQL from sqlite3_update_hook
		
		sql, params, err := c.parseChangeToSQL(change)
		if err != nil {
			log.Printf("[SQL Replay] Failed to parse change to SQL: %v", err)
			continue // Skip unparseable changes
		}
		
		statement := SQLStatement{
			SQL:       sql,
			Params:    params,
			Timestamp: change.Timestamp,
			Metadata: map[string]interface{}{
				"change_type": change.Type,
				"size":        change.Size,
			},
		}
		
		statements = append(statements, statement)
	}
	
	return statements, nil
}

// parseChangeToSQL converts a Change to SQL statement
// This is a simplified implementation - in practice, this would parse WAL entries
func (c *SQLReplayClient) parseChangeToSQL(change Change) (string, []interface{}, error) {
	// For demonstration, we'll create synthetic SQL based on change type
	// In a real implementation, this would parse actual WAL data or captured SQL
	
	switch change.Type {
	case "write":
		// Simulate INSERT statement
		return "INSERT INTO _replay_log (timestamp, data) VALUES (?, ?)", 
			   []interface{}{change.Timestamp.Unix(), change.Data}, nil
	
	case "wal_write":
		// Simulate UPDATE statement  
		return "UPDATE _replay_table SET data = ? WHERE id = ?",
			   []interface{}{change.Data, change.Timestamp.Unix()}, nil
	
	case "truncate":
		// Simulate DELETE statement
		return "DELETE FROM _replay_table WHERE created_at < ?",
			   []interface{}{change.Timestamp}, nil
	
	default:
		return "", nil, fmt.Errorf("unknown change type: %s", change.Type)
	}
}

// SendSQLBatch sends a batch of SQL statements to a target node
func (c *SQLReplayClient) SendSQLBatch(ctx context.Context, target string, batch SQLReplayBatch) error {
	// Serialize batch to JSON
	data, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("failed to marshal SQL batch: %w", err)
	}
	
	// Send to target node
	url := fmt.Sprintf("http://%s/sql-replay", target)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "DistSQLite-SQL-Replay/1.0")
	
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send SQL batch: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("SQL replay failed: %s - %s", resp.Status, string(body))
	}
	
	log.Printf("[SQL Replay] Successfully sent %d statements to %s", 
		len(batch.Statements), target)
	
	return nil
}

// SQLReplayServer handles receiving and applying SQL replay batches
type SQLReplayServer struct {
	dbPath     string
	db         *sql.DB
	httpServer *http.Server
}

// NewSQLReplayServer creates a new SQL replay server
func NewSQLReplayServer(dbPath string, port int) *SQLReplayServer {
	mux := http.NewServeMux()
	server := &SQLReplayServer{
		dbPath: dbPath,
		httpServer: &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: mux,
		},
	}
	
	mux.HandleFunc("/sql-replay", server.handleSQLReplay)
	mux.HandleFunc("/health", server.handleHealth)
	
	return server
}

// Start starts the SQL replay server
func (s *SQLReplayServer) Start() error {
	// Open database connection
	db, err := sql.Open("sqlite", s.dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	s.db = db
	
	// Start HTTP server
	go func() {
		log.Printf("[SQL Replay Server] Starting on %s", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[SQL Replay Server] Error: %v", err)
		}
	}()
	
	return nil
}

// Stop stops the SQL replay server
func (s *SQLReplayServer) Stop() error {
	if s.db != nil {
		s.db.Close()
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	return s.httpServer.Shutdown(ctx)
}

// handleSQLReplay processes incoming SQL replay batches
func (s *SQLReplayServer) handleSQLReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPOST {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Parse batch
	var batch SQLReplayBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	
	// Apply SQL statements in transaction
	if err := s.applySQLBatch(batch); err != nil {
		log.Printf("[SQL Replay Server] Failed to apply batch: %v", err)
		http.Error(w, fmt.Sprintf("Failed to apply SQL batch: %v", err), http.StatusInternalServerError)
		return
	}
	
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status": "success", "statements_applied": %d}`, len(batch.Statements))
	
	log.Printf("[SQL Replay Server] Applied %d statements from batch %s", 
		len(batch.Statements), batch.BatchID)
}

// applySQLBatch applies a batch of SQL statements in a transaction
func (s *SQLReplayServer) applySQLBatch(batch SQLReplayBatch) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() // Will be no-op if committed
	
	// Apply each statement
	for i, stmt := range batch.Statements {
		if err := s.applySQLStatement(tx, stmt); err != nil {
			return fmt.Errorf("failed to apply statement %d: %w", i, err)
		}
	}
	
	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	
	return nil
}

// applySQLStatement applies a single SQL statement
func (s *SQLReplayServer) applySQLStatement(tx *sql.Tx, stmt SQLStatement) error {
	// Basic SQL injection protection - check for dangerous patterns
	sqlLower := strings.ToLower(strings.TrimSpace(stmt.SQL))
	if strings.Contains(sqlLower, "drop ") || 
	   strings.Contains(sqlLower, "create ") ||
	   strings.Contains(sqlLower, "alter ") {
		return fmt.Errorf("DDL statements not allowed in SQL replay: %s", stmt.SQL)
	}
	
	// Execute the statement
	if len(stmt.Params) > 0 {
		_, err := tx.Exec(stmt.SQL, stmt.Params...)
		return err
	} else {
		_, err := tx.Exec(stmt.SQL)
		return err
	}
}

// handleHealth provides health check endpoint
func (s *SQLReplayServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Check database connection
	if err := s.db.Ping(); err != nil {
		http.Error(w, "Database unhealthy", http.StatusServiceUnavailable)
		return
	}
	
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status": "healthy", "timestamp": "%s"}`, time.Now().Format(time.RFC3339))
}