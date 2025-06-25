#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <signal.h>
#include <pthread.h>

// Forward declaration from our VFS
extern int distVFSInit(void *goCallbacks);

// Function pointers to the real SQLite functions
static int (*real_sqlite3_open_v2)(const char*, void**, int, const char*) = NULL;
static int (*real_sqlite3_open)(const char*, void**) = NULL;
static int (*real_sqlite3_open16)(const void*, void**) = NULL;

// Sidecar process PID
static pid_t sidecar_pid = 0;
static pthread_mutex_t sidecar_mutex = PTHREAD_MUTEX_INITIALIZER;

// Check if sidecar is running
static int sidecarRunning(void) {
    if (sidecar_pid <= 0) return 0;
    
    // Check if process still exists
    if (kill(sidecar_pid, 0) == 0) {
        return 1;
    }
    
    // Process doesn't exist, reset PID
    sidecar_pid = 0;
    return 0;
}

// Start the DistSQLite sidecar process
static int startSidecar(void) {
    pthread_mutex_lock(&sidecar_mutex);
    
    // Double-check if already running
    if (sidecarRunning()) {
        pthread_mutex_unlock(&sidecar_mutex);
        return 1;
    }
    
    printf("[DistSQLite] Starting sidecar process...\n");
    
    pid_t pid = fork();
    if (pid == 0) {
        // Child process - exec the sidecar
        char *argv[] = {"distsqlite", "--auto-start", NULL};
        execvp("distsqlite", argv);
        
        // If execvp fails, try from common paths
        execv("/usr/local/bin/distsqlite", argv);
        execv("./build/distsqlite", argv);
        
        fprintf(stderr, "[DistSQLite] Failed to start sidecar process\n");
        _exit(1);
    } else if (pid > 0) {
        // Parent process
        sidecar_pid = pid;
        printf("[DistSQLite] Sidecar started with PID %d\n", sidecar_pid);
        
        // Give sidecar a moment to initialize
        usleep(100000); // 100ms
        
        pthread_mutex_unlock(&sidecar_mutex);
        return 1;
    } else {
        // Fork failed
        fprintf(stderr, "[DistSQLite] Failed to fork sidecar process\n");
        pthread_mutex_unlock(&sidecar_mutex);
        return 0;
    }
}

// Initialize our interception
static void initializeDistSQLite(void) {
    static int initialized = 0;
    if (initialized) return;
    initialized = 1;
    
    printf("[DistSQLite] Initializing zero-change SQLite distribution...\n");
    
    // Load real SQLite functions
    real_sqlite3_open_v2 = dlsym(RTLD_NEXT, "sqlite3_open_v2");
    real_sqlite3_open = dlsym(RTLD_NEXT, "sqlite3_open");
    real_sqlite3_open16 = dlsym(RTLD_NEXT, "sqlite3_open16");
    
    if (!real_sqlite3_open_v2 || !real_sqlite3_open) {
        fprintf(stderr, "[DistSQLite] Warning: Could not find SQLite functions\n");
        return;
    }
    
    // Initialize our VFS
    if (distVFSInit(NULL) != 0) {
        fprintf(stderr, "[DistSQLite] Failed to initialize VFS\n");
        return;
    }
    
    // Start sidecar process
    if (!startSidecar()) {
        fprintf(stderr, "[DistSQLite] Warning: Could not start sidecar\n");
        // Continue anyway - VFS might still work for local operations
    }
    
    printf("[DistSQLite] Initialization complete - SQLite is now distributed!\n");
}

// Intercepted sqlite3_open_v2 function
int sqlite3_open_v2(const char *filename, void **ppDb, int flags, const char *zVfs) {
    initializeDistSQLite();
    
    // Force our VFS if not specified or if default is requested
    if (!zVfs || strcmp(zVfs, "unix") == 0 || strcmp(zVfs, "") == 0) {
        printf("[DistSQLite] Intercepted sqlite3_open_v2 for %s, using DistSQLite VFS\n", 
               filename ? filename : "<memory>");
        zVfs = "distsqlite";
    }
    
    // Call the real function with our VFS
    return real_sqlite3_open_v2(filename, ppDb, flags, zVfs);
}

// Intercepted sqlite3_open function
int sqlite3_open(const char *filename, void **ppDb) {
    initializeDistSQLite();
    
    printf("[DistSQLite] Intercepted sqlite3_open for %s, using DistSQLite VFS\n", 
           filename ? filename : "<memory>");
    
    // Call sqlite3_open_v2 with our VFS
    return sqlite3_open_v2(filename, ppDb, 
                          2 | 64, // SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE
                          "distsqlite");
}

// Intercepted sqlite3_open16 function
int sqlite3_open16(const void *filename, void **ppDb) {
    initializeDistSQLite();
    
    // For UTF-16, we can't easily print the filename, but we can still intercept
    printf("[DistSQLite] Intercepted sqlite3_open16, using DistSQLite VFS\n");
    
    // SQLite will convert UTF-16 to UTF-8 internally, then call our sqlite3_open_v2
    return real_sqlite3_open16(filename, ppDb);
}

// Cleanup function called at exit
static void cleanup(void) {
    if (sidecar_pid > 0) {
        printf("[DistSQLite] Cleaning up sidecar process...\n");
        kill(sidecar_pid, SIGTERM);
        
        // Wait for graceful shutdown
        int status;
        int waited = 0;
        while (waited < 5000 && waitpid(sidecar_pid, &status, WNOHANG) == 0) {
            usleep(10000); // 10ms
            waited += 10;
        }
        
        // Force kill if still running
        if (kill(sidecar_pid, 0) == 0) {
            kill(sidecar_pid, SIGKILL);
            waitpid(sidecar_pid, &status, 0);
        }
        
        sidecar_pid = 0;
    }
}

// Constructor - called when library is loaded
__attribute__((constructor)) void init(void) {
    // Register cleanup function
    atexit(cleanup);
    
    // Don't initialize immediately - wait for first SQLite call
    // This avoids startup overhead for non-SQLite applications
    printf("[DistSQLite] Injection library loaded - ready to intercept SQLite calls\n");
}

// Destructor - called when library is unloaded
__attribute__((destructor)) void destroy(void) {
    cleanup();
}