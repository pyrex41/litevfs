// Package distcore sqlite_rsync integration - Protocol implementation for sqlite3_rsync
package distcore

import (
	"fmt"
	"net"
	"sync"
	"time"
)

/*
#include "../../src/distcore.h"
#include <stdlib.h>
#include <string.h>
*/
import "C"

// SqliteRsyncMessage represents the sqlite3_rsync protocol messages
type SqliteRsyncMessage struct {
	Type    uint8
	Payload []byte
}

// Message type constants matching the OCaml implementation
const (
	OriginBeginType  = 0x41
	OriginEndType    = 0x42
	OriginErrorType  = 0x43
	OriginPageType   = 0x44
	OriginTxnType    = 0x45
	OriginMsgType    = 0x46
	
	ReplicaBeginType = 0x61
	ReplicaErrorType = 0x62
	ReplicaEndType   = 0x63
	ReplicaHashType  = 0x64
	ReplicaReadyType = 0x65
	ReplicaMsgType   = 0x66
	
	// Security limits
	MaxMessageSize   = 16 * 1024 * 1024 // 16MB maximum message size
	MaxPageSize      = 64 * 1024        // 64KB maximum page size
	MaxPageCount     = 1000000          // 1M maximum pages
	
	// Timeout limits
	ConnectionTimeout = 30 * time.Second  // 30s connection timeout
	ReadTimeout      = 10 * time.Second   // 10s read timeout
	WriteTimeout     = 10 * time.Second   // 10s write timeout
)

// SyncPage represents a page that needs syncing (from OCaml VFS)
type SyncPage struct {
	PageNum int
	Hash    string
	Data    []byte
}



// SqliteRsyncSession manages a sync session using the sqlite3_rsync protocol
type SqliteRsyncSession struct {
	conn       net.Conn
	dbPath     string
	isOrigin   bool
	protocol   int
	pageSize   int
	pageCount  int
	
	// Enhanced with OCaml performance engine
	engine     *OCamlEngine
	
	// Protocol state
	mu         sync.RWMutex
	state      string
	errChan    chan error
	closeOnce  sync.Once
}

// NewOriginRsyncSession creates a new origin (source) sync session
func NewOriginRsyncSession(conn net.Conn, dbPath string, pageCount int) (*SqliteRsyncSession, error) {
	engine, err := GetEngine()
	if err != nil {
		return nil, fmt.Errorf("failed to get OCaml engine: %w", err)
	}
	
	return &SqliteRsyncSession{
		conn:      conn,
		dbPath:    dbPath,
		isOrigin:  true,
		protocol:  1, // Protocol version 1
		pageSize:  4096, // Standard SQLite page size
		pageCount: pageCount,
		engine:    engine,
		state:     "init",
		errChan:   make(chan error, 1),
	}, nil
}

// NewReplicaRsyncSession creates a new replica (destination) sync session
func NewReplicaRsyncSession(conn net.Conn, dbPath string) (*SqliteRsyncSession, error) {
	engine, err := GetEngine()
	if err != nil {
		return nil, fmt.Errorf("failed to get OCaml engine: %w", err)
	}
	
	return &SqliteRsyncSession{
		conn:     conn,
		dbPath:   dbPath,
		isOrigin: false,
		protocol: 1,
		pageSize: 4096,
		engine:   engine,
		state:    "wait_begin",
		errChan:  make(chan error, 1),
	}, nil
}

// RunOriginSync executes the origin side of the sync protocol
func (s *SqliteRsyncSession) RunOriginSync() error {
	defer s.Close()
	
	// Send OriginBegin
	if err := s.sendOriginBegin(); err != nil {
		return fmt.Errorf("failed to send OriginBegin: %w", err)
	}
	
	// Wait for ReplicaBegin
	msg, err := s.receiveMessage()
	if err != nil {
		return fmt.Errorf("failed to receive ReplicaBegin: %w", err)
	}
	
	if msg.Type != ReplicaBeginType {
		return fmt.Errorf("expected ReplicaBegin, got type %d", msg.Type)
	}
	
	// Collect replica hashes
	replicaHashes := make(map[int]string)
	pageNum := 0
	
	for {
		msg, err := s.receiveMessage()
		if err != nil {
			return fmt.Errorf("failed to receive message: %w", err)
		}
		
		switch msg.Type {
		case ReplicaHashType:
			if len(msg.Payload) >= 20 {
				hash := string(msg.Payload[:20])
				replicaHashes[pageNum] = hash
				pageNum++
			}
			
		case ReplicaReadyType:
			// All hashes received, compute diff and send pages
			return s.sendDiffPages(replicaHashes)
			
		case ReplicaErrorType:
			return fmt.Errorf("replica error: %s", string(msg.Payload[4:]))
			
		default:
			return fmt.Errorf("unexpected message type: %d", msg.Type)
		}
	}
}

// RunReplicaSync executes the replica side of the sync protocol
func (s *SqliteRsyncSession) RunReplicaSync() error {
	defer s.Close()
	
	// Wait for OriginBegin
	msg, err := s.receiveMessage()
	if err != nil {
		return fmt.Errorf("failed to receive OriginBegin: %w", err)
	}
	
	if msg.Type != OriginBeginType {
		return fmt.Errorf("expected OriginBegin, got type %d", msg.Type)
	}
	
	// Parse OriginBegin
	if len(msg.Payload) < 6 {
		return fmt.Errorf("invalid OriginBegin payload")
	}
	
	protocol := int(msg.Payload[0])
	pageSizePow2 := int(msg.Payload[1])
	pageSize := 1 << pageSizePow2
	pageCount := int(msg.Payload[2])<<24 | int(msg.Payload[3])<<16 | int(msg.Payload[4])<<8 | int(msg.Payload[5])
	
	// Security: Validate parameters to prevent resource exhaustion
	if protocol < 1 || protocol > 10 {
		return fmt.Errorf("unsupported protocol version: %d", protocol)
	}
	if pageSize < 512 || pageSize > MaxPageSize {
		return fmt.Errorf("invalid page size: %d (must be 512-%d)", pageSize, MaxPageSize)
	}
	if pageCount < 0 || pageCount > MaxPageCount {
		return fmt.Errorf("invalid page count: %d (max %d)", pageCount, MaxPageCount)
	}
	
	s.protocol = protocol
	s.pageSize = pageSize
	s.pageCount = pageCount
	
	// Send ReplicaBegin
	if err := s.sendReplicaBegin(); err != nil {
		return fmt.Errorf("failed to send ReplicaBegin: %w", err)
	}
	
	// Send our page hashes (delegate to OCaml VFS)
	if err := s.sendPageHashes(); err != nil {
		return fmt.Errorf("failed to send page hashes: %w", err)
	}
	
	// Send ReplicaReady
	if err := s.sendReplicaReady(); err != nil {
		return fmt.Errorf("failed to send ReplicaReady: %w", err)
	}
	
	// Receive and apply pages
	return s.receiveAndApplyPages()
}

// sendOriginBegin sends the initial origin message
func (s *SqliteRsyncSession) sendOriginBegin() error {
	payload := make([]byte, 6)
	payload[0] = byte(s.protocol)
	
	// Calculate page size power of 2
	pageSizePow2 := 0
	for size := s.pageSize; size > 1; size >>= 1 {
		pageSizePow2++
	}
	payload[1] = byte(pageSizePow2)
	
	// Page count (4 bytes, big endian)
	payload[2] = byte(s.pageCount >> 24)
	payload[3] = byte(s.pageCount >> 16)
	payload[4] = byte(s.pageCount >> 8)
	payload[5] = byte(s.pageCount)
	
	return s.sendMessage(SqliteRsyncMessage{
		Type:    OriginBeginType,
		Payload: payload,
	})
}

// sendDiffPages computes and sends pages that differ (delegates to OCaml VFS)
func (s *SqliteRsyncSession) sendDiffPages(replicaHashes map[int]string) error {
	// Get sync pages from OCaml VFS
	pageNums, pageHashes, pageData, err := s.engine.GetSyncPages(s.dbPath, s.pageCount)
	if err != nil {
		return fmt.Errorf("failed to get sync pages: %w", err)
	}
	
	// Compute diff against replica hashes
	diffPages := make([]int, 0)
	
	for i, pageNum := range pageNums {
		localHash := pageHashes[i]
		replicaHash, exists := replicaHashes[pageNum]
		
		// Send page if replica doesn't have it or hashes differ
		if !exists || localHash != replicaHash {
			diffPages = append(diffPages, i)
		}
	}
	
	// Send differing pages
	for _, idx := range diffPages {
		page := SyncPage{
			PageNum: pageNums[idx],
			Hash:    pageHashes[idx],
			Data:    pageData[idx],
		}
		
		if err := s.sendOriginPage(page); err != nil {
			return fmt.Errorf("failed to send page %d: %w", page.PageNum, err)
		}
	}
	
	// Send transaction completion
	return s.sendOriginTxn(len(diffPages))
}

// sendOriginPage sends a single page
func (s *SqliteRsyncSession) sendOriginPage(page SyncPage) error {
	payload := make([]byte, 4+len(page.Data))
	payload[0] = byte(page.PageNum >> 24)
	payload[1] = byte(page.PageNum >> 16)
	payload[2] = byte(page.PageNum >> 8)
	payload[3] = byte(page.PageNum)
	copy(payload[4:], page.Data)
	
	return s.sendMessage(SqliteRsyncMessage{
		Type:    OriginPageType,
		Payload: payload,
	})
}

// sendOriginTxn sends transaction completion
func (s *SqliteRsyncSession) sendOriginTxn(pageCount int) error {
	payload := make([]byte, 4)
	payload[0] = byte(pageCount >> 24)
	payload[1] = byte(pageCount >> 16)
	payload[2] = byte(pageCount >> 8)
	payload[3] = byte(pageCount)
	
	return s.sendMessage(SqliteRsyncMessage{
		Type:    OriginTxnType,
		Payload: payload,
	})
}

// sendReplicaBegin sends replica begin message
func (s *SqliteRsyncSession) sendReplicaBegin() error {
	payload := make([]byte, 6)
	payload[0] = byte(s.protocol)
	
	return s.sendMessage(SqliteRsyncMessage{
		Type:    ReplicaBeginType,
		Payload: payload,
	})
}

// sendPageHashes sends page hashes to origin (delegates to OCaml VFS)
func (s *SqliteRsyncSession) sendPageHashes() error {
	// Get page hashes from OCaml VFS
	pageNums, pageHashes, _, err := s.engine.GetSyncPages(s.dbPath, s.pageCount)
	if err != nil {
		return fmt.Errorf("failed to get page hashes: %w", err)
	}
	
	// Send hash for each page
	for i, pageNum := range pageNums {
		if pageNum >= s.pageCount {
			break // Don't exceed declared page count
		}
		
		hash := pageHashes[i]
		if len(hash) > 20 {
			hash = hash[:20] // Truncate to 20 bytes (SHA-1 size)
		}
		
		// Pad hash to 20 bytes if needed
		hashBytes := make([]byte, 20)
		copy(hashBytes, hash)
		
		if err := s.sendMessage(SqliteRsyncMessage{
			Type:    ReplicaHashType,
			Payload: hashBytes,
		}); err != nil {
			return fmt.Errorf("failed to send hash for page %d: %w", pageNum, err)
		}
	}
	
	return nil
}

// sendReplicaReady sends ready message
func (s *SqliteRsyncSession) sendReplicaReady() error {
	return s.sendMessage(SqliteRsyncMessage{
		Type:    ReplicaReadyType,
		Payload: []byte{},
	})
}

// receiveAndApplyPages receives and applies pages from origin
func (s *SqliteRsyncSession) receiveAndApplyPages() error {
	for {
		msg, err := s.receiveMessage()
		if err != nil {
			return fmt.Errorf("failed to receive message: %w", err)
		}
		
		switch msg.Type {
		case OriginPageType:
			if err := s.applyPage(msg.Payload); err != nil {
				return fmt.Errorf("failed to apply page: %w", err)
			}
			
		case OriginTxnType:
			// Transaction complete
			return nil
			
		case OriginErrorType:
			return fmt.Errorf("origin error: %s", string(msg.Payload[4:]))
			
		default:
			return fmt.Errorf("unexpected message type: %d", msg.Type)
		}
	}
}

// applyPage applies a received page (delegates to OCaml VFS)
func (s *SqliteRsyncSession) applyPage(payload []byte) error {
	if len(payload) < 4 {
		return fmt.Errorf("invalid page payload")
	}
	
	pageNum := int(payload[0])<<24 | int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	pageData := payload[4:]
	
	// Security: Validate page data size
	if len(pageData) > MaxPageSize {
		return fmt.Errorf("page data too large: %d bytes (max %d)", len(pageData), MaxPageSize)
	}
	
	// Apply page via OCaml VFS
	if err := s.engine.ApplyPage(s.dbPath, pageNum, pageData); err != nil {
		return fmt.Errorf("failed to apply page %d: %w", pageNum, err)
	}
	
	return nil
}

// sendMessage sends a message over the connection
func (s *SqliteRsyncSession) sendMessage(msg SqliteRsyncMessage) error {
	// Set write timeout
	if err := s.conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return fmt.Errorf("failed to set write deadline: %w", err)
	}
	
	// Send type
	if _, err := s.conn.Write([]byte{msg.Type}); err != nil {
		return err
	}
	
	// Send payload length (4 bytes, big endian)
	length := len(msg.Payload)
	lengthBytes := []byte{
		byte(length >> 24),
		byte(length >> 16),
		byte(length >> 8),
		byte(length),
	}
	if _, err := s.conn.Write(lengthBytes); err != nil {
		return err
	}
	
	// Send payload
	if len(msg.Payload) > 0 {
		if _, err := s.conn.Write(msg.Payload); err != nil {
			return err
		}
	}
	
	return nil
}

// receiveMessage receives a message from the connection
func (s *SqliteRsyncSession) receiveMessage() (SqliteRsyncMessage, error) {
	// Set read timeout
	if err := s.conn.SetReadDeadline(time.Now().Add(ReadTimeout)); err != nil {
		return SqliteRsyncMessage{}, fmt.Errorf("failed to set read deadline: %w", err)
	}
	
	// Read type
	typeBytes := make([]byte, 1)
	if _, err := s.conn.Read(typeBytes); err != nil {
		return SqliteRsyncMessage{}, err
	}
	
	// Read payload length
	lengthBytes := make([]byte, 4)
	if _, err := s.conn.Read(lengthBytes); err != nil {
		return SqliteRsyncMessage{}, err
	}
	
	length := int(lengthBytes[0])<<24 | int(lengthBytes[1])<<16 | int(lengthBytes[2])<<8 | int(lengthBytes[3])
	
	// Security: Check message size limit to prevent DoS attacks
	if length < 0 {
		return SqliteRsyncMessage{}, fmt.Errorf("invalid message length: %d", length)
	}
	if length > MaxMessageSize {
		return SqliteRsyncMessage{}, fmt.Errorf("message too large: %d bytes (max %d)", length, MaxMessageSize)
	}
	
	// Read payload
	var payload []byte
	if length > 0 {
		payload = make([]byte, length)
		if _, err := s.conn.Read(payload); err != nil {
			return SqliteRsyncMessage{}, err
		}
	}
	
	return SqliteRsyncMessage{
		Type:    typeBytes[0],
		Payload: payload,
	}, nil
}

// Close closes the session
func (s *SqliteRsyncSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.conn != nil {
			err = s.conn.Close()
		}
		close(s.errChan)
	})
	return err
}

// Wait waits for the session to complete
func (s *SqliteRsyncSession) Wait() error {
	select {
	case err := <-s.errChan:
		return err
	}
}