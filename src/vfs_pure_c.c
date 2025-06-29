/* Pure C SQLite VFS for DistSQLite - minimal implementation */

#include <sqlite3.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>

/* Original VFS we're wrapping */
static sqlite3_vfs *original_vfs = NULL;

/* DistSQLite VFS structure */
typedef struct distsqlite_file {
    sqlite3_file base;
    sqlite3_file *real_file;
    char *filename;
} distsqlite_file;

/* Forward declarations */
static int distsqlite_open(sqlite3_vfs*, const char *zName, sqlite3_file*, int flags, int *pOutFlags);
static int distsqlite_close(sqlite3_file*);
static int distsqlite_read(sqlite3_file*, void*, int iAmt, sqlite3_int64 iOfst);
static int distsqlite_write(sqlite3_file*, const void*, int iAmt, sqlite3_int64 iOfst);
static int distsqlite_truncate(sqlite3_file*, sqlite3_int64 size);
static int distsqlite_sync(sqlite3_file*, int flags);
static int distsqlite_filesize(sqlite3_file*, sqlite3_int64 *pSize);

/* IO Methods */
static sqlite3_io_methods distsqlite_io_methods = {
    1,                          /* iVersion */
    distsqlite_close,          /* xClose */
    distsqlite_read,           /* xRead */
    distsqlite_write,          /* xWrite */
    distsqlite_truncate,       /* xTruncate */
    distsqlite_sync,           /* xSync */
    distsqlite_filesize,       /* xFileSize */
    NULL,                      /* xLock */
    NULL,                      /* xUnlock */
    NULL,                      /* xCheckReservedLock */
    NULL,                      /* xFileControl */
    NULL,                      /* xSectorSize */
    NULL,                      /* xDeviceCharacteristics */
    NULL, NULL, NULL, NULL,    /* xShmMap, xShmLock, xShmBarrier, xShmUnmap */
    NULL, NULL                 /* xFetch, xUnfetch */
};

/* Open file */
static int distsqlite_open(sqlite3_vfs *vfs, const char *zName, sqlite3_file *pFile, 
                          int flags, int *pOutFlags) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    
    printf("[DistSQLite VFS] Opening file: %s (flags=0x%x)\n", 
           zName ? zName : "<memory>", flags);
    
    /* Decode flags for debugging */
    if (flags & SQLITE_OPEN_MAIN_DB) printf("  - Main database\n");
    if (flags & SQLITE_OPEN_TEMP_DB) printf("  - Temp database\n");
    if (flags & SQLITE_OPEN_TRANSIENT_DB) printf("  - Transient database\n");
    if (flags & SQLITE_OPEN_MAIN_JOURNAL) printf("  - Main journal\n");
    if (flags & SQLITE_OPEN_TEMP_JOURNAL) printf("  - Temp journal\n");
    if (flags & SQLITE_OPEN_SUBJOURNAL) printf("  - Subjournal\n");
    if (flags & SQLITE_OPEN_SUPER_JOURNAL) printf("  - Super journal\n");
    if (flags & SQLITE_OPEN_WAL) printf("  - WAL file\n");
    
    /* Allocate space for the real file */
    p->real_file = (sqlite3_file*)malloc(original_vfs->szOsFile);
    if (!p->real_file) return SQLITE_NOMEM;
    
    /* Store filename */
    if (zName) {
        p->filename = strdup(zName);
    } else {
        p->filename = NULL;
    }
    
    /* Open the real file */
    int rc = original_vfs->xOpen(original_vfs, zName, p->real_file, flags, pOutFlags);
    if (rc == SQLITE_OK) {
        p->base.pMethods = &distsqlite_io_methods;
        printf("[DistSQLite VFS] File opened successfully\n");
    } else {
        free(p->real_file);
        if (p->filename) free(p->filename);
        printf("[DistSQLite VFS] Failed to open file: %d\n", rc);
    }
    
    return rc;
}

/* Close file */
static int distsqlite_close(sqlite3_file *pFile) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    
    printf("[DistSQLite VFS] Closing file: %s\n", p->filename ? p->filename : "<memory>");
    
    int rc = p->real_file->pMethods->xClose(p->real_file);
    free(p->real_file);
    if (p->filename) free(p->filename);
    
    return rc;
}

/* Read from file */
static int distsqlite_read(sqlite3_file *pFile, void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    
    /* For now, just pass through to real file */
    return p->real_file->pMethods->xRead(p->real_file, zBuf, iAmt, iOfst);
}

/* Write to file */
static int distsqlite_write(sqlite3_file *pFile, const void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    
    printf("[DistSQLite VFS] Write: %d bytes at offset %lld to %s\n", 
           iAmt, (long long)iOfst, p->filename ? p->filename : "<memory>");
    
    /* Check if this is a WAL write */
    if (p->filename && strstr(p->filename, "-wal")) {
        printf("[DistSQLite VFS] WAL write detected - replication point\n");
    }
    
    /* TODO: This is where we'd capture writes for replication */
    /* For now, let's just log the first few bytes */
    if (iAmt > 0) {
        unsigned char *data = (unsigned char*)zBuf;
        printf("[DistSQLite VFS] Data preview: %02x %02x %02x %02x...\n",
               data[0], data[1], data[2], data[3]);
    }
    
    /* Pass through to real file */
    return p->real_file->pMethods->xWrite(p->real_file, zBuf, iAmt, iOfst);
}

/* Truncate file */
static int distsqlite_truncate(sqlite3_file *pFile, sqlite3_int64 size) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    printf("[DistSQLite VFS] Truncate: %s to %lld bytes\n", 
           p->filename ? p->filename : "<memory>", (long long)size);
    return p->real_file->pMethods->xTruncate(p->real_file, size);
}

/* Sync file */
static int distsqlite_sync(sqlite3_file *pFile, int flags) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    printf("[DistSQLite VFS] Sync: %s (flags=%d)\n", 
           p->filename ? p->filename : "<memory>", flags);
    
    /* This is a critical replication point - data should be durable */
    if (p->filename && strstr(p->filename, "-wal")) {
        printf("[DistSQLite VFS] WAL sync - replication checkpoint\n");
    }
    
    return p->real_file->pMethods->xSync(p->real_file, flags);
}

/* Get file size */
static int distsqlite_filesize(sqlite3_file *pFile, sqlite3_int64 *pSize) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    return p->real_file->pMethods->xFileSize(p->real_file, pSize);
}

/* Delete file */
static int distsqlite_delete(sqlite3_vfs *vfs, const char *zName, int syncDir) {
    printf("[DistSQLite VFS] Delete: %s\n", zName);
    return original_vfs->xDelete(original_vfs, zName, syncDir);
}

/* Test for access permissions */
static int distsqlite_access(sqlite3_vfs *vfs, const char *zName, int flags, int *pResOut) {
    return original_vfs->xAccess(original_vfs, zName, flags, pResOut);
}

/* Full pathname */
static int distsqlite_fullpathname(sqlite3_vfs *vfs, const char *zName, int nOut, char *zOut) {
    return original_vfs->xFullPathname(original_vfs, zName, nOut, zOut);
}

/* Random number generator */
static int distsqlite_randomness(sqlite3_vfs *vfs, int nByte, char *zOut) {
    return original_vfs->xRandomness(original_vfs, nByte, zOut);
}

/* Sleep */
static int distsqlite_sleep(sqlite3_vfs *vfs, int microseconds) {
    return original_vfs->xSleep(original_vfs, microseconds);
}

/* Current time */
static int distsqlite_currenttime(sqlite3_vfs *vfs, double *pTimeOut) {
    return original_vfs->xCurrentTime(original_vfs, pTimeOut);
}

/* DistSQLite VFS definition */
static sqlite3_vfs distsqlite_vfs = {
    2,                          /* iVersion */
    0,                          /* szOsFile (set at runtime) */
    1024,                       /* mxPathname */
    NULL,                       /* pNext */
    "distsqlite",              /* zName */
    NULL,                       /* pAppData */
    distsqlite_open,           /* xOpen */
    distsqlite_delete,         /* xDelete */
    distsqlite_access,         /* xAccess */
    distsqlite_fullpathname,   /* xFullPathname */
    NULL,                       /* xDlOpen */
    NULL,                       /* xDlError */
    NULL,                       /* xDlSym */
    NULL,                       /* xDlClose */
    distsqlite_randomness,     /* xRandomness */
    distsqlite_sleep,          /* xSleep */
    distsqlite_currenttime,    /* xCurrentTime */
    NULL,                       /* xGetLastError */
    NULL                        /* xCurrentTimeInt64 */
};

/* Initialize and register VFS */
int distsqlite_vfs_init(void) {
    printf("[DistSQLite] Initializing pure C VFS...\n");
    
    /* Get the default VFS */
    original_vfs = sqlite3_vfs_find(NULL);
    if (!original_vfs) {
        fprintf(stderr, "[DistSQLite] Failed to find default VFS\n");
        return -1;
    }
    
    /* Set the size of our file structure */
    distsqlite_vfs.szOsFile = sizeof(distsqlite_file);
    
    /* Register our VFS */
    int rc = sqlite3_vfs_register(&distsqlite_vfs, 1); /* Make it default */
    if (rc != SQLITE_OK) {
        fprintf(stderr, "[DistSQLite] Failed to register VFS: %d\n", rc);
        return -1;
    }
    
    printf("[DistSQLite] VFS registered successfully as default\n");
    return 0;
}

/* Constructor - called when library is loaded */
__attribute__((constructor)) void init_vfs(void) {
    distsqlite_vfs_init();
}