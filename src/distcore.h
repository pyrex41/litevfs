/* DistSQLite Core - C header file for Go integration */

#ifndef DISTCORE_H
#define DISTCORE_H

#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Write event structure matching Go and OCaml */
typedef struct {
    char* path;
    int64_t offset;
    int32_t length;
    char* data;
    int is_wal;
    int64_t fence_token;
} write_event_t;

/* Replication strategy enum */
typedef enum {
    STRATEGY_TXLOG = 0,
    STRATEGY_BULK_SYNC = 1,
    STRATEGY_HYBRID = 2,
    STRATEGY_PAGE_DIFF = 3
} replication_strategy_t;

/* Initialization and cleanup */
void distcore_init(void);
void distcore_cleanup(void);

/* Lock-free write queue operations */
void* distcore_queue_create(int capacity);
int distcore_queue_enqueue(void* queue, const write_event_t* event);
int distcore_queue_dequeue_batch(void* queue, write_event_t* events, int max_events);
int distcore_queue_pending_count(void* queue);

/* Replication strategy and encoding */
replication_strategy_t distcore_compute_strategy(const write_event_t* events, int count);
int distcore_encode_write_batch(const write_event_t* events, int count, 
                               char* output_buffer, int buffer_size);

/* Binary diff computation */
int distcore_compute_diff(const char* old_db, int old_size,
                         const char* new_db, int new_size,
                         int page_size, char* diff_output, int output_size);

/* Performance monitoring (optional) */
typedef struct {
    int64_t queue_enqueues;
    int64_t queue_dequeues;
    int64_t bytes_processed;
    int64_t diff_operations;
    int64_t replication_operations;
    int64_t state_transitions;
} distcore_stats_t;

int distcore_get_stats(distcore_stats_t* stats);
void distcore_reset_stats(void);

#ifdef __cplusplus
}
#endif

#endif /* DISTCORE_H */