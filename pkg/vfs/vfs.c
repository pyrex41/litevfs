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
    void *goCallbacks;          // Go callback functions
} DistVFS;

// Our file structure that wraps the original
typedef struct DistFile {
    sqlite3_file base;          // Must be first
    sqlite3_file *pOriginal;    // Original file handle
    DistVFS *pVFS;             // Our VFS
    char *zPath;               // Database path
    int isWAL;                 // Is this a WAL file?
    int isMain;                // Is this the main database file?
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
    g_distVFS->goCallbacks = goCallbacks;
    
    // Register our VFS
    return sqlite3_vfs_register(&g_distVFS->base, 0);
}

// VFS Methods Implementation

static int distVfsOpen(sqlite3_vfs *pVfs, const char *zName, sqlite3_file *pFile, int flags, int *pOutFlags) {
    DistVFS *pDistVfs = (DistVFS*)pVfs->pAppData;
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    // Allocate space for the original file structure
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
    
    if (pDistFile->pOriginal) {
        rc = pDistFile->pOriginal->pMethods->xClose(pDistFile->pOriginal);
        sqlite3_free(pDistFile->pOriginal);
    }
    
    if (pDistFile->zPath) {
        sqlite3_free(pDistFile->zPath);
    }
    
    return rc;
}

static int distFileRead(sqlite3_file *pFile, void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xRead(pDistFile->pOriginal, zBuf, iAmt, iOfst);
}

static int distFileWrite(sqlite3_file *pFile, const void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    // Perform the write to the original file
    rc = pDistFile->pOriginal->pMethods->xWrite(pDistFile->pOriginal, zBuf, iAmt, iOfst);
    
    // TODO: Here we would notify the Go layer about the write for replication
    // For now, we just pass through to the original implementation
    
    return rc;
}

static int distFileTruncate(sqlite3_file *pFile, sqlite3_int64 size) {
    DistFile *pDistFile = (DistFile*)pFile;
    return pDistFile->pOriginal->pMethods->xTruncate(pDistFile->pOriginal, size);
}

static int distFileSync(sqlite3_file *pFile, int flags) {
    DistFile *pDistFile = (DistFile*)pFile;
    int rc;
    
    // Sync the original file first
    rc = pDistFile->pOriginal->pMethods->xSync(pDistFile->pOriginal, flags);
    
    // TODO: Here we would trigger replication after a successful sync
    // This is where we'd decide between transaction log shipping vs rsync
    
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