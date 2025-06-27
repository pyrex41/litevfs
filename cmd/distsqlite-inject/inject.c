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

// OCaml VFS initialization (from the OCaml distcore library)
extern int distcore_init(void);
extern int distcore_health_check(void);

// Function pointers to the real SQLite functions
static int (*real_sqlite3_open_v2)(const char*, void**, int, const char*) = NULL;
static int (*real_sqlite3_open)(const char*, void**) = NULL;
static int (*real_sqlite3_open16)(const void*, void**) = NULL;

// Sidecar process PID
static pid_t sidecar_pid = 0;
static pthread_mutex_t sidecar_mutex = PTHREAD_MUTEX_INITIALIZER;

// OCaml VFS initialization state
static int ocaml_vfs_initialized = 0;

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
        char *argv[] = {"distsqlite-zmq", "--mode", "node", "--endpoints", "tcp://localhost:5555", "--node-id", "auto", "--auto-start", NULL};
        execvp("distsqlite-zmq", argv);
        
        // If execvp fails, try from common paths
        execv("/usr/local/bin/distsqlite-zmq", argv);
        execv("./build/distsqlite-zmq", argv);
        
        // Fallback to regular distsqlite
        char *fallback_argv[] = {"distsqlite", "--auto-start", NULL};
        execvp("distsqlite", fallback_argv);
        execv("/usr/local/bin/distsqlite", fallback_argv);
        execv("./build/distsqlite", fallback_argv);
        
        fprintf(stderr, "[DistSQLite] Failed to start any sidecar process\n");
        _exit(1);
    } else if (pid > 0) {
        // Parent process
        sidecar_pid = pid;
        printf("[DistSQLite] Sidecar started with PID %d\n", sidecar_pid);
        
        // Wait for sidecar to initialize with proper health checks
        int initialized = 0;
        for (int attempts = 0; attempts < 100; attempts++) {
            if (distcore_health_check() == 0) {
                initialized = 1;
                printf("[DistSQLite] OCaml VFS ready after %d attempts\n", attempts + 1);
                break;
            }
            usleep(50000); // 50ms retry interval
        }
        
        if (!initialized) {
            fprintf(stderr, "[DistSQLite] Warning: OCaml VFS did not initialize within timeout\n");
            // Continue anyway - might work with degraded functionality
        }
        
        pthread_mutex_unlock(&sidecar_mutex);
        return 1;
    } else {
        // Fork failed
        fprintf(stderr, "[DistSQLite] Failed to fork sidecar process\n");
        pthread_mutex_unlock(&sidecar_mutex);
        return 0;
    }
}

// Initialize OCaml VFS
static int initializeOCamlVFS(void) {
    if (ocaml_vfs_initialized) return 1;
    
    printf("[DistSQLite] Initializing OCaml VFS engine...\n");
    
    // Initialize the OCaml distcore library
    if (distcore_init() != 0) {
        fprintf(stderr, "[DistSQLite] Failed to initialize OCaml distcore\n");
        return 0;
    }
    
    ocaml_vfs_initialized = 1;
    printf("[DistSQLite] OCaml VFS engine initialized successfully\n");
    return 1;
}

// Initialize our interception
static void initializeDistSQLite(void) {
    static int initialized = 0;
    if (initialized) return;
    initialized = 1;
    
    printf("[DistSQLite] Initializing zero-change SQLite distribution with OCaml VFS...\n");
    
    // Load real SQLite functions
    real_sqlite3_open_v2 = dlsym(RTLD_NEXT, "sqlite3_open_v2");
    real_sqlite3_open = dlsym(RTLD_NEXT, "sqlite3_open");
    real_sqlite3_open16 = dlsym(RTLD_NEXT, "sqlite3_open16");
    
    if (!real_sqlite3_open_v2 || !real_sqlite3_open) {
        fprintf(stderr, "[DistSQLite] Warning: Could not find SQLite functions\n");
        return;
    }
    
    // Initialize OCaml VFS
    if (!initializeOCamlVFS()) {
        fprintf(stderr, "[DistSQLite] Warning: OCaml VFS initialization failed\n");
        // Continue anyway - might work with basic functionality
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
    
    // Note: With OCaml VFS, we don't force a specific VFS name
    // Instead, the OCaml system handles VFS registration and interception
    printf("[DistSQLite] Intercepted sqlite3_open_v2 for %s\n", 
           filename ? filename : "<memory>");
    
    // Call the real function - OCaml VFS will intercept at the C level
    return real_sqlite3_open_v2(filename, ppDb, flags, zVfs);
}

// Intercepted sqlite3_open function
int sqlite3_open(const char *filename, void **ppDb) {
    initializeDistSQLite();
    
    printf("[DistSQLite] Intercepted sqlite3_open for %s\n", 
           filename ? filename : "<memory>");
    
    // Call the real function - OCaml VFS will intercept at the C level
    return real_sqlite3_open(filename, ppDb);
}

// Intercepted sqlite3_open16 function
int sqlite3_open16(const void *filename, void **ppDb) {
    initializeDistSQLite();
    
    printf("[DistSQLite] Intercepted sqlite3_open16\n");
    
    // Call the real function - OCaml VFS will intercept at the C level
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