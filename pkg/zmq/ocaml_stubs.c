/* OCaml C stubs for ZMQ integration */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <caml/mlvalues.h>
#include <caml/memory.h>
#include <caml/alloc.h>
#include <caml/custom.h>
#include <caml/callback.h>
#include <caml/threads.h>

/* Global OCaml initialization flag */
static int ocaml_initialized = 0;

/* Initialize OCaml runtime (thread-safe) */
int zmq_ocaml_init(void) {
    if (!ocaml_initialized) {
        caml_acquire_runtime_system();
        char *argv[] = {"distcore", NULL};
        caml_startup(argv);
        ocaml_initialized = 1;
        caml_release_runtime_system();
        return 1;
    }
    return 1;
}

/* Create OCaml write event from C data */
value zmq_create_write_event(const char* path, int64_t offset, const char* data, 
                            int len, int is_wal, int64_t fence_token) {
    CAMLparam0();
    CAMLlocal4(event, path_val, data_val, bigarray);
    
    /* Create OCaml string for path */
    path_val = caml_copy_string(path);
    
    /* Create bigarray for data (zero-copy) */
    intnat dims[1] = {len};
    bigarray = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, NULL, dims);
    memcpy(Caml_ba_data_val(bigarray), data, len);
    
    /* Create record: {path; offset; data; is_wal; fence_token} */
    event = caml_alloc_tuple(5);
    Store_field(event, 0, path_val);
    Store_field(event, 1, caml_copy_int64(offset));
    Store_field(event, 2, bigarray);
    Store_field(event, 3, Val_bool(is_wal));
    Store_field(event, 4, caml_copy_int64(fence_token));
    
    CAMLreturn(event);
}

/* Convert OCaml strategy result to C string */
int zmq_extract_strategy(value strategy_val, char* buffer, int buffer_size) {
    CAMLparam1(strategy_val);
    
    if (Is_block(strategy_val) && Tag_val(strategy_val) == 0) {
        /* Assume it's a variant with string data */
        value str_val = Field(strategy_val, 0);
        if (Is_block(str_val) && Tag_val(str_val) == String_tag) {
            const char* str = String_val(str_val);
            int len = strlen(str);
            if (len < buffer_size) {
                strcpy(buffer, str);
                CAMLreturnT(int, 1);
            }
        }
    }
    
    CAMLreturnT(int, 0);
}

/* Thread-safe OCaml callback wrapper */
typedef struct {
    const char* callback_name;
    value arg1;
    value arg2;
    value result;
    int success;
} ocaml_callback_data;

void ocaml_callback_wrapper(void* data) {
    ocaml_callback_data* cb_data = (ocaml_callback_data*)data;
    
    CAMLparam0();
    CAMLlocal1(result);
    
    const value* callback = caml_named_value(cb_data->callback_name);
    if (callback != NULL) {
        if (cb_data->arg2 == Val_unit) {
            result = caml_callback(*callback, cb_data->arg1);
        } else {
            result = caml_callback2(*callback, cb_data->arg1, cb_data->arg2);
        }
        cb_data->result = result;
        cb_data->success = 1;
    } else {
        cb_data->success = 0;
    }
    
    CAMLreturn0;
}

/* Thread-safe enqueue operation */
int zmq_ocaml_enqueue(void* queue_ptr, const char* path, int64_t offset, 
                     const char* data, int len, int is_wal, int64_t fence_token) {
    if (!ocaml_initialized) {
        return 0;
    }
    
    caml_acquire_runtime_system();
    
    CAMLparam0();
    CAMLlocal1(event);
    
    /* Create write event */
    event = zmq_create_write_event(path, offset, data, len, is_wal, fence_token);
    
    /* Call OCaml enqueue function */
    ocaml_callback_data cb_data;
    cb_data.callback_name = "queue_enqueue";
    cb_data.arg1 = (value)queue_ptr;
    cb_data.arg2 = event;
    cb_data.success = 0;
    
    ocaml_callback_wrapper(&cb_data);
    
    int success = cb_data.success;
    
    CAMLreturn0;
    caml_release_runtime_system();
    
    return success;
}

/* Thread-safe dequeue operation */
int zmq_ocaml_dequeue(void* queue_ptr, int max_size, void** events_out, int* count_out) {
    if (!ocaml_initialized) {
        return 0;
    }
    
    caml_acquire_runtime_system();
    
    CAMLparam0();
    CAMLlocal1(batch);
    
    ocaml_callback_data cb_data;
    cb_data.callback_name = "queue_dequeue_batch";
    cb_data.arg1 = (value)queue_ptr;
    cb_data.arg2 = Val_int(max_size);
    cb_data.success = 0;
    
    ocaml_callback_wrapper(&cb_data);
    
    if (cb_data.success) {
        batch = cb_data.result;
        *count_out = Wosize_val(batch);
        *events_out = (void*)batch; /* Keep as OCaml value */
        caml_release_runtime_system();
        CAMLreturnT(int, 1);
    }
    
    *count_out = 0;
    *events_out = NULL;
    
    CAMLreturn0;
    caml_release_runtime_system();
    return 0;
}

/* Thread-safe strategy computation */
int zmq_ocaml_compute_strategy(void* batch_ptr, char* strategy_out, int strategy_len) {
    if (!ocaml_initialized) {
        return 0;
    }
    
    caml_acquire_runtime_system();
    
    CAMLparam0();
    
    ocaml_callback_data cb_data;
    cb_data.callback_name = "compute_strategy";
    cb_data.arg1 = (value)batch_ptr;
    cb_data.arg2 = Val_unit;
    cb_data.success = 0;
    
    ocaml_callback_wrapper(&cb_data);
    
    int result = 0;
    if (cb_data.success) {
        result = zmq_extract_strategy(cb_data.result, strategy_out, strategy_len);
    }
    
    CAMLreturn0;
    caml_release_runtime_system();
    
    return result;
}

/* Thread-safe queue creation */
void* zmq_ocaml_create_queue(int capacity) {
    if (!ocaml_initialized) {
        return NULL;
    }
    
    caml_acquire_runtime_system();
    
    CAMLparam0();
    
    ocaml_callback_data cb_data;
    cb_data.callback_name = "queue_create";
    cb_data.arg1 = Val_int(capacity);
    cb_data.arg2 = Val_unit;
    cb_data.success = 0;
    
    ocaml_callback_wrapper(&cb_data);
    
    void* queue = NULL;
    if (cb_data.success) {
        queue = (void*)cb_data.result;
        /* In production, register as global root to prevent GC */
    }
    
    CAMLreturn0;
    caml_release_runtime_system();
    
    return queue;
}

/* Performance monitoring integration */
typedef struct {
    int64_t queue_operations;
    int64_t bytes_processed;
    int64_t strategy_computations;
    double avg_latency_ms;
} zmq_perf_stats;

int zmq_ocaml_get_perf_stats(zmq_perf_stats* stats) {
    if (!ocaml_initialized || !stats) {
        return 0;
    }
    
    caml_acquire_runtime_system();
    
    CAMLparam0();
    CAMLlocal1(stats_val);
    
    const value* stats_fn = caml_named_value("get_performance_stats");
    if (stats_fn != NULL) {
        stats_val = caml_callback(*stats_fn, Val_unit);
        
        /* Extract stats from OCaml record/list */
        /* This would need proper field extraction based on OCaml implementation */
        stats->queue_operations = 0;
        stats->bytes_processed = 0;
        stats->strategy_computations = 0;
        stats->avg_latency_ms = 0.0;
        
        CAMLreturn0;
        caml_release_runtime_system();
        return 1;
    }
    
    CAMLreturn0;
    caml_release_runtime_system();
    return 0;
}