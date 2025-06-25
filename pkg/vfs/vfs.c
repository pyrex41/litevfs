#include <sqlite3.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <dlfcn.h>
#include <errno.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <fcntl.h>
#include <pthread.h>
#include <sys/mman.h>

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

// Go callback function pointer types
typedef void (*OnWriteCallback)(const char* dbPath, const void* data, int size, sqlite3_int64 offset);
typedef void (*OnSyncCallback)(const char* dbPath, int flags);
typedef void (*OnTruncateCallback)(const char* dbPath, sqlite3_int64 size);

// Callback registry
typedef struct CallbackRegistry {
    OnWriteCallback onWrite;
    OnSyncCallback onSync;
    OnTruncateCallback onTruncate;
    pthread_mutex_t mutex;
} CallbackRegistry;

// Shared memory structure for IPC with Go sidecar
typedef struct SharedMemory {
    pthread_mutex_t mutex;
    pthread_cond_t cond;
    volatile int pendingOps;
    volatile int maxOps;
    char operations[1024][512]; // Ring buffer for operations
    volatile int head;
    volatile int tail;
} SharedMemory;

// Write batch structure
typedef struct WriteBatch {
    char dbPath[256];
    void* data;
    int totalSize;
    int operationCount;
    sqlite3_int64 firstOffset;
    sqlite3_int64 lastOffset;
    struct WriteBatch* next;
} WriteBatch;

// Our VFS structure that wraps the original
typedef struct DistVFS {
    sqlite3_vfs base;           // Must be first
    sqlite3_vfs *pOriginal;     // Original VFS we're wrapping
    CallbackRegistry callbacks; // Go callback functions
    SharedMemory* sharedMem;    // Shared memory for IPC
    WriteBatch* pendingBatches; // Pending write batches
    pthread_mutex_t batchMutex; // Mutex for batch operations
    int batchThreshold;         // Batch size threshold
    int batchTimeout;           // Batch timeout in milliseconds
} DistVFS;

// Our file structure that wraps the original
typedef struct DistFile {
    sqlite3_file base;          // Must be first
    sqlite3_file *pOriginal;    // Original file handle
    DistVFS *pVFS;             // Our VFS
    char *zPath;               // Database path
    int isWAL;                 // Is this a WAL file?
    int isMain;                // Is this the main database file?
    pthread_mutex_t writeMutex; // Mutex for write operations
    WriteBatch* currentBatch;   // Current write batch
    sqlite3_int64 lastWriteOffset; // Last write offset for batching
} DistFile;

// Global VFS instance
static DistVFS *g_distVFS = NULL;

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

// Initialize shared memory for IPC
static int initSharedMemory(DistVFS* vfs) {
    // Create shared memory segment
    int fd = shm_open("/distsqlite_ipc", O_CREAT | O_RDWR | O_EXCL, 0666);
    if (fd < 0) {
        // Try to open existing segment
        fd = shm_open("/distsqlite_ipc", O_RDWR, 0666);
        if (fd < 0) {
            return SQLITE_ERROR;
        }
    } else {
        // New segment, set size
        if (ftruncate(fd, sizeof(SharedMemory)) < 0) {
            close(fd);
            shm_unlink("/distsqlite_ipc");
            return SQLITE_ERROR;
        }
    }
    
    // Map shared memory
    vfs->sharedMem = mmap(NULL, sizeof(SharedMemory), PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    close(fd);
    
    if (vfs->sharedMem == MAP_FAILED) {
        return SQLITE_ERROR;
    }
    
    // Initialize mutex and condition variable for inter-process synchronization
    pthread_mutexattr_t mutexAttr;
    pthread_mutexattr_init(&mutexAttr);
    pthread_mutexattr_setpshared(&mutexAttr, PTHREAD_PROCESS_SHARED);
    pthread_mutex_init(&vfs->sharedMem->mutex, &mutexAttr);
    pthread_mutexattr_destroy(&mutexAttr);
    
    pthread_condattr_t condAttr;
    pthread_condattr_init(&condAttr);
    pthread_condattr_setpshared(&condAttr, PTHREAD_PROCESS_SHARED);
    pthread_cond_init(&vfs->sharedMem->cond, &condAttr);
    pthread_condattr_destroy(&condAttr);
    
    // Initialize ring buffer
    vfs->sharedMem->pendingOps = 0;
    vfs->sharedMem->maxOps = 1024;
    vfs->sharedMem->head = 0;
    vfs->sharedMem->tail = 0;
    
    return SQLITE_OK;
}

// Add operation to shared memory ring buffer
static int addOperationToSharedMem(DistVFS* vfs, const char* operation) {
    if (!vfs->sharedMem) return SQLITE_ERROR;
    
    pthread_mutex_lock(&vfs->sharedMem->mutex);
    
    // Check if buffer is full
    if (vfs->sharedMem->pendingOps >= vfs->sharedMem->maxOps) {
        pthread_mutex_unlock(&vfs->sharedMem->mutex);
        return SQLITE_FULL;
    }
    
    // Add operation to ring buffer
    strncpy(vfs->sharedMem->operations[vfs->sharedMem->head], operation, 511);
    vfs->sharedMem->operations[vfs->sharedMem->head][511] = '\0';
    
    vfs->sharedMem->head = (vfs->sharedMem->head + 1) % vfs->sharedMem->maxOps;
    vfs->sharedMem->pendingOps++;
    
    // Signal waiting processes
    pthread_cond_signal(&vfs->sharedMem->cond);
    pthread_mutex_unlock(&vfs->sharedMem->mutex);
    
    return SQLITE_OK;
}

// Register Go callbacks
int distVFSRegisterCallbacks(OnWriteCallback onWrite, OnSyncCallback onSync, OnTruncateCallback onTruncate) {
    if (!g_distVFS) return SQLITE_ERROR;
    
    pthread_mutex_lock(&g_distVFS->callbacks.mutex);
    g_distVFS->callbacks.onWrite = onWrite;
    g_distVFS->callbacks.onSync = onSync;
    g_distVFS->callbacks.onTruncate = onTruncate;
    pthread_mutex_unlock(&g_distVFS->callbacks.mutex);
    
    return SQLITE_OK;
}

// Initialize our VFS by wrapping the default VFS
int distVFSInit(void) {
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
    
    // CRITICAL FIX: Set correct size for our DistFile structure
    g_distVFS->base.szOsFile = sizeof(DistFile);
    
    // Override with our methods
    g_distVFS->base.zName = "distsqlite";
    g_distVFS->base.pAppData = g_distVFS;
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
    
    // Initialize callback mutex
    pthread_mutex_init(&g_distVFS->callbacks.mutex, NULL);
    pthread_mutex_init(&g_distVFS->batchMutex, NULL);
    
    // Initialize shared memory for IPC
    if (initSharedMemory(g_distVFS) != SQLITE_OK) {
        sqlite3_free(g_distVFS);
        g_distVFS = NULL;
        return SQLITE_ERROR;
    }
    
    // Set default batch parameters
    g_distVFS->batchThreshold = 64 * 1024; // 64KB batch threshold
    g_distVFS->batchTimeout = 100; // 100ms batch timeout
    
    // Register our VFS
    return sqlite3_vfs_register(&g_distVFS->base, 0);
}

// Cleanup function
int distVFSShutdown(void) {
    if (!g_distVFS) return SQLITE_OK;
    
    // Unregister VFS
    sqlite3_vfs_unregister(&g_distVFS->base);
    
    // Cleanup shared memory
    if (g_distVFS->sharedMem) {
        pthread_mutex_destroy(&g_distVFS->sharedMem->mutex);
        pthread_cond_destroy(&g_distVFS->sharedMem->cond);
        munmap(g_distVFS->sharedMem, sizeof(SharedMemory));
        shm_unlink("/distsqlite_ipc");
    }
    
    // Cleanup mutexes
    pthread_mutex_destroy(&g_distVFS->callbacks.mutex);
    pthread_mutex_destroy(&g_distVFS->batchMutex);
    
    // Free VFS structure
    sqlite3_free(g_distVFS);
    g_distVFS = NULL;
    
    return SQLITE_OK;
}

// VFS Methods Implementation

static int distVfsOpen(sqlite3_vfs *pVfs, const char *zName, sqlite3_file *pFile, int flags, int *pOutFlags) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    // Allocate space for the original file structure - CRITICAL FIX
    pDistFile->pOriginal = sqlite3_malloc(pDistVfs->pOriginal->szOsFile);
    if (!pDistFile->pOriginal) {
        return SQLITE_NOMEM;
    }
    
    // Call original VFS open
    rc = pDistVfs->pOriginal->xOpen(pDistVfs->pOriginal, zName, pDistFile->pOriginal, flags, pOutFlags);
    if (rc != SQLITE_OK) {
        sqlite3_free(pDistFile->pOriginal);
        return rc;
    }
    
    // Initialize our file structure
    pDistFile->base.pMethods = &distIoMethods;
    pDistFile->pVFS = pDistVfs;
    pDistFile->currentBatch = NULL;
    pDistFile->lastWriteOffset = -1;
    
    // Initialize write mutex
    pthread_mutex_init(&pDistFile->writeMutex, NULL);
    
    // Store database path and determine file type
    if (zName) {
        size_t len = strlen(zName);
        pDistFile->zPath = sqlite3_malloc(len + 1);
        if (pDistFile->zPath) {
            strcpy(pDistFile->zPath, zName);
            
            // Check if this is a WAL file
            pDistFile->isWAL = (len > 4 && strcmp(zName + len - 4, "-wal") == 0);
            
            // Check if this is main database file (not WAL, not SHM, not journal)
            pDistFile->isMain = !pDistFile->isWAL && 
                              !(len > 4 && strcmp(zName + len - 4, "-shm") == 0) &&
                              !(len > 8 && strcmp(zName + len - 8, "-journal") == 0);
        }
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
    
    // Flush any pending batches
    if (pDistFile->currentBatch) {
        // TODO: Flush batch before closing
        sqlite3_free(pDistFile->currentBatch->data);
        sqlite3_free(pDistFile->currentBatch);
    }
    
    if (pDistFile->pOriginal) {
        rc = pDistFile->pOriginal->pMethods->xClose(pDistFile->pOriginal);
        sqlite3_free(pDistFile->pOriginal);
    }
    
    if (pDistFile->zPath) {
        sqlite3_free(pDistFile->zPath);
    }
    
    // Cleanup mutex
    pthread_mutex_destroy(&pDistFile->writeMutex);
    
    return rc;
}

static int distFileRead(sqlite3_file *pFile, void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xRead(pDistFile->pOriginal, zBuf, iAmt, iOfst);
}

static int distFileWrite(sqlite3_file *pFile, const void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    pthread_mutex_lock(&pDistFile->writeMutex);
    
    // Perform the write to the original file
    rc = pDistFile->pOriginal->pMethods->xWrite(pDistFile->pOriginal, zBuf, iAmt, iOfst);
    
    if (rc == SQLITE_OK) {
        // Add to shared memory operation queue
        char operation[512];
        snprintf(operation, sizeof(operation), "WRITE|%s|%lld|%d", 
                pDistFile->zPath ? pDistFile->zPath : "unknown", 
                (long long)iOfst, iAmt);
        addOperationToSharedMem(pDistFile->pVFS, operation);
        
        // Call Go callback if registered
        pthread_mutex_lock(&pDistFile->pVFS->callbacks.mutex);
        if (pDistFile->pVFS->callbacks.onWrite) {
            pDistFile->pVFS->callbacks.onWrite(pDistFile->zPath, zBuf, iAmt, iOfst);
        }
        pthread_mutex_unlock(&pDistFile->pVFS->callbacks.mutex);
    }
    
    pthread_mutex_unlock(&pDistFile->writeMutex);
    
    return rc;
}

static int distFileTruncate(sqlite3_file *pFile, sqlite3_int64 size) {
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    rc = pDistFile->pOriginal->pMethods->xTruncate(pDistFile->pOriginal, size);
    
    if (rc == SQLITE_OK) {
        // Add to shared memory operation queue
        char operation[512];
        snprintf(operation, sizeof(operation), "TRUNCATE|%s|%lld", 
                pDistFile->zPath ? pDistFile->zPath : "unknown", 
                (long long)size);
        addOperationToSharedMem(pDistFile->pVFS, operation);
        
        // Call Go callback if registered
        pthread_mutex_lock(&pDistFile->pVFS->callbacks.mutex);
        if (pDistFile->pVFS->callbacks.onTruncate) {
            pDistFile->pVFS->callbacks.onTruncate(pDistFile->zPath, size);
        }
        pthread_mutex_unlock(&pDistFile->pVFS->callbacks.mutex);
    }
    
    return rc;
}

static int distFileSync(sqlite3_file *pFile, int flags) {
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    // Sync the original file first
    rc = pDistFile->pOriginal->pMethods->xSync(pDistFile->pOriginal, flags);
    
    if (rc == SQLITE_OK) {
        // Add to shared memory operation queue
        char operation[512];
        snprintf(operation, sizeof(operation), "SYNC|%s|%d", 
                pDistFile->zPath ? pDistFile->zPath : "unknown", flags);
        addOperationToSharedMem(pDistFile->pVFS, operation);
        
        // Call Go callback if registered
        pthread_mutex_lock(&pDistFile->pVFS->callbacks.mutex);
        if (pDistFile->pVFS->callbacks.onSync) {
            pDistFile->pVFS->callbacks.onSync(pDistFile->zPath, flags);
        }
        pthread_mutex_unlock(&pDistFile->pVFS->callbacks.mutex);
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