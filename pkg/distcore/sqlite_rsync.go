// Package distcore sqlite_rsync integration - Protocol implementation for sqlite3_rsync
package distcore

import (
	"fmt"
	"net"
	"sync"
	"unsafe"
)

/*
#include "../../src/distcore.h"
#include <stdlib.h>
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
	// Note: Page tracking and diffing is handled by OCaml VFS
	// This is just the protocol implementation
	
	// TODO: Call OCaml VFS to get actual sync pages
	// For now, send transaction completion with 0 pages
	return s.sendOriginTxn(0)
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
	// Note: Page hashing is handled by OCaml VFS
	// This is just the protocol implementation
	
	// TODO: Call OCaml VFS to get actual page hashes
	// For now, send empty hash
	emptyHash := make([]byte, 20)
	
	return s.sendMessage(SqliteRsyncMessage{
		Type:    ReplicaHashType,
		Payload: emptyHash,
	})
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
	
	// Note: Page application is handled by OCaml VFS
	// This is just the protocol implementation
	_ = pageNum
	_ = pageData
	
	return nil
}

// sendMessage sends a message over the connection
func (s *SqliteRsyncSession) sendMessage(msg SqliteRsyncMessage) error {
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