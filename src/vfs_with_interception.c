/* DistSQLite VFS with SQLite open interception */

#define _GNU_SOURCE
#include <sqlite3.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <dlfcn.h>

/* Original VFS we're wrapping */
static sqlite3_vfs *original_vfs = NULL;
static int vfs_initialized = 0;

/* Replication queue (simplified) */
typedef struct write_event {
    char filename[256];
    sqlite3_int64 offset;
    int size;
    unsigned char data[4096]; /* First 4KB of write */
    struct write_event *next;
} write_event_t;

static write_event_t *write_queue_head = NULL;
static write_event_t *write_queue_tail = NULL;
static int write_count = 0;

/* DistSQLite VFS structure */
typedef struct distsqlite_file {
    sqlite3_file base;
    sqlite3_file *real_file;
    char *filename;
} distsqlite_file;

/* Forward declarations for VFS methods */
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

/* Queue a write for replication */
static void queue_write(const char *filename, sqlite3_int64 offset, const void *data, int size) {
    write_event_t *event = malloc(sizeof(write_event_t));
    if (!event) return;
    
    strncpy(event->filename, filename ? filename : "<memory>", sizeof(event->filename) - 1);
    event->offset = offset;
    event->size = size;
    
    /* Copy first 4KB of data */
    int copy_size = size < 4096 ? size : 4096;
    memcpy(event->data, data, copy_size);
    event->next = NULL;
    
    /* Add to queue */
    if (write_queue_tail) {
        write_queue_tail->next = event;
    } else {
        write_queue_head = event;
    }
    write_queue_tail = event;
    write_count++;
    
    printf("[DistSQLite] Queued write #%d: %s offset=%lld size=%d\n", 
           write_count, event->filename, (long long)offset, size);
}

/* VFS Implementation */

static int distsqlite_open(sqlite3_vfs *vfs, const char *zName, sqlite3_file *pFile, 
                          int flags, int *pOutFlags) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    
    printf("[DistSQLite VFS] Opening: %s\n", zName ? zName : "<memory>");
    
    /* Allocate space for the real file */
    p->real_file = (sqlite3_file*)malloc(original_vfs->szOsFile);
    if (!p->real_file) return SQLITE_NOMEM;
    
    /* Store filename */
    p->filename = zName ? strdup(zName) : NULL;
    
    /* Open the real file */
    int rc = original_vfs->xOpen(original_vfs, zName, p->real_file, flags, pOutFlags);
    if (rc == SQLITE_OK) {
        p->base.pMethods = &distsqlite_io_methods;
    } else {
        free(p->real_file);
        if (p->filename) free(p->filename);
    }
    
    return rc;
}

static int distsqlite_close(sqlite3_file *pFile) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    
    printf("[DistSQLite VFS] Closing: %s\n", p->filename ? p->filename : "<memory>");
    
    int rc = p->real_file->pMethods->xClose(p->real_file);
    free(p->real_file);
    if (p->filename) free(p->filename);
    
    return rc;
}

static int distsqlite_read(sqlite3_file *pFile, void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    return p->real_file->pMethods->xRead(p->real_file, zBuf, iAmt, iOfst);
}

static int distsqlite_write(sqlite3_file *pFile, const void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    
    /* Queue write for replication */
    queue_write(p->filename, iOfst, zBuf, iAmt);
    
    /* Pass through to real file */
    return p->real_file->pMethods->xWrite(p->real_file, zBuf, iAmt, iOfst);
}

static int distsqlite_truncate(sqlite3_file *pFile, sqlite3_int64 size) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    printf("[DistSQLite VFS] Truncate: %s to %lld\n", 
           p->filename ? p->filename : "<memory>", (long long)size);
    return p->real_file->pMethods->xTruncate(p->real_file, size);
}

static int distsqlite_sync(sqlite3_file *pFile, int flags) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    printf("[DistSQLite VFS] Sync: %s (pending writes: %d)\n", 
           p->filename ? p->filename : "<memory>", write_count);
    
    /* This is where we'd send writes to replicas */
    if (write_count > 0) {
        printf("[DistSQLite] Would replicate %d writes to other nodes\n", write_count);
        /* TODO: Actually send to replicas */
    }
    
    return p->real_file->pMethods->xSync(p->real_file, flags);
}

static int distsqlite_filesize(sqlite3_file *pFile, sqlite3_int64 *pSize) {
    distsqlite_file *p = (distsqlite_file*)pFile;
    return p->real_file->pMethods->xFileSize(p->real_file, pSize);
}

/* VFS wrapper methods */
static int distsqlite_delete(sqlite3_vfs *vfs, const char *zName, int syncDir) {
    printf("[DistSQLite VFS] Delete: %s\n", zName);
    return original_vfs->xDelete(original_vfs, zName, syncDir);
}

static int distsqlite_access(sqlite3_vfs *vfs, const char *zName, int flags, int *pResOut) {
    return original_vfs->xAccess(original_vfs, zName, flags, pResOut);
}

static int distsqlite_fullpathname(sqlite3_vfs *vfs, const char *zName, int nOut, char *zOut) {
    return original_vfs->xFullPathname(original_vfs, zName, nOut, zOut);
}

static int distsqlite_randomness(sqlite3_vfs *vfs, int nByte, char *zOut) {
    return original_vfs->xRandomness(original_vfs, nByte, zOut);
}

static int distsqlite_sleep(sqlite3_vfs *vfs, int microseconds) {
    return original_vfs->xSleep(original_vfs, microseconds);
}

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

/* Initialize VFS */
static void init_vfs(void) {
    if (vfs_initialized) return;
    vfs_initialized = 1;
    
    printf("[DistSQLite] Initializing VFS with replication...\n");
    
    /* Get the default VFS */
    original_vfs = sqlite3_vfs_find(NULL);
    if (!original_vfs) {
        fprintf(stderr, "[DistSQLite] Failed to find default VFS\n");
        return;
    }
    
    /* Set our file size */
    distsqlite_vfs.szOsFile = sizeof(distsqlite_file);
    
    /* Register our VFS */
    int rc = sqlite3_vfs_register(&distsqlite_vfs, 1); /* Make it default */
    if (rc != SQLITE_OK) {
        fprintf(stderr, "[DistSQLite] Failed to register VFS: %d\n", rc);
        return;
    }
    
    printf("[DistSQLite] VFS registered successfully\n");
}

/* Intercept sqlite3_open functions */
static int (*real_sqlite3_open)(const char*, sqlite3**) = NULL;
static int (*real_sqlite3_open_v2)(const char*, sqlite3**, int, const char*) = NULL;

int sqlite3_open(const char *filename, sqlite3 **ppDb) {
    init_vfs();
    
    if (!real_sqlite3_open) {
        real_sqlite3_open = dlsym(RTLD_NEXT, "sqlite3_open");
    }
    
    printf("[DistSQLite] Intercepted sqlite3_open(%s)\n", filename ? filename : ":memory:");
    return real_sqlite3_open(filename, ppDb);
}

int sqlite3_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs) {
    init_vfs();
    
    if (!real_sqlite3_open_v2) {
        real_sqlite3_open_v2 = dlsym(RTLD_NEXT, "sqlite3_open_v2");
    }
    
    printf("[DistSQLite] Intercepted sqlite3_open_v2(%s, flags=0x%x, vfs=%s)\n", 
           filename ? filename : ":memory:", flags, zVfs ? zVfs : "default");
    
    /* Force our VFS */
    return real_sqlite3_open_v2(filename, ppDb, flags, "distsqlite");
}

/* Constructor */
__attribute__((constructor)) void distsqlite_init(void) {
    printf("[DistSQLite] Library loaded - ready to intercept SQLite\n");
}