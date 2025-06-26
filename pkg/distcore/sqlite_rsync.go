// Package distcore sqlite_rsync integration - extends the OCaml performance engine with sqlite3_rsync protocol
package distcore

/*
#include "../../src/distcore.h"
#include <stdlib.h>

// Additional functions for sqlite3_rsync
extern void* distcore_create_page_tracker(int page_size);
extern int distcore_vfs_intercept_read(void* file, char* buffer, int64_t offset, int amount);
extern int distcore_vfs_intercept_write(void* file, char* buffer, int64_t offset, int amount);
extern int distcore_get_sync_pages(void* file, int* page_nums, char** page_hashes, char** page_data, int max_pages);
extern int distcore_compute_diff_pages(int* local_pages, char** local_hashes, 
                                       int* replica_pages, char** replica_hashes, 
                                       int local_count, int replica_count,
                                       int* diff_pages, int max_diff);
*/
import "C"
import (
	"fmt"
	"net"
	"sync"
	"unsafe"
)

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

// PageTracker tracks SQLite pages for efficient sync
type PageTracker struct {
	tracker   unsafe.Pointer
	pageSize  int
	mu        sync.RWMutex
}

// NewPageTracker creates a new page tracker
func NewPageTracker(pageSize int) (*PageTracker, error) {
	tracker := C.distcore_create_page_tracker(C.int(pageSize))
	if tracker == nil {
		return nil, fmt.Errorf("failed to create page tracker")
	}
	
	return &PageTracker{
		tracker:  tracker,
		pageSize: pageSize,
	}, nil
}

// VFSFile represents a SQLite file with VFS interception
type VFSFile struct {
	path      string
	tracker   *PageTracker
	baseFile  unsafe.Pointer
	intercept bool
	mu        sync.RWMutex
}

// NewVFSFile creates a new VFS file with interception
func NewVFSFile(path string, pageSize int) (*VFSFile, error) {
	tracker, err := NewPageTracker(pageSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create page tracker: %w", err)
	}
	
	return &VFSFile{
		path:      path,
		tracker:   tracker,
		intercept: true,
	}, nil
}

// Read intercepts SQLite reads to track page access
func (f *VFSFile) Read(buffer []byte, offset int64) (int, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	if !f.intercept {
		// Fall back to normal read
		return 0, fmt.Errorf("VFS interception disabled")
	}
	
	result := C.distcore_vfs_intercept_read(
		f.baseFile,
		(*C.char)(unsafe.Pointer(&buffer[0])),
		C.int64_t(offset),
		C.int(len(buffer)),
	)
	
	return int(result), nil
}

// Write intercepts SQLite writes to track page changes
func (f *VFSFile) Write(buffer []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	
	if !f.intercept {
		return 0, fmt.Errorf("VFS interception disabled")
	}
	
	result := C.distcore_vfs_intercept_write(
		f.baseFile,
		(*C.char)(unsafe.Pointer(&buffer[0])),
		C.int64_t(offset),
		C.int(len(buffer)),
	)
	
	return int(result), nil
}

// SyncPage represents a page that needs syncing
type SyncPage struct {
	PageNum int
	Hash    string
	Data    []byte
}

// GetSyncPages returns pages that need syncing
func (f *VFSFile) GetSyncPages() ([]SyncPage, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	const maxPages = 10000
	pageNums := make([]C.int, maxPages)
	pageHashes := make([]*C.char, maxPages)
	pageData := make([]*C.char, maxPages)
	
	count := C.distcore_get_sync_pages(
		f.baseFile,
		&pageNums[0],
		&pageHashes[0],
		&pageData[0],
		C.int(maxPages),
	)
	
	if count < 0 {
		return nil, fmt.Errorf("failed to get sync pages")
	}
	
	pages := make([]SyncPage, int(count))
	for i := 0; i < int(count); i++ {
		pages[i] = SyncPage{
			PageNum: int(pageNums[i]),
			Hash:    C.GoString(pageHashes[i]),
			Data:    C.GoBytes(unsafe.Pointer(pageData[i]), C.int(f.tracker.pageSize)),
		}
		
		// Free C memory
		C.free(unsafe.Pointer(pageHashes[i]))
		C.free(unsafe.Pointer(pageData[i]))
	}
	
	return pages, nil
}

// ComputeDiffPages determines which pages need to be sent based on replica hashes
func ComputeDiffPages(localPages []SyncPage, replicaHashes map[int]string) ([]SyncPage, error) {
	if len(localPages) == 0 {
		return []SyncPage{}, nil
	}
	
	// Convert to C arrays
	localPageNums := make([]C.int, len(localPages))
	localHashPtrs := make([]*C.char, len(localPages))
	
	replicaPageNums := make([]C.int, len(replicaHashes))
	replicaHashPtrs := make([]*C.char, len(replicaHashes))
	
	// Fill local arrays
	for i, page := range localPages {
		localPageNums[i] = C.int(page.PageNum)
		localHashPtrs[i] = C.CString(page.Hash)
		defer C.free(unsafe.Pointer(localHashPtrs[i]))
	}
	
	// Fill replica arrays
	i := 0
	for pageNum, hash := range replicaHashes {
		replicaPageNums[i] = C.int(pageNum)
		replicaHashPtrs[i] = C.CString(hash)
		defer C.free(unsafe.Pointer(replicaHashPtrs[i]))
		i++
	}
	
	// Compute diff
	const maxDiff = 10000
	diffPages := make([]C.int, maxDiff)
	
	diffCount := C.distcore_compute_diff_pages(
		&localPageNums[0], &localHashPtrs[0],
		&replicaPageNums[0], &replicaHashPtrs[0],
		C.int(len(localPages)), C.int(len(replicaHashes)),
		&diffPages[0], C.int(maxDiff),
	)
	
	if diffCount < 0 {
		return nil, fmt.Errorf("failed to compute diff pages")
	}
	
	// Build result
	result := make([]SyncPage, 0, int(diffCount))
	for i := 0; i < int(diffCount); i++ {
		pageNum := int(diffPages[i])
		// Find the corresponding local page
		for _, page := range localPages {
			if page.PageNum == pageNum {
				result = append(result, page)
				break
			}
		}
	}
	
	return result, nil
}

// SqliteRsyncSession manages a sync session using the sqlite3_rsync protocol
type SqliteRsyncSession struct {
	conn       net.Conn
	file       *VFSFile
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
func NewOriginRsyncSession(conn net.Conn, file *VFSFile, pageCount int) (*SqliteRsyncSession, error) {
	engine, err := GetEngine()
	if err != nil {
		return nil, fmt.Errorf("failed to get OCaml engine: %w", err)
	}
	
	return &SqliteRsyncSession{
		conn:      conn,
		file:      file,
		isOrigin:  true,
		protocol:  1, // Protocol version 1
		pageSize:  file.tracker.pageSize,
		pageCount: pageCount,
		engine:    engine,
		state:     "init",
		errChan:   make(chan error, 1),
	}, nil
}

// NewReplicaRsyncSession creates a new replica (destination) sync session
func NewReplicaRsyncSession(conn net.Conn, file *VFSFile) (*SqliteRsyncSession, error) {
	engine, err := GetEngine()
	if err != nil {
		return nil, fmt.Errorf("failed to get OCaml engine: %w", err)
	}
	
	return &SqliteRsyncSession{
		conn:     conn,
		file:     file,
		isOrigin: false,
		protocol: 1,
		pageSize: file.tracker.pageSize,
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
	
	// Send our page hashes
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

// sendDiffPages computes and sends pages that differ
func (s *SqliteRsyncSession) sendDiffPages(replicaHashes map[int]string) error {
	// Get local pages that need syncing
	localPages, err := s.file.GetSyncPages()
	if err != nil {
		return fmt.Errorf("failed to get sync pages: %w", err)
	}
	
	// Compute which pages actually need to be sent
	diffPages, err := ComputeDiffPages(localPages, replicaHashes)
	if err != nil {
		return fmt.Errorf("failed to compute diff pages: %w", err)
	}
	
	// Send each differing page
	for _, page := range diffPages {
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

// sendReplicaBegin sends replica begin response
func (s *SqliteRsyncSession) sendReplicaBegin() error {
	return s.sendMessage(SqliteRsyncMessage{
		Type:    ReplicaBeginType,
		Payload: []byte{byte(s.protocol)},
	})
}

// sendPageHashes sends hashes of all our pages
func (s *SqliteRsyncSession) sendPageHashes() error {
	pages, err := s.file.GetSyncPages()
	if err != nil {
		return fmt.Errorf("failed to get pages: %w", err)
	}
	
	for _, page := range pages {
		// Ensure hash is exactly 20 bytes (pad or truncate)
		hash := page.Hash
		if len(hash) < 20 {
			hash = hash + string(make([]byte, 20-len(hash)))
		} else if len(hash) > 20 {
			hash = hash[:20]
		}
		
		if err := s.sendMessage(SqliteRsyncMessage{
			Type:    ReplicaHashType,
			Payload: []byte(hash),
		}); err != nil {
			return fmt.Errorf("failed to send hash for page %d: %w", page.PageNum, err)
		}
	}
	
	return nil
}

// sendReplicaReady sends ready signal
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
			
		case OriginEndType:
			return nil
			
		case OriginErrorType:
			return fmt.Errorf("origin error: %s", string(msg.Payload[4:]))
			
		default:
			return fmt.Errorf("unexpected message type: %d", msg.Type)
		}
	}
}

// applyPage applies a received page to our database
func (s *SqliteRsyncSession) applyPage(payload []byte) error {
	if len(payload) < 4 {
		return fmt.Errorf("invalid page payload")
	}
	
	pageNum := int(payload[0])<<24 | int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	pageData := payload[4:]
	
	// Calculate offset and write the page
	offset := int64(pageNum * s.pageSize)
	_, err := s.file.Write(pageData, offset)
	
	return err
}

// sendMessage sends a message to the peer
func (s *SqliteRsyncSession) sendMessage(msg SqliteRsyncMessage) error {
	// Write message type
	if _, err := s.conn.Write([]byte{msg.Type}); err != nil {
		return err
	}
	
	// Write payload
	if len(msg.Payload) > 0 {
		if _, err := s.conn.Write(msg.Payload); err != nil {
			return err
		}
	}
	
	return nil
}

// receiveMessage receives a message from the peer
func (s *SqliteRsyncSession) receiveMessage() (SqliteRsyncMessage, error) {
	// Read message type
	msgType := make([]byte, 1)
	if _, err := s.conn.Read(msgType); err != nil {
		return SqliteRsyncMessage{}, err
	}
	
	// Read payload based on message type
	var payload []byte
	var err error
	
	switch msgType[0] {
	case OriginBeginType:
		payload = make([]byte, 6)
		_, err = s.conn.Read(payload)
		
	case ReplicaBeginType:
		payload = make([]byte, 1)
		_, err = s.conn.Read(payload)
		
	case ReplicaHashType:
		payload = make([]byte, 20)
		_, err = s.conn.Read(payload)
		
	case OriginPageType:
		// Read page number first
		pageNumBytes := make([]byte, 4)
		if _, err := s.conn.Read(pageNumBytes); err != nil {
			return SqliteRsyncMessage{}, err
		}
		
		// Read page data
		pageData := make([]byte, s.pageSize)
		if _, err := s.conn.Read(pageData); err != nil {
			return SqliteRsyncMessage{}, err
		}
		
		payload = append(pageNumBytes, pageData...)
		
	case OriginTxnType:
		payload = make([]byte, 4)
		_, err = s.conn.Read(payload)
		
	case OriginErrorType, OriginMsgType, ReplicaErrorType, ReplicaMsgType:
		// Read length first
		lengthBytes := make([]byte, 4)
		if _, err := s.conn.Read(lengthBytes); err != nil {
			return SqliteRsyncMessage{}, err
		}
		
		length := int(lengthBytes[0])<<24 | int(lengthBytes[1])<<16 | int(lengthBytes[2])<<8 | int(lengthBytes[3])
		
		payload = make([]byte, 4+length)
		copy(payload[:4], lengthBytes)
		_, err = s.conn.Read(payload[4:])
		
	case OriginEndType, ReplicaEndType, ReplicaReadyType:
		// No payload
		payload = []byte{}
		
	default:
		err = fmt.Errorf("unknown message type: 0x%02x", msgType[0])
	}
	
	if err != nil {
		return SqliteRsyncMessage{}, err
	}
	
	return SqliteRsyncMessage{Type: msgType[0], Payload: payload}, nil
}

// Close closes the sync session
func (s *SqliteRsyncSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.errChan)
		err = s.conn.Close()
	})
	return err
}

// Wait waits for the sync to complete
func (s *SqliteRsyncSession) Wait() error {
	select {
	case err := <-s.errChan:
		return err
	default:
		if s.isOrigin {
			return s.RunOriginSync()
		} else {
			return s.RunReplicaSync()
		}
	}
}