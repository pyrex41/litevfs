#include <sqlite3.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stddef.h>
#include <unistd.h>
#include <dlfcn.h>
#include <errno.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <fcntl.h>
#include <sys/mman.h>
#include <sys/ipc.h>
#include <sys/shm.h>
#include <semaphore.h>
#include <time.h>

// Shared memory IPC structures for communicating with Go sidecar
#define SHM_BASE_KEY 0x44495354  // "DIST" in hex - base key for derivation
#define MAX_WRITE_EVENTS 1000
#define MAX_PATH_LEN 512
#define MAX_DATA_LEN 8192

// Generate safe shared memory key to prevent conflicts and security issues
static key_t generateSafeSharedMemoryKey(void) {
    pid_t pid = getpid();
    uid_t uid = getuid();
    
    // Combine base key with PID and UID to create unique, safe key
    key_t key = SHM_BASE_KEY ^ (pid << 16) ^ (uid << 8);
    
    printf("[DistSQLite VFS] Generated safe shared memory key: 0x%x (PID: %d, UID: %d)\n", 
           key, pid, uid);
    
    return key;
}

typedef struct {
    char path[MAX_PATH_LEN];
    int64_t offset;
    int32_t length;
    int32_t is_wal;
    int64_t timestamp_ns;
    char data[MAX_DATA_LEN];
} WriteEvent;

typedef struct {
    sem_t mutex;
    sem_t producer_sem;
    sem_t consumer_sem;
    volatile int head;
    volatile int tail;
    volatile int shutdown;
    WriteEvent events[MAX_WRITE_EVENTS];
} SharedMemory;

// Forward declarations
static int distVfsOpen(sqlite3_vfs*, const char *zName, sqlite3_file*, int flags, int *pOutFlags);
static int distVfsDelete(sqlite3_vfs*, const char *zName, int syncDir);
static int distVfsAccess(sqlite3_vfs*, const char *zName, int flags, int *pResOut);
static int distVfsFullPathname(sqlite3_vfs*, const char *zName, int nOut, char *zOut);
static void* distVfsDlOpen(sqlite3_vfs*, const char *zFilename);
static void distVfsDlError(sqlite3_vfs*, int nByte, char *zErrMsg);
static void (*distVfsDlSym(sqlite3_vfs*, void*, const char *zSymbol))(void);
static void distVfsDlClose(sqlite3_vfs*, void*);
static int distVfsRandomness(sqlite3_vfs*, int nByte, char *zOut);
static int distVfsSleep(sqlite3_vfs*, int microseconds);
static int distVfsCurrentTime(sqlite3_vfs*, double*);
static int distVfsGetLastError(sqlite3_vfs*, int, char*);
static int distVfsCurrentTimeInt64(sqlite3_vfs*, sqlite3_int64*);

// File methods
static int distFileClose(sqlite3_file*);
static int distFileRead(sqlite3_file*, void*, int iAmt, sqlite3_int64 iOfst);
static int distFileWrite(sqlite3_file*, const void*, int iAmt, sqlite3_int64 iOfst);
static int distFileTruncate(sqlite3_file*, sqlite3_int64 size);
static int distFileSync(sqlite3_file*, int flags);
static int distFileFileSize(sqlite3_file*, sqlite3_int64 *pSize);
static int distFileLock(sqlite3_file*, int);
static int distFileUnlock(sqlite3_file*, int);
static int distFileCheckReservedLock(sqlite3_file*, int *pResOut);
static int distFileFileControl(sqlite3_file*, int op, void *pArg);
static int distFileSectorSize(sqlite3_file*);
static int distFileDeviceCharacteristics(sqlite3_file*);

// Our VFS structure that wraps the original
typedef struct DistVFS {
    sqlite3_vfs base;           // Must be first
    sqlite3_vfs *pOriginal;     // Original VFS we're wrapping
    SharedMemory *shm;          // Shared memory for IPC
    int shm_id;                 // Shared memory ID
} DistVFS;

// Our file structure that wraps the original
typedef struct DistFile {
    sqlite3_file base;          // Must be first
    sqlite3_file *pOriginal;    // Original file handle
    DistVFS *pVFS;             // Our VFS
    char *zPath;               // Database path
    int isWAL;                 // Is this a WAL file?
    int isMain;                // Is this the main database file?
    int writeCount;            // Number of writes since last batch
    int64_t lastBatchTime;     // Last batch time in nanoseconds
} DistFile;

// Compile-time assertion to ensure base struct is first in DistFile
// This is required by SQLite's VFS interface
_Static_assert(offsetof(DistFile, base) == 0, 
               "sqlite3_file base must be first member of DistFile");

// Global VFS instance
static DistVFS *g_distVFS = NULL;

// Write batching configuration
#define BATCH_SIZE_THRESHOLD 10
#define BATCH_TIME_THRESHOLD_NS (100 * 1000000) // 100ms in nanoseconds

// File method table
static const sqlite3_io_methods distIoMethods = {
    3,                                      // iVersion
    distFileClose,                          // xClose
    distFileRead,                           // xRead
    distFileWrite,                          // xWrite
    distFileTruncate,                       // xTruncate
    distFileSync,                           // xSync
    distFileFileSize,                       // xFileSize
    distFileLock,                           // xLock
    distFileUnlock,                         // xUnlock
    distFileCheckReservedLock,              // xCheckReservedLock
    distFileFileControl,                    // xFileControl
    distFileSectorSize,                     // xSectorSize
    distFileDeviceCharacteristics,          // xDeviceCharacteristics
    NULL,                                   // xShmMap
    NULL,                                   // xShmLock
    NULL,                                   // xShmBarrier
    NULL,                                   // xShmUnmap
    NULL,                                   // xFetch
    NULL                                    // xUnfetch
};

// Get current time in nanoseconds
static int64_t getCurrentTimeNs(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

// Initialize shared memory for IPC with Go sidecar
static int initSharedMemory(DistVFS *pVfs) {
    // Create or get shared memory segment with dynamic key
    key_t shmKey = generateSafeSharedMemoryKey();
    pVfs->shm_id = shmget(shmKey, sizeof(SharedMemory), IPC_CREAT | 0600); // Restrict permissions
    if (pVfs->shm_id == -1) {
        fprintf(stderr, "[DistSQLite VFS] Failed to create shared memory: %s\n", strerror(errno));
        return SQLITE_ERROR;
    }
    
    // Attach to shared memory
    pVfs->shm = (SharedMemory*)shmat(pVfs->shm_id, NULL, 0);
    if (pVfs->shm == (void*)-1) {
        fprintf(stderr, "[DistSQLite VFS] Failed to attach to shared memory: %s\n", strerror(errno));
        // Clean up shared memory segment on failure
        shmctl(pVfs->shm_id, IPC_RMID, NULL);
        return SQLITE_ERROR;
    }
    
    // Initialize shared memory structure (only if we're the first)
    if (pVfs->shm->head == 0 && pVfs->shm->tail == 0) {
        printf("[DistSQLite VFS] Initializing shared memory IPC\n");
        
        // Initialize semaphores with proper cleanup on failure
        if (sem_init(&pVfs->shm->mutex, 1, 1) != 0) {
            fprintf(stderr, "[DistSQLite VFS] Failed to initialize mutex semaphore: %s\n", strerror(errno));
            goto cleanup_shm;
        }
        
        if (sem_init(&pVfs->shm->producer_sem, 1, MAX_WRITE_EVENTS) != 0) {
            fprintf(stderr, "[DistSQLite VFS] Failed to initialize producer semaphore: %s\n", strerror(errno));
            sem_destroy(&pVfs->shm->mutex);
            goto cleanup_shm;
        }
        
        if (sem_init(&pVfs->shm->consumer_sem, 1, 0) != 0) {
            fprintf(stderr, "[DistSQLite VFS] Failed to initialize consumer semaphore: %s\n", strerror(errno));
            sem_destroy(&pVfs->shm->mutex);
            sem_destroy(&pVfs->shm->producer_sem);
            goto cleanup_shm;
        }
        
        pVfs->shm->head = 0;
        pVfs->shm->tail = 0;
        pVfs->shm->shutdown = 0;
    }
    
    return SQLITE_OK;

cleanup_shm:
    shmdt(pVfs->shm);
    shmctl(pVfs->shm_id, IPC_RMID, NULL);
    pVfs->shm = NULL;
    return SQLITE_ERROR;
}

// Send write event to Go sidecar via shared memory
static void notifyWriteEvent(DistFile *pDistFile, int64_t offset, const void *data, int length) {
    if (!pDistFile->pVFS->shm) return;
    
    SharedMemory *shm = pDistFile->pVFS->shm;
    
    // Try to acquire producer semaphore (non-blocking)
    struct timespec timeout;
    clock_gettime(CLOCK_REALTIME, &timeout);
    timeout.tv_nsec += 1000000; // 1ms timeout
    if (timeout.tv_nsec >= 1000000000) {
        timeout.tv_sec++;
        timeout.tv_nsec -= 1000000000;
    }
    
    if (sem_timedwait(&shm->producer_sem, &timeout) != 0) {
        // Queue is full or timeout - skip this write notification
        return;
    }
    
    // Acquire mutex
    sem_wait(&shm->mutex);
    
    // Add write event to circular buffer
    WriteEvent *event = &shm->events[shm->tail];
    
    // Copy path (with bounds checking)
    if (pDistFile->zPath) {
        strncpy(event->path, pDistFile->zPath, MAX_PATH_LEN - 1);
        event->path[MAX_PATH_LEN - 1] = '\0';
    } else {
        event->path[0] = '\0';
    }
    
    event->offset = offset;
    event->length = length;
    event->is_wal = pDistFile->isWAL;
    event->timestamp_ns = getCurrentTimeNs();
    
    // Copy data (with bounds checking)
    int copyLen = length < MAX_DATA_LEN ? length : MAX_DATA_LEN;
    if (data && copyLen > 0) {
        memcpy(event->data, data, copyLen);
    }
    event->length = copyLen; // Update length to reflect actual copied data
    
    // Advance tail pointer
    shm->tail = (shm->tail + 1) % MAX_WRITE_EVENTS;
    
    // Release mutex
    sem_post(&shm->mutex);
    
    // Signal consumer
    sem_post(&shm->consumer_sem);
}

// Initialize our VFS by wrapping the default VFS
int distVFSInit(void *goCallbacks) {
    sqlite3_vfs *pOriginal = sqlite3_vfs_find(NULL);
    if (!pOriginal) {
        return SQLITE_ERROR;
    }
    
    // Already initialized
    if (g_distVFS) {
        return SQLITE_OK;
    }
    
    // Allocate our VFS structure
    g_distVFS = sqlite3_malloc(sizeof(DistVFS));
    if (!g_distVFS) {
        return SQLITE_NOMEM;
    }
    memset(g_distVFS, 0, sizeof(DistVFS));
    
    // Copy original VFS structure
    memcpy(&g_distVFS->base, pOriginal, sizeof(sqlite3_vfs));
    
    // Override with our methods
    g_distVFS->base.zName = "distsqlite";
    g_distVFS->base.pAppData = g_distVFS;
    // Ensure our file size is at least as large as the original VFS requires
    // This is critical for SQLite compatibility
    g_distVFS->base.szOsFile = (sizeof(DistFile) > pOriginal->szOsFile) 
                               ? sizeof(DistFile) 
                               : pOriginal->szOsFile;
    g_distVFS->base.xOpen = distVfsOpen;
    g_distVFS->base.xDelete = distVfsDelete;
    g_distVFS->base.xAccess = distVfsAccess;
    g_distVFS->base.xFullPathname = distVfsFullPathname;
    g_distVFS->base.xDlOpen = distVfsDlOpen;
    g_distVFS->base.xDlError = distVfsDlError;
    g_distVFS->base.xDlSym = distVfsDlSym;
    g_distVFS->base.xDlClose = distVfsDlClose;
    g_distVFS->base.xRandomness = distVfsRandomness;
    g_distVFS->base.xSleep = distVfsSleep;
    g_distVFS->base.xCurrentTime = distVfsCurrentTime;
    g_distVFS->base.xGetLastError = distVfsGetLastError;
    g_distVFS->base.xCurrentTimeInt64 = distVfsCurrentTimeInt64;
    
    // Store references
    g_distVFS->pOriginal = pOriginal;
    
    // Initialize shared memory for IPC
    if (initSharedMemory(g_distVFS) != SQLITE_OK) {
        fprintf(stderr, "[DistSQLite VFS] Warning: Shared memory IPC initialization failed\n");
        // Continue without IPC - degraded mode
    } else {
        printf("[DistSQLite VFS] Shared memory IPC initialized successfully\n");
    }
    
    // Register our VFS
    int rc = sqlite3_vfs_register(&g_distVFS->base, 0);
    if (rc == SQLITE_OK) {
        printf("[DistSQLite VFS] Registered successfully\n");
    }
    return rc;
}

// VFS Methods Implementation

static int distVfsOpen(sqlite3_vfs *pVfs, const char *zName, sqlite3_file *pFile, int flags, int *pOutFlags) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    // Initialize our file structure first
    memset(pDistFile, 0, sizeof(DistFile));
    pDistFile->base.pMethods = &distIoMethods;
    pDistFile->pVFS = pDistVfs;
    pDistFile->writeCount = 0;
    pDistFile->lastBatchTime = getCurrentTimeNs();
    
    // Allocate space for the original file structure
    pDistFile->pOriginal = sqlite3_malloc(pDistVfs->pOriginal->szOsFile);
    if (!pDistFile->pOriginal) {
        return SQLITE_NOMEM;
    }
    
    // Call original VFS open
    rc = pDistVfs->pOriginal->xOpen(pDistVfs->pOriginal, zName, pDistFile->pOriginal, flags, pOutFlags);
    if (rc != SQLITE_OK) {
        // Clean up on failure to prevent memory leak
        sqlite3_free(pDistFile->pOriginal);
        pDistFile->pOriginal = NULL;
        return rc;
    }
    
    // Store database path and determine file type
    if (zName) {
        size_t len = strlen(zName);
        pDistFile->zPath = sqlite3_malloc(len + 1);
        if (!pDistFile->zPath) {
            // Clean up on allocation failure
            pDistFile->pOriginal->pMethods->xClose(pDistFile->pOriginal);
            sqlite3_free(pDistFile->pOriginal);
            pDistFile->pOriginal = NULL;
            return SQLITE_NOMEM;
        }
        
        strcpy(pDistFile->zPath, zName);
        
        // Check if this is a WAL file
        pDistFile->isWAL = (len > 4 && strcmp(zName + len - 4, "-wal") == 0);
        
        // Check if this is main database file (not WAL, not SHM, not journal)
        pDistFile->isMain = !pDistFile->isWAL && 
                          !(len > 4 && strcmp(zName + len - 4, "-shm") == 0) &&
                          !(len > 8 && strcmp(zName + len - 8, "-journal") == 0);
    }
    
    return SQLITE_OK;
}

static int distVfsDelete(sqlite3_vfs *pVfs, const char *zName, int syncDir) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xDelete(pDistVfs->pOriginal, zName, syncDir);
}

static int distVfsAccess(sqlite3_vfs *pVfs, const char *zName, int flags, int *pResOut) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xAccess(pDistVfs->pOriginal, zName, flags, pResOut);
}

static int distVfsFullPathname(sqlite3_vfs *pVfs, const char *zName, int nOut, char *zOut) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xFullPathname(pDistVfs->pOriginal, zName, nOut, zOut);
}

static void* distVfsDlOpen(sqlite3_vfs *pVfs, const char *zFilename) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xDlOpen(pDistVfs->pOriginal, zFilename);
}

static void distVfsDlError(sqlite3_vfs *pVfs, int nByte, char *zErrMsg) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    pDistVfs->pOriginal->xDlError(pDistVfs->pOriginal, nByte, zErrMsg);
}

static void (*distVfsDlSym(sqlite3_vfs *pVfs, void *pHandle, const char *zSymbol))(void) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xDlSym(pDistVfs->pOriginal, pHandle, zSymbol);
}

static void distVfsDlClose(sqlite3_vfs *pVfs, void *pHandle) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    pDistVfs->pOriginal->xDlClose(pDistVfs->pOriginal, pHandle);
}

static int distVfsRandomness(sqlite3_vfs *pVfs, int nByte, char *zOut) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xRandomness(pDistVfs->pOriginal, nByte, zOut);
}

static int distVfsSleep(sqlite3_vfs *pVfs, int microseconds) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xSleep(pDistVfs->pOriginal, microseconds);
}

static int distVfsCurrentTime(sqlite3_vfs *pVfs, double *pTimeOut) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xCurrentTime(pDistVfs->pOriginal, pTimeOut);
}

static int distVfsGetLastError(sqlite3_vfs *pVfs, int nBuf, char *zBuf) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xGetLastError(pDistVfs->pOriginal, nBuf, zBuf);
}

static int distVfsCurrentTimeInt64(sqlite3_vfs *pVfs, sqlite3_int64 *piNow) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    return pDistVfs->pOriginal->xCurrentTimeInt64(pDistVfs->pOriginal, piNow);
}

// File Methods Implementation

static int distFileClose(sqlite3_file *pFile) {
    DistFile *pDistFile = (DistFile*)pFile;
    int rc = SQLITE_OK;
    
    // Flush any pending writes
    if (pDistFile->writeCount > 0) {
        notifyWriteEvent(pDistFile, -1, NULL, 0); // Special flush event
    }
    
    // Clean up original file handle
    if (pDistFile->pOriginal) {
        rc = pDistFile->pOriginal->pMethods->xClose(pDistFile->pOriginal);
        sqlite3_free(pDistFile->pOriginal);
        pDistFile->pOriginal = NULL;
    }
    
    // Clean up path string
    if (pDistFile->zPath) {
        sqlite3_free(pDistFile->zPath);
        pDistFile->zPath = NULL;
    }
    
    return rc;
}

static int distFileRead(sqlite3_file *pFile, void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    DistFile *pDistFile = (DistFile*)pFile;
    // Reads are passed through directly - no replication needed
    return pDistFile->pOriginal->pMethods->xRead(pDistFile->pOriginal, zBuf, iAmt, iOfst);
}

static int distFileWrite(sqlite3_file *pFile, const void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    // Perform the write to the original file first
    rc = pDistFile->pOriginal->pMethods->xWrite(pDistFile->pOriginal, zBuf, iAmt, iOfst);
    if (rc != SQLITE_OK) {
        return rc;
    }
    
    // Notify Go sidecar about the write for replication
    // Only track writes to main database and WAL files
    if (pDistFile->isMain || pDistFile->isWAL) {
        notifyWriteEvent(pDistFile, iOfst, zBuf, iAmt);
        pDistFile->writeCount++;
        
        // Check if we should trigger immediate replication
        int64_t currentTime = getCurrentTimeNs();
        if (pDistFile->writeCount >= BATCH_SIZE_THRESHOLD || 
            (currentTime - pDistFile->lastBatchTime) >= BATCH_TIME_THRESHOLD_NS) {
            
            // Send batch completion signal
            notifyWriteEvent(pDistFile, -2, NULL, 0); // Special batch signal
            pDistFile->writeCount = 0;
            pDistFile->lastBatchTime = currentTime;
        }
    }
    
    return rc;
}

static int distFileTruncate(sqlite3_file *pFile, sqlite3_int64 size) {
    DistFile *pDistFile = (DistFile*)pFile;
    int rc = pDistFile->pOriginal->pMethods->xTruncate(pDistFile->pOriginal, size);
    
    // Notify about truncation
    if (rc == SQLITE_OK && (pDistFile->isMain || pDistFile->isWAL)) {
        notifyWriteEvent(pDistFile, -3, &size, sizeof(size)); // Special truncate signal
    }
    
    return rc;
}

static int distFileSync(sqlite3_file *pFile, int flags) {
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    // Sync the original file first
    rc = pDistFile->pOriginal->pMethods->xSync(pDistFile->pOriginal, flags);
    if (rc != SQLITE_OK) {
        return rc;
    }
    
    // After successful sync, trigger replication
    // This is a natural batching point for writes
    if (pDistFile->isMain || pDistFile->isWAL) {
        notifyWriteEvent(pDistFile, -4, &flags, sizeof(flags)); // Special sync signal
        pDistFile->writeCount = 0; // Reset write count after sync
        pDistFile->lastBatchTime = getCurrentTimeNs();
    }
    
    return rc;
}

static int distFileFileSize(sqlite3_file *pFile, sqlite3_int64 *pSize) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xFileSize(pDistFile->pOriginal, pSize);
}

static int distFileLock(sqlite3_file *pFile, int eLock) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xLock(pDistFile->pOriginal, eLock);
}

static int distFileUnlock(sqlite3_file *pFile, int eLock) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xUnlock(pDistFile->pOriginal, eLock);
}

static int distFileCheckReservedLock(sqlite3_file *pFile, int *pResOut) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xCheckReservedLock(pDistFile->pOriginal, pResOut);
}

static int distFileFileControl(sqlite3_file *pFile, int op, void *pArg) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xFileControl(pDistFile->pOriginal, op, pArg);
}

static int distFileSectorSize(sqlite3_file *pFile) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xSectorSize(pDistFile->pOriginal);
}

static int distFileDeviceCharacteristics(sqlite3_file *pFile) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xDeviceCharacteristics(pDistFile->pOriginal);
}