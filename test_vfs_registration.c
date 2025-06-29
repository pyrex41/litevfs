#include <stdio.h>
#include <stdlib.h>
#include <dlfcn.h>
#include <sqlite3.h>

int main() {
    printf("=== Testing DistCore VFS Registration ===\n");
    
    /* Load the distcore library */
    void* handle = dlopen("./build/libdistcore.so", RTLD_NOW | RTLD_GLOBAL);
    if (!handle) {
        fprintf(stderr, "Failed to load libdistcore.so: %s\n", dlerror());
        return 1;
    }
    printf("✓ Loaded libdistcore.so\n");
    
    /* Get the init function */
    int (*distcore_init)(void) = dlsym(handle, "distcore_init");
    if (!distcore_init) {
        fprintf(stderr, "Failed to find distcore_init: %s\n", dlerror());
        dlclose(handle);
        return 1;
    }
    printf("✓ Found distcore_init function\n");
    
    /* Initialize the library (this should register the VFS) */
    if (distcore_init() != 0) {
        fprintf(stderr, "Failed to initialize distcore\n");
        dlclose(handle);
        return 1;
    }
    printf("✓ Initialized distcore (VFS should be registered)\n");
    
    /* Check if the VFS is registered */
    sqlite3_vfs* vfs = sqlite3_vfs_find("distcore");
    if (vfs) {
        printf("✓ Found 'distcore' VFS!\n");
        printf("  VFS name: %s\n", vfs->zName);
        printf("  VFS version: %d\n", vfs->iVersion);
        printf("  Max pathname: %d\n", vfs->mxPathname);
    } else {
        fprintf(stderr, "✗ 'distcore' VFS not found\n");
        dlclose(handle);
        return 1;
    }
    
    /* Test opening a database with the VFS */
    printf("\nTesting database operations with distcore VFS...\n");
    sqlite3* db;
    int rc = sqlite3_open_v2("test_vfs.db", &db, 
                             SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE,
                             "distcore");
    if (rc != SQLITE_OK) {
        fprintf(stderr, "Failed to open database: %s\n", sqlite3_errmsg(db));
        sqlite3_close(db);
        dlclose(handle);
        return 1;
    }
    printf("✓ Opened database with distcore VFS\n");
    
    /* Create a table */
    rc = sqlite3_exec(db, "CREATE TABLE IF NOT EXISTS test (id INTEGER PRIMARY KEY, data TEXT)", 
                      NULL, NULL, NULL);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "Failed to create table: %s\n", sqlite3_errmsg(db));
        sqlite3_close(db);
        dlclose(handle);
        return 1;
    }
    printf("✓ Created table\n");
    
    /* Insert data */
    rc = sqlite3_exec(db, "INSERT INTO test (data) VALUES ('VFS test data')", 
                      NULL, NULL, NULL);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "Failed to insert data: %s\n", sqlite3_errmsg(db));
        sqlite3_close(db);
        dlclose(handle);
        return 1;
    }
    printf("✓ Inserted data\n");
    
    /* Query data */
    sqlite3_stmt* stmt;
    rc = sqlite3_prepare_v2(db, "SELECT data FROM test WHERE id = 1", -1, &stmt, NULL);
    if (rc == SQLITE_OK && sqlite3_step(stmt) == SQLITE_ROW) {
        const char* data = (const char*)sqlite3_column_text(stmt, 0);
        printf("✓ Retrieved data: %s\n", data);
    }
    sqlite3_finalize(stmt);
    
    /* Close database */
    sqlite3_close(db);
    printf("✓ Closed database\n");
    
    /* Clean up */
    dlclose(handle);
    printf("\n=== All VFS tests passed! ===\n");
    
    return 0;
}