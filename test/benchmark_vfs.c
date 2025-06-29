/* DistCore VFS Performance Benchmark */

#include <stdio.h>
#include <stdlib.h>
#include <time.h>
#include <sqlite3.h>
#include <string.h>
#include <sys/time.h>

#define NUM_ITERATIONS 10000
#define NUM_ROWS 1000

/* Get current time in microseconds */
static double get_time_us() {
    struct timeval tv;
    gettimeofday(&tv, NULL);
    return tv.tv_sec * 1000000.0 + tv.tv_usec;
}

/* Benchmark function */
static void benchmark_vfs(const char* vfs_name) {
    sqlite3* db;
    char* err_msg = NULL;
    int rc;
    
    printf("\n=== Benchmarking VFS: %s ===\n", vfs_name ? vfs_name : "default");
    
    /* Open database */
    rc = sqlite3_open_v2(":memory:", &db, 
                         SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE,
                         vfs_name);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "Cannot open database: %s\n", sqlite3_errmsg(db));
        return;
    }
    
    /* Create table */
    rc = sqlite3_exec(db, 
        "CREATE TABLE bench (id INTEGER PRIMARY KEY, data TEXT, value REAL)",
        NULL, NULL, &err_msg);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "SQL error: %s\n", err_msg);
        sqlite3_free(err_msg);
        sqlite3_close(db);
        return;
    }
    
    /* Benchmark inserts */
    double start = get_time_us();
    
    sqlite3_exec(db, "BEGIN", NULL, NULL, NULL);
    for (int i = 0; i < NUM_ITERATIONS; i++) {
        char sql[256];
        snprintf(sql, sizeof(sql), 
                 "INSERT INTO bench (data, value) VALUES ('test%d', %f)",
                 i, (double)i * 3.14159);
        sqlite3_exec(db, sql, NULL, NULL, NULL);
    }
    sqlite3_exec(db, "COMMIT", NULL, NULL, NULL);
    
    double insert_time = get_time_us() - start;
    printf("Insert %d rows: %.2f ms (%.2f rows/sec)\n", 
           NUM_ITERATIONS, insert_time / 1000.0,
           NUM_ITERATIONS / (insert_time / 1000000.0));
    
    /* Benchmark selects */
    start = get_time_us();
    
    for (int i = 0; i < NUM_ROWS; i++) {
        sqlite3_stmt* stmt;
        rc = sqlite3_prepare_v2(db, 
            "SELECT * FROM bench WHERE id = ?", -1, &stmt, NULL);
        sqlite3_bind_int(stmt, 1, rand() % NUM_ITERATIONS);
        sqlite3_step(stmt);
        sqlite3_finalize(stmt);
    }
    
    double select_time = get_time_us() - start;
    printf("Select %d rows: %.2f ms (%.2f queries/sec)\n",
           NUM_ROWS, select_time / 1000.0,
           NUM_ROWS / (select_time / 1000000.0));
    
    /* Benchmark updates */
    start = get_time_us();
    
    sqlite3_exec(db, "BEGIN", NULL, NULL, NULL);
    for (int i = 0; i < NUM_ROWS; i++) {
        char sql[256];
        snprintf(sql, sizeof(sql),
                 "UPDATE bench SET value = value * 2 WHERE id = %d",
                 rand() % NUM_ITERATIONS);
        sqlite3_exec(db, sql, NULL, NULL, NULL);
    }
    sqlite3_exec(db, "COMMIT", NULL, NULL, NULL);
    
    double update_time = get_time_us() - start;
    printf("Update %d rows: %.2f ms (%.2f updates/sec)\n",
           NUM_ROWS, update_time / 1000.0,
           NUM_ROWS / (update_time / 1000000.0));
    
    /* Calculate total and throughput */
    double total_time = insert_time + select_time + update_time;
    double total_ops = NUM_ITERATIONS + NUM_ROWS * 2;
    printf("Total time: %.2f ms\n", total_time / 1000.0);
    printf("Total throughput: %.2f ops/sec\n", total_ops / (total_time / 1000000.0));
    
    sqlite3_close(db);
}

int main() {
    printf("DistCore VFS Performance Benchmark\n");
    printf("==================================\n");
    
    /* Seed random number generator */
    srand(time(NULL));
    
    /* Benchmark default VFS */
    benchmark_vfs(NULL);
    
    /* Check if distcore VFS is available */
    sqlite3_vfs* vfs = sqlite3_vfs_find("distcore");
    if (vfs) {
        benchmark_vfs("distcore");
        
        /* Compare performance */
        printf("\n=== Performance Impact ===\n");
        printf("Compare the results above to see VFS overhead\n");
    } else {
        printf("\n[INFO] DistCore VFS not found - load with LD_PRELOAD\n");
    }
    
    return 0;
}