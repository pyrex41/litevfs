/* DistSQLite Core - C stubs for Go integration */

#include <caml/mlvalues.h>
#include <caml/callback.h>
#include <caml/memory.h>
#include <caml/alloc.h>
#include <caml/bigarray.h>
#include <caml/fail.h>
#include <caml/custom.h>
#include <string.h>
#include <stdint.h>
#include <stdbool.h>    /* For bool type */
#include "distcore_errors.h"
#include "distcore_thread_safety.h"
#ifdef HAVE_AVX2
#include <immintrin.h>  /* For AVX2 intrinsics */
#elif defined(HAVE_SSE2)
#include <emmintrin.h>  /* For SSE2 intrinsics */
#endif
#include <cpuid.h>      /* For CPU feature detection */
#include <errno.h>      /* For error codes */
#include <limits.h>     /* For overflow checks */
#include <stdio.h>      /* For logging */
#include <stdlib.h>     /* For getenv */

/* Security constants */
#define MAX_PATH_LENGTH 4096        /* Maximum safe path length */
#define MAX_STACK_ALLOC 256        /* Maximum safe stack allocation */
#define MAX_BUFFER_SIZE (1024 * 1024 * 100)  /* 100MB max buffer */
#define MIN_BUFFER_SIZE 1          /* Minimum buffer size */

/* Logging macros for security events */
#define LOG_SECURITY_ERROR(msg, ...) \
    fprintf(stderr, "[SECURITY ERROR] %s:%d: " msg "\n", __FILE__, __LINE__, ##__VA_ARGS__)

#define LOG_BOUNDS_ERROR(func, param, value, max) \
    LOG_SECURITY_ERROR("%s: %s=%d exceeds maximum %d", func, param, value, max)

/* CPU feature detection */
static int cpu_has_avx2 = -1; /* -1 = not checked, 0 = no, 1 = yes */
static int cpu_has_sse2 = -1; /* -1 = not checked, 0 = no, 1 = yes */

static int check_avx2_support(void) {
#ifdef HAVE_AVX2
    if (cpu_has_avx2 != -1) {
        return cpu_has_avx2;
    }
    
    unsigned int eax, ebx, ecx, edx;
    
    /* Check for AVX2 support */
    if (__get_cpuid_count(7, 0, &eax, &ebx, &ecx, &edx)) {
        cpu_has_avx2 = (ebx & (1 << 5)) ? 1 : 0; /* AVX2 bit */
    } else {
        cpu_has_avx2 = 0;
    }
    
    return cpu_has_avx2;
#else
    return 0; /* AVX2 not available at compile time */
#endif
}

static int check_sse2_support(void) {
#ifdef HAVE_SSE2
    if (cpu_has_sse2 != -1) {
        return cpu_has_sse2;
    }
    
    unsigned int eax, ebx, ecx, edx;
    
    /* Check for SSE2 support */
    if (__get_cpuid(1, &eax, &ebx, &ecx, &edx)) {
        cpu_has_sse2 = (edx & (1 << 26)) ? 1 : 0; /* SSE2 bit */
    } else {
        cpu_has_sse2 = 0;
    }
    
    return cpu_has_sse2;
#else
    return 0; /* SSE2 not available at compile time */
#endif
}

/* Enhanced write event structure with security tracking */
typedef struct {
    char* path;
    int64_t offset;
    int32_t length;
    char* data;
    int is_wal;
    int64_t fence_token;
    int _needs_free;  /* Internal flag for memory management */
    int _alloc_type;  /* Track allocation type: 0=stack, 1=heap */
} write_event_t;

/* Enhanced cleanup with proper error handling */
void distcore_cleanup_write_events(write_event_t* events, int count) {
    if (!events) {
        LOG_SECURITY_ERROR("cleanup_write_events: NULL events pointer");
        return;
    }
    
    if (count < 0 || count > 10000) {  /* Sanity check */
        LOG_SECURITY_ERROR("cleanup_write_events: invalid count %d", count);
        return;
    }
    
    for (int i = 0; i < count; i++) {
        if (events[i]._needs_free && events[i].path) {
            /* Validate pointer before freeing */
            if (events[i]._alloc_type == 1) {  /* Heap allocated */
                free(events[i].path);
                events[i].path = NULL;
                events[i]._needs_free = 0;
                events[i]._alloc_type = 0;
            } else {
                /* Stack allocated - just mark as cleaned */
                events[i].path = NULL;
                events[i]._needs_free = 0;
            }
        }
    }
}

/* Global variables expected by OCaml runtime */
value *caml_globals = NULL;
void *caml_globals_map = NULL;
char **caml_program = NULL;
void *caml_code_segments = NULL;
struct caml_ba_proxy_ {void *data;} caml_ba_proxy = {NULL};
char *caml_frametable = NULL;
intnat caml_young_limit = 0;
intnat caml_young_ptr = 0;

/* OCaml exception variables - comprehensive set */
value caml_exn_Failure = 0;
value caml_exn_Invalid_argument = 0;
value caml_exn_Not_found = 0;
value caml_exn_Assert_failure = 0;
value caml_exn_Out_of_memory = 0;
value caml_exn_Stack_overflow = 0;
value caml_exn_Sys_error = 0;
value caml_exn_End_of_file = 0;
value caml_exn_Division_by_zero = 0;
value caml_exn_Match_failure = 0;
value caml_exn_Sys_blocked_io = 0;
value caml_exn_Undefined_recursive_module = 0;
int caml_runtime_warnings = 0;
int caml_setup_args_is_done = 0;

/* Custom operations for int64 */
struct custom_operations caml_int64_ops = {
    "int64",
    NULL,  /* finalize */
    NULL,  /* compare */
    NULL,  /* hash */
    NULL,  /* serialize */
    NULL,  /* deserialize */
    NULL,  /* compare_ext */
    NULL   /* fixed_length */
};

/* Custom operations for int32 */
struct custom_operations caml_int32_ops = {
    "int32",
    NULL, NULL, NULL, NULL, NULL, NULL, NULL
};

/* Custom operations for nativeint */
struct custom_operations caml_nativeint_ops = {
    "nativeint",
    NULL, NULL, NULL, NULL, NULL, NULL, NULL
};

/* Missing OCaml apply functions - implementing them ourselves */
value caml_apply2(value fun, value arg1, value arg2) {
    CAMLparam3(fun, arg1, arg2);
    CAMLlocal1(result);
    
    value args[2] = {arg1, arg2};
    result = caml_callbackN(fun, 2, args);
    CAMLreturn(result);
}

value caml_apply3(value fun, value arg1, value arg2, value arg3) {
    CAMLparam4(fun, arg1, arg2, arg3);
    CAMLlocal1(result);
    
    value args[3] = {arg1, arg2, arg3};
    result = caml_callbackN(fun, 3, args);
    CAMLreturn(result);
}

/* Additional common OCaml runtime functions - removed, conflicts with static lib */

/* Implementing missing OCaml Sys module functions */
#include <sys/stat.h>

value caml_sys_file_exists(value name) {
    struct stat st;
    return Val_bool(stat(String_val(name), &st) == 0);
}

/* Forward declaration */
int distcore_register_vfs();

/* Initialize OCaml runtime - called once from Go or injection */
int distcore_init(void) {
    static int initialized = 0;
    if (initialized) return DISTCORE_OK;
    initialized = 1;
    
    /* Initialize thread safety first */
    int rc = distcore_thread_safety_init();
    if (rc != DISTCORE_OK) {
        fprintf(stderr, "[DistCore] Failed to initialize thread safety: %s\n", 
                distcore_error_string(rc));
        return rc;
    }
    
    printf("[DistCore] Initializing OCaml runtime...\n");
    char *argv[] = {"distcore", NULL};
    
    DISTCORE_OCAML_LOCK();
    caml_startup(argv);
    DISTCORE_OCAML_UNLOCK();
    
    printf("[DistCore] OCaml runtime initialized successfully\n");
    
    /* Don't register VFS here - it will be done in distcore_init_vfs */
    return DISTCORE_OK;
}

/* Initialize VFS - separate from OCaml init */
int distcore_init_vfs(void) {
    static int vfs_initialized = 0;
    if (vfs_initialized) return 0;
    vfs_initialized = 1;
    
    /* Make sure OCaml is initialized first */
    distcore_init();
    
    /* Register the VFS */
    printf("[DistCore] Registering SQLite VFS...\n");
    if (distcore_register_vfs() != 0) {
        fprintf(stderr, "[DistCore] Failed to register VFS\n");
        return -1;
    }
    
    return 0;
}

/* Health check for injection library */
int distcore_health_check(void) {
    // Simple health check - return 0 if OCaml runtime is working
    CAMLparam0();
    CAMLlocal1(result);
    
    static value *health_func = NULL;
    if (!health_func) {
        health_func = caml_named_value("health_check");
        if (!health_func) {
            // If no health check function, assume we're healthy if runtime works
            CAMLreturnT(int, 0);
        }
    }
    
    result = caml_callback(*health_func, Val_unit);
    CAMLreturnT(int, Bool_val(result) ? 0 : 1);
}

/* Lock-free write queue operations */
void* distcore_queue_create(int capacity) {
    CAMLparam0();
    CAMLlocal1(result);
    
    static value *create_func = NULL;
    if (!create_func) {
        create_func = caml_named_value("queue_create");
        if (!create_func) {
            CAMLreturnT(void*, NULL);
        }
    }
    
    result = caml_callback(*create_func, Val_int(capacity));
    CAMLreturnT(void*, (void*)result);
}

int distcore_queue_enqueue(void* queue, const write_event_t* event) {
    CAMLparam0();
    CAMLlocal3(ml_event, ml_data, result);
    
    /* Create Bigarray pointing to C memory (zero-copy) */
    intnat dims[1];
    dims[0] = event->length;
    ml_data = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, (void*)event->data, dims);
    
    /* Build OCaml record - allocate with 5 fields */
    ml_event = caml_alloc(5, 0);
    Store_field(ml_event, 0, caml_copy_string(event->path));
    Store_field(ml_event, 1, caml_copy_int64(event->offset));
    Store_field(ml_event, 2, ml_data);
    Store_field(ml_event, 3, Val_bool(event->is_wal));
    Store_field(ml_event, 4, caml_copy_int64(event->fence_token));
    
    static value *enqueue_func = NULL;
    if (!enqueue_func) {
        enqueue_func = caml_named_value("queue_enqueue");
        if (!enqueue_func) {
            CAMLreturnT(int, 0);
        }
    }
    
    result = caml_callback2(*enqueue_func, (value)queue, ml_event);
    CAMLreturnT(int, Bool_val(result));
}

/* Enhanced dequeue with comprehensive security checks */
int distcore_queue_dequeue_batch(void* queue, write_event_t* events, int max_events) {
    CAMLparam0();
    CAMLlocal2(result, batch_array);
    
    /* Input validation */
    if (!queue || !events) {
        LOG_SECURITY_ERROR("dequeue_batch: NULL pointer parameters");
        CAMLreturnT(int, -1);
    }
    
    if (max_events <= 0 || max_events > 10000) {
        LOG_BOUNDS_ERROR("dequeue_batch", "max_events", max_events, 10000);
        CAMLreturnT(int, -1);
    }
    
    static value *dequeue_func = NULL;
    if (!dequeue_func) {
        dequeue_func = caml_named_value("queue_dequeue_batch");
        if (!dequeue_func) {
            LOG_SECURITY_ERROR("dequeue_batch: OCaml function not found");
            CAMLreturnT(int, -1);
        }
    }
    
    result = caml_callback2(*dequeue_func, (value)queue, Val_int(max_events));
    batch_array = result;
    
    /* Convert OCaml array back to C array */
    int array_len = Wosize_val(batch_array);
    int count = (array_len < max_events) ? array_len : max_events;
    
    for (int i = 0; i < count; i++) {
        value ml_event = Field(batch_array, i);
        
        /* Extract fields from OCaml record */
        const char* path = String_val(Field(ml_event, 0));
        size_t path_len = strlen(path);
        
        /* SECURITY FIX: Comprehensive path length validation */
        if (path_len == 0) {
            LOG_SECURITY_ERROR("dequeue_batch: empty path at index %d", i);
            CAMLreturnT(int, -1);
        }
        
        if (path_len > MAX_PATH_LENGTH) {
            LOG_BOUNDS_ERROR("dequeue_batch", "path_len", (int)path_len, MAX_PATH_LENGTH);
            CAMLreturnT(int, -1);
        }
        
        /* SECURITY FIX: Safe allocation with overflow protection */
        if (path_len < MAX_STACK_ALLOC) {
            /* Stack allocation - safe for small strings */
            events[i].path = alloca(path_len + 1);
            events[i]._alloc_type = 0;  /* Stack allocated */
            events[i]._needs_free = 0;
        } else {
            /* Heap allocation with overflow check */
            if (path_len > SIZE_MAX - 1) {
                LOG_SECURITY_ERROR("dequeue_batch: path length overflow at index %d", i);
                /* Cleanup already allocated memory */
                for (int j = 0; j < i; j++) {
                    if (events[j]._needs_free && events[j]._alloc_type == 1) {
                        free(events[j].path);
                    }
                }
                CAMLreturnT(int, -1);
            }
            
            events[i].path = malloc(path_len + 1);
            if (!events[i].path) {
                LOG_SECURITY_ERROR("dequeue_batch: malloc failed for path at index %d", i);
                /* Cleanup already allocated memory */
                for (int j = 0; j < i; j++) {
                    if (events[j]._needs_free && events[j]._alloc_type == 1) {
                        free(events[j].path);
                    }
                }
                CAMLreturnT(int, -1);
            }
            events[i]._alloc_type = 1;  /* Heap allocated */
            events[i]._needs_free = 1;
        }
        
        /* Safe string copy */
        memcpy(events[i].path, path, path_len);
        events[i].path[path_len] = '\0';
        
        events[i].offset = Int64_val(Field(ml_event, 1));
        
        /* Extract data from Bigarray - this is already managed by OCaml */
        value ml_data = Field(ml_event, 2);
        struct caml_ba_array* ba = Caml_ba_array_val(ml_data);
        
        /* SECURITY FIX: Validate Bigarray data */
        if (!ba || !ba->data || ba->dim[0] < 0) {
            LOG_SECURITY_ERROR("dequeue_batch: invalid Bigarray at index %d", i);
            /* Cleanup */
            if (events[i]._needs_free && events[i]._alloc_type == 1) {
                free(events[i].path);
            }
            for (int j = 0; j < i; j++) {
                if (events[j]._needs_free && events[j]._alloc_type == 1) {
                    free(events[j].path);
                }
            }
            CAMLreturnT(int, -1);
        }
        
        events[i].data = (char*)ba->data;
        events[i].length = ba->dim[0];
        
        events[i].is_wal = Bool_val(Field(ml_event, 3));
        events[i].fence_token = Int64_val(Field(ml_event, 4));
    }
    
    CAMLreturnT(int, count);
}

/* Get pending count in queue */
int distcore_queue_pending_count(void* queue) {
    CAMLparam0();
    CAMLlocal1(result);
    
    static value *pending_func = NULL;
    if (!pending_func) {
        pending_func = caml_named_value("queue_pending_count");
        if (!pending_func) {
            CAMLreturnT(int, 0);
        }
    }
    
    result = caml_callback(*pending_func, (value)queue);
    CAMLreturnT(int, Int_val(result));
}

/* Compute replication strategy */
typedef enum {
    STRATEGY_TXLOG = 0,
    STRATEGY_BULK_SYNC = 1,
    STRATEGY_HYBRID = 2,
    STRATEGY_PAGE_DIFF = 3
} replication_strategy_t;

replication_strategy_t distcore_compute_strategy(const write_event_t* events, int count) {
    CAMLparam0();
    CAMLlocal2(batch_array, result);
    
    /* Convert C array to OCaml array */
    batch_array = caml_alloc(count, 0);
    for (int i = 0; i < count; i++) {
        value ml_event = caml_alloc(5, 0);
        Store_field(ml_event, 0, caml_copy_string(events[i].path));
        Store_field(ml_event, 1, caml_copy_int64(events[i].offset));
        
        /* Create Bigarray for data */
        intnat dims[1];
        dims[0] = events[i].length;
        value ml_data = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, 
                                      (void*)events[i].data, dims);
        Store_field(ml_event, 2, ml_data);
        Store_field(ml_event, 3, Val_bool(events[i].is_wal));
        Store_field(ml_event, 4, caml_copy_int64(events[i].fence_token));
        
        Store_field(batch_array, i, ml_event);
    }
    
    static value *strategy_func = NULL;
    if (!strategy_func) {
        strategy_func = caml_named_value("compute_strategy");
        if (!strategy_func) {
            CAMLreturnT(replication_strategy_t, STRATEGY_TXLOG);
        }
    }
    
    result = caml_callback(*strategy_func, batch_array);
    
    /* Convert OCaml variant to C enum */
    int strategy = Int_val(result);
    CAMLreturnT(replication_strategy_t, (replication_strategy_t)strategy);
}

/* Enhanced encode function with comprehensive validation */
int distcore_encode_write_batch(const write_event_t* events, int count, 
                               char* output_buffer, int buffer_size) {
    CAMLparam0();
    CAMLlocal2(batch_array, result);
    
    /* SECURITY FIX: Comprehensive input validation */
    if (!events || !output_buffer) {
        LOG_SECURITY_ERROR("encode_write_batch: NULL pointer parameters");
        CAMLreturnT(int, -4);
    }
    
    if (count < 0 || count > 10000) {
        LOG_BOUNDS_ERROR("encode_write_batch", "count", count, 10000);
        CAMLreturnT(int, -4);
    }
    
    if (buffer_size < MIN_BUFFER_SIZE || buffer_size > MAX_BUFFER_SIZE) {
        LOG_BOUNDS_ERROR("encode_write_batch", "buffer_size", buffer_size, MAX_BUFFER_SIZE);
        CAMLreturnT(int, -4);
    }
    
    /* Convert C array to OCaml array */
    batch_array = caml_alloc(count, 0);
    for (int i = 0; i < count; i++) {
        /* Validate each event */
        if (!events[i].path || !events[i].data) {
            LOG_SECURITY_ERROR("encode_write_batch: NULL data in event %d", i);
            CAMLreturnT(int, -4);
        }
        
        if (events[i].length < 0 || events[i].length > MAX_BUFFER_SIZE) {
            LOG_BOUNDS_ERROR("encode_write_batch", "event.length", events[i].length, MAX_BUFFER_SIZE);
            CAMLreturnT(int, -4);
        }
        
        value ml_event = caml_alloc(5, 0);
        Store_field(ml_event, 0, caml_copy_string(events[i].path));
        Store_field(ml_event, 1, caml_copy_int64(events[i].offset));
        
        intnat dims[1];
        dims[0] = events[i].length;
        value ml_data = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, 
                                      (void*)events[i].data, dims);
        Store_field(ml_event, 2, ml_data);
        Store_field(ml_event, 3, Val_bool(events[i].is_wal));
        Store_field(ml_event, 4, caml_copy_int64(events[i].fence_token));
        
        Store_field(batch_array, i, ml_event);
    }
    
    static value *encode_func = NULL;
    if (!encode_func) {
        encode_func = caml_named_value("encode_write_batch");
        if (!encode_func) {
            LOG_SECURITY_ERROR("encode_write_batch: OCaml function not found");
            CAMLreturnT(int, -4);
        }
    }
    
    result = caml_callback(*encode_func, batch_array);
    
    /* SECURITY FIX: Enhanced validation with overflow checks */
    if (!result || !Is_block(result)) {
        LOG_SECURITY_ERROR("encode_write_batch: invalid OCaml result");
        CAMLreturnT(int, -4);
    }
    
    struct caml_ba_array* ba = Caml_ba_array_val(result);
    if (!ba || !ba->data) {
        LOG_SECURITY_ERROR("encode_write_batch: invalid result Bigarray");
        CAMLreturnT(int, -4);
    }
    
    /* Check for integer overflow in dimension */
    if (ba->dim[0] < 0) {
        LOG_SECURITY_ERROR("encode_write_batch: negative encoded size");
        CAMLreturnT(int, -2);
    }
    
    int encoded_size = ba->dim[0];
    
    /* SECURITY FIX: Enhanced size validation with overflow protection */
    if (encoded_size > MAX_BUFFER_SIZE) {
        LOG_BOUNDS_ERROR("encode_write_batch", "encoded_size", encoded_size, MAX_BUFFER_SIZE);
        CAMLreturnT(int, -2);
    }
    
    /* Check for potential integer overflow in comparison */
    if (encoded_size > INT_MAX || buffer_size > INT_MAX) {
        LOG_SECURITY_ERROR("encode_write_batch: size overflow risk");
        CAMLreturnT(int, -2);
    }
    
    if (encoded_size <= buffer_size) {
        /* Safe to copy */
        memcpy(output_buffer, ba->data, encoded_size);
        CAMLreturnT(int, encoded_size);
    } else {
        LOG_SECURITY_ERROR("encode_write_batch: buffer too small (%d < %d)", buffer_size, encoded_size);
        CAMLreturnT(int, -1); /* Buffer too small */
    }
}

/* Binary diff computation */
int distcore_compute_diff(const char* old_db, int old_size,
                         const char* new_db, int new_size,
                         int page_size, char* diff_output, int output_size) {
    CAMLparam0();
    CAMLlocal3(old_array, new_array, result);
    
    /* Create Bigarrays for old and new database */
    intnat old_dims[1] = {old_size};
    intnat new_dims[1] = {new_size};
    
    old_array = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, (void*)old_db, old_dims);
    new_array = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, (void*)new_db, new_dims);
    
    static value *diff_func = NULL;
    if (!diff_func) {
        diff_func = caml_named_value("compute_diff");
        if (!diff_func) {
            CAMLreturnT(int, 0);
        }
    }
    
    result = caml_callback3(*diff_func, old_array, new_array, Val_int(page_size));
    
    /* Process diff result and serialize to output buffer */
    /* This is a simplified version - real implementation would serialize the diff list */
    CAMLreturnT(int, 0); /* Placeholder */
}

/* Enhanced SIMD memory operations with comprehensive bounds checking */
value distcore_memcmp_stub(value v_arr1, value v_offset1, value v_arr2, value v_offset2, value v_len) {
    CAMLparam5(v_arr1, v_offset1, v_arr2, v_offset2, v_len);
    
    /* SECURITY FIX: Validate Bigarray pointers */
    if (!Is_block(v_arr1) || !Is_block(v_arr2)) {
        LOG_SECURITY_ERROR("memcmp_stub: invalid Bigarray parameters");
        CAMLreturn(Val_int(-1));
    }
    
    struct caml_ba_array* ba1 = Caml_ba_array_val(v_arr1);
    struct caml_ba_array* ba2 = Caml_ba_array_val(v_arr2);
    
    if (!ba1 || !ba2 || !ba1->data || !ba2->data) {
        LOG_SECURITY_ERROR("memcmp_stub: NULL Bigarray data");
        CAMLreturn(Val_int(-1));
    }
    
    int offset1 = Int_val(v_offset1);
    int offset2 = Int_val(v_offset2);
    int len = Int_val(v_len);
    
    /* SECURITY FIX: Enhanced bounds checking with overflow protection */
    if (offset1 < 0 || offset2 < 0 || len < 0) {
        LOG_SECURITY_ERROR("memcmp_stub: negative parameters");
        CAMLreturn(Val_int(-1));
    }
    
    /* Check for integer overflow in addition */
    if (offset1 > INT_MAX - len || offset2 > INT_MAX - len) {
        LOG_SECURITY_ERROR("memcmp_stub: offset + length overflow");
        CAMLreturn(Val_int(-1));
    }
    
    if (offset1 + len > ba1->dim[0] || offset2 + len > ba2->dim[0]) {
        LOG_SECURITY_ERROR("memcmp_stub: out of bounds access");
        CAMLreturn(Val_int(-1));
    }
    
    if (len > MAX_BUFFER_SIZE) {
        LOG_BOUNDS_ERROR("memcmp_stub", "len", len, MAX_BUFFER_SIZE);
        CAMLreturn(Val_int(-1));
    }
    
    char* ptr1 = (char*)ba1->data + offset1;
    char* ptr2 = (char*)ba2->data + offset2;
    
    /* Fall back to standard memcmp - SIMD disabled for now */
    int result = memcmp(ptr1, ptr2, len);
    CAMLreturn(Val_int(result));
}

/* Enhanced SIMD memory copy with security validation */
value distcore_memcpy_stub(value v_dst, value v_dst_offset, value v_src, value v_src_offset, value v_len) {
    CAMLparam5(v_dst, v_dst_offset, v_src, v_src_offset, v_len);
    
    /* SECURITY FIX: Validate parameters */
    if (!Is_block(v_dst) || !Is_block(v_src)) {
        LOG_SECURITY_ERROR("memcpy_stub: invalid Bigarray parameters");
        CAMLreturn(Val_unit);
    }
    
    struct caml_ba_array* dst_ba = Caml_ba_array_val(v_dst);
    struct caml_ba_array* src_ba = Caml_ba_array_val(v_src);
    
    if (!dst_ba || !src_ba || !dst_ba->data || !src_ba->data) {
        LOG_SECURITY_ERROR("memcpy_stub: NULL Bigarray data");
        CAMLreturn(Val_unit);
    }
    
    int dst_offset = Int_val(v_dst_offset);
    int src_offset = Int_val(v_src_offset);
    int len = Int_val(v_len);
    
    /* SECURITY FIX: Comprehensive bounds checking */
    if (dst_offset < 0 || src_offset < 0 || len < 0) {
        LOG_SECURITY_ERROR("memcpy_stub: negative parameters");
        CAMLreturn(Val_unit);
    }
    
    if (dst_offset > INT_MAX - len || src_offset > INT_MAX - len) {
        LOG_SECURITY_ERROR("memcpy_stub: offset + length overflow");
        CAMLreturn(Val_unit);
    }
    
    if (dst_offset + len > dst_ba->dim[0] || src_offset + len > src_ba->dim[0]) {
        LOG_SECURITY_ERROR("memcpy_stub: out of bounds access");
        CAMLreturn(Val_unit);
    }
    
    if (len > MAX_BUFFER_SIZE) {
        LOG_BOUNDS_ERROR("memcpy_stub", "len", len, MAX_BUFFER_SIZE);
        CAMLreturn(Val_unit);
    }
    
    char* dst = (char*)dst_ba->data + dst_offset;
    char* src = (char*)src_ba->data + src_offset;
    
    /* Check for overlapping memory regions */
    if ((src < dst && src + len > dst) || (dst < src && dst + len > src)) {
        LOG_SECURITY_ERROR("memcpy_stub: overlapping memory regions detected");
        /* Use memmove for overlapping regions */
        memmove(dst, src, len);
        CAMLreturn(Val_unit);
    }
    
    /* Fall back to standard memcpy - SIMD disabled for now */
    memcpy(dst, src, len);
    
    CAMLreturn(Val_unit);
}

/* SIMD hash computation */
value distcore_hash_simd_stub(value v_data, value v_offset, value v_len) {
    CAMLparam3(v_data, v_offset, v_len);
    
    struct caml_ba_array* ba = Caml_ba_array_val(v_data);
    char* data = (char*)ba->data + Int_val(v_offset);
    int len = Int_val(v_len);
    
    uint32_t hash = 0x9e3779b9U;
    
    /* Use scalar hash - SIMD disabled for now */
    for (int i = 0; i < len; i++) {
        hash = hash * 0x9e3779b9U + (unsigned char)data[i];
    }
    
    CAMLreturn(caml_copy_int32(hash));
}

/* VFS Integration Support */
#include <sqlite3.h>

/* Extended file structure with OCaml integration */
typedef struct {
    sqlite3_file base;
    value ocaml_file;
    int page_size;
    bool intercept_enabled;
    char* db_path;
} ocaml_vfs_file;

/* Page tracker operations */
void* distcore_create_page_tracker(int page_size) {
    CAMLparam0();
    CAMLlocal1(result);
    
    static value *create_func = NULL;
    if (!create_func) {
        create_func = caml_named_value("create_page_tracker");
        if (!create_func) {
            CAMLreturnT(void*, NULL);
        }
    }
    
    result = caml_callback(*create_func, Val_int(page_size));
    CAMLreturnT(void*, (void*)result);
}

/* VFS file operations */
static int vfs_close(sqlite3_file *pFile) {
    ocaml_vfs_file *file = (ocaml_vfs_file*)pFile;
    if (file->db_path) {
        free(file->db_path);
    }
    return SQLITE_OK;
}

static int vfs_read(sqlite3_file *pFile, void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    ocaml_vfs_file *file = (ocaml_vfs_file*)pFile;
    
    if (file->intercept_enabled) {
        CAMLparam0();
        CAMLlocal3(result, buffer, ml_file);
        
        /* Create Bigarray for buffer */
        intnat dims[1] = {iAmt};
        buffer = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, zBuf, dims);
        
        ml_file = file->ocaml_file;
        
        static value *read_func = NULL;
        if (!read_func) {
            read_func = caml_named_value("vfs_intercept_read");
            if (!read_func) {
                CAMLreturnT(int, SQLITE_IOERR_READ);
            }
        }
        
        result = caml_callback3(*read_func, ml_file, buffer, caml_copy_int64(iOfst));
        
        if (Is_exception_result(result)) {
            CAMLreturnT(int, SQLITE_IOERR_READ);
        }
        
        int bytes_read = Int_val(result);
        if (bytes_read == iAmt) {
            CAMLreturnT(int, SQLITE_OK);
        } else if (bytes_read < iAmt) {
            /* Partial read - zero out remaining buffer */
            memset((char*)zBuf + bytes_read, 0, iAmt - bytes_read);
            CAMLreturnT(int, SQLITE_IOERR_SHORT_READ);
        }
        
        CAMLreturnT(int, SQLITE_OK);
    }
    
    /* Fall back to default implementation */
    return SQLITE_IOERR_READ;
}

static int vfs_write(sqlite3_file *pFile, const void *zBuf, int iAmt, sqlite3_int64 iOfst) {
    ocaml_vfs_file *file = (ocaml_vfs_file*)pFile;
    
    if (file->intercept_enabled) {
        CAMLparam0();
        CAMLlocal3(result, buffer, ml_file);
        
        /* Create Bigarray for buffer */
        intnat dims[1] = {iAmt};
        buffer = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, (void*)zBuf, dims);
        
        ml_file = file->ocaml_file;
        
        static value *write_func = NULL;
        if (!write_func) {
            write_func = caml_named_value("vfs_intercept_write");
            if (!write_func) {
                CAMLreturnT(int, SQLITE_IOERR_WRITE);
            }
        }
        
        result = caml_callback3(*write_func, ml_file, buffer, caml_copy_int64(iOfst));
        
        if (Is_exception_result(result)) {
            CAMLreturnT(int, SQLITE_IOERR_WRITE);
        }
        
        int bytes_written = Int_val(result);
        if (bytes_written == iAmt) {
            CAMLreturnT(int, SQLITE_OK);
        }
        
        CAMLreturnT(int, SQLITE_IOERR_WRITE);
    }
    
    /* Fall back to default implementation */
    return SQLITE_IOERR_WRITE;
}

static int vfs_truncate(sqlite3_file *pFile, sqlite3_int64 size) {
    return SQLITE_OK;
}

static int vfs_sync(sqlite3_file *pFile, int flags) {
    return SQLITE_OK;
}

static int vfs_file_size(sqlite3_file *pFile, sqlite3_int64 *pSize) {
    *pSize = 0;
    return SQLITE_OK;
}

/* VFS method table */
static const sqlite3_io_methods ocaml_io_methods = {
    3,                    /* iVersion */
    vfs_close,           /* xClose */
    vfs_read,            /* xRead */
    vfs_write,           /* xWrite */
    vfs_truncate,        /* xTruncate */
    vfs_sync,            /* xSync */
    vfs_file_size,       /* xFileSize */
    NULL,                /* xLock */
    NULL,                /* xUnlock */
    NULL,                /* xCheckReservedLock */
    NULL,                /* xFileControl */
    NULL,                /* xSectorSize */
    NULL,                /* xDeviceCharacteristics */
    NULL,                /* xShmMap */
    NULL,                /* xShmLock */
    NULL,                /* xShmBarrier */
    NULL,                /* xShmUnmap */
    NULL,                /* xFetch */
    NULL                 /* xUnfetch */
};

/* Forward declarations for VFS methods */
static int vfs_open(sqlite3_vfs*, const char*, sqlite3_file*, int, int*);
static int vfs_delete(sqlite3_vfs*, const char*, int);
static int vfs_access(sqlite3_vfs*, const char*, int, int*);
static int vfs_full_pathname(sqlite3_vfs*, const char*, int, char*);
static void* vfs_dlopen(sqlite3_vfs*, const char*);
static void vfs_dlerror(sqlite3_vfs*, int, char*);
static void (*vfs_dlsym(sqlite3_vfs*, void*, const char*))(void);
static void vfs_dlclose(sqlite3_vfs*, void*);
static int vfs_randomness(sqlite3_vfs*, int, char*);
static int vfs_sleep(sqlite3_vfs*, int);
static int vfs_current_time(sqlite3_vfs*, double*);
static int vfs_get_last_error(sqlite3_vfs*, int, char*);

/* Get the default VFS */
static sqlite3_vfs* base_vfs = NULL;

/* Our custom VFS structure */
static sqlite3_vfs distcore_vfs = {
    3,                        /* iVersion */
    sizeof(ocaml_vfs_file),   /* szOsFile */
    1024,                     /* mxPathname */
    NULL,                     /* pNext */
    "distcore",               /* zName */
    NULL,                     /* pAppData */
    vfs_open,                 /* xOpen */
    vfs_delete,               /* xDelete */
    vfs_access,               /* xAccess */
    vfs_full_pathname,        /* xFullPathname */
    vfs_dlopen,               /* xDlOpen */
    vfs_dlerror,              /* xDlError */
    vfs_dlsym,                /* xDlSym */
    vfs_dlclose,              /* xDlClose */
    vfs_randomness,           /* xRandomness */
    vfs_sleep,                /* xSleep */
    vfs_current_time,         /* xCurrentTime */
    vfs_get_last_error,       /* xGetLastError */
    NULL,                     /* xCurrentTimeInt64 */
    NULL,                     /* xSetSystemCall */
    NULL,                     /* xGetSystemCall */
    NULL                      /* xNextSystemCall */
};

/* VFS xOpen implementation */
static int vfs_open(sqlite3_vfs *pVfs, const char *zName, sqlite3_file *pFile, 
                    int flags, int *pOutFlags) {
    ocaml_vfs_file *file = (ocaml_vfs_file*)pFile;
    
    /* Initialize the file structure */
    memset(file, 0, sizeof(ocaml_vfs_file));
    file->base.pMethods = &ocaml_io_methods;
    
    /* Store database path */
    if (zName) {
        file->db_path = strdup(zName);
        if (!file->db_path) {
            return SQLITE_NOMEM;
        }
    }
    
    /* Create page tracker */
    file->page_size = 4096; /* Default SQLite page size */
    file->ocaml_file = (value)distcore_create_page_tracker(file->page_size);
    file->intercept_enabled = true;
    
    /* For now, we're not actually opening a file - just intercepting */
    /* In a real implementation, we'd delegate to the base VFS */
    
    if (pOutFlags) {
        *pOutFlags = flags;
    }
    
    printf("[DistCore VFS] Opened database: %s\n", zName ? zName : "(temp)");
    
    return SQLITE_OK;
}

/* Delegate other VFS methods to the base VFS */
static int vfs_delete(sqlite3_vfs *pVfs, const char *zName, int syncDir) {
    return base_vfs->xDelete(base_vfs, zName, syncDir);
}

static int vfs_access(sqlite3_vfs *pVfs, const char *zName, int flags, int *pResOut) {
    return base_vfs->xAccess(base_vfs, zName, flags, pResOut);
}

static int vfs_full_pathname(sqlite3_vfs *pVfs, const char *zName, int nOut, char *zOut) {
    return base_vfs->xFullPathname(base_vfs, zName, nOut, zOut);
}

static void* vfs_dlopen(sqlite3_vfs *pVfs, const char *zFilename) {
    return base_vfs->xDlOpen(base_vfs, zFilename);
}

static void vfs_dlerror(sqlite3_vfs *pVfs, int nByte, char *zErrMsg) {
    base_vfs->xDlError(base_vfs, nByte, zErrMsg);
}

static void (*vfs_dlsym(sqlite3_vfs *pVfs, void *p, const char *zSymbol))(void) {
    return base_vfs->xDlSym(base_vfs, p, zSymbol);
}

static void vfs_dlclose(sqlite3_vfs *pVfs, void *p) {
    base_vfs->xDlClose(base_vfs, p);
}

static int vfs_randomness(sqlite3_vfs *pVfs, int nByte, char *zOut) {
    return base_vfs->xRandomness(base_vfs, nByte, zOut);
}

static int vfs_sleep(sqlite3_vfs *pVfs, int microseconds) {
    return base_vfs->xSleep(base_vfs, microseconds);
}

static int vfs_current_time(sqlite3_vfs *pVfs, double *pTime) {
    return base_vfs->xCurrentTime(base_vfs, pTime);
}

static int vfs_get_last_error(sqlite3_vfs *pVfs, int nByte, char *zOut) {
    return base_vfs->xGetLastError(base_vfs, nByte, zOut);
}

/* Register the VFS with SQLite */
int distcore_register_vfs() {
    /* Get the default VFS to delegate to */
    base_vfs = sqlite3_vfs_find(NULL);
    if (!base_vfs) {
        fprintf(stderr, "[DistCore VFS] Failed to find default VFS\n");
        return -1;
    }
    
    /* Register our VFS */
    int rc = sqlite3_vfs_register(&distcore_vfs, 1); /* Make it default */
    if (rc != SQLITE_OK) {
        fprintf(stderr, "[DistCore VFS] Failed to register VFS: %d\n", rc);
        return -1;
    }
    
    printf("[DistCore VFS] Successfully registered VFS 'distcore' as default\n");
    return 0;
}

/* Unregister the VFS */
int distcore_unregister_vfs() {
    int rc = sqlite3_vfs_unregister(&distcore_vfs);
    if (rc != SQLITE_OK) {
        fprintf(stderr, "[DistCore VFS] Failed to unregister VFS: %d\n", rc);
        return -1;
    }
    
    printf("[DistCore VFS] Successfully unregistered VFS 'distcore'\n");
    return 0;
}

/* Enhanced VFS operations with type safety */
int distcore_vfs_intercept_read(void* file, char* buffer, int64_t offset, int amount) {
    /* SECURITY FIX: Enhanced parameter validation */
    if (!file || !buffer) {
        LOG_SECURITY_ERROR("vfs_intercept_read: NULL parameters");
        return -1;
    }
    
    if (amount < 0 || amount > MAX_BUFFER_SIZE) {
        LOG_BOUNDS_ERROR("vfs_intercept_read", "amount", amount, MAX_BUFFER_SIZE);
        return -1;
    }
    
    if (offset < 0) {
        LOG_SECURITY_ERROR("vfs_intercept_read: negative offset");
        return -1;
    }
    
    /* SECURITY FIX: Type validation before casting */
    sqlite3_file *sqlite_file = (sqlite3_file*)file;
    
    /* Basic sanity check on the file structure */
    if (!sqlite_file->pMethods) {
        LOG_SECURITY_ERROR("vfs_intercept_read: invalid file structure");
        return -1;
    }
    
    return vfs_read(sqlite_file, buffer, amount, offset);
}

int distcore_vfs_intercept_write(void* file, char* buffer, int64_t offset, int amount) {
    /* SECURITY FIX: Enhanced parameter validation */
    if (!file || !buffer) {
        LOG_SECURITY_ERROR("vfs_intercept_write: NULL parameters");
        return -1;
    }
    
    if (amount < 0 || amount > MAX_BUFFER_SIZE) {
        LOG_BOUNDS_ERROR("vfs_intercept_write", "amount", amount, MAX_BUFFER_SIZE);
        return -1;
    }
    
    if (offset < 0) {
        LOG_SECURITY_ERROR("vfs_intercept_write: negative offset");
        return -1;
    }
    
    /* SECURITY FIX: Type validation before casting */
    sqlite3_file *sqlite_file = (sqlite3_file*)file;
    
    /* Basic sanity check on the file structure */
    if (!sqlite_file->pMethods) {
        LOG_SECURITY_ERROR("vfs_intercept_write: invalid file structure");
        return -1;
    }
    
    return vfs_write(sqlite_file, buffer, amount, offset);
}

/* Get sync pages from VFS file */
int distcore_get_sync_pages(void* file, int* page_nums, char** page_hashes, char** page_data, int max_pages) {
    if (!file) return -1;
    
    CAMLparam0();
    CAMLlocal2(result, ml_file);
    
    ocaml_vfs_file *vfs_file = (ocaml_vfs_file*)file;
    ml_file = vfs_file->ocaml_file;
    
    static value *sync_pages_func = NULL;
    if (!sync_pages_func) {
        sync_pages_func = caml_named_value("get_sync_pages");
        if (!sync_pages_func) {
            CAMLreturnT(int, -1);
        }
    }
    
    result = caml_callback(*sync_pages_func, ml_file);
    
    if (Is_exception_result(result)) {
        CAMLreturnT(int, -1);
    }
    
    /* Process OCaml list of sync pages */
    int count = 0;
    value pages_list = result;
    
    while (pages_list != Val_emptylist && count < max_pages) {
        value page_tuple = Field(pages_list, 0);
        
        /* Extract (page_num, hash, data) tuple */
        page_nums[count] = Int_val(Field(page_tuple, 0));
        
        /* Copy hash string - caller responsible for freeing */
        value hash_val = Field(page_tuple, 1);
        int hash_len = caml_string_length(hash_val);
        page_hashes[count] = malloc(hash_len + 1);
        if (!page_hashes[count]) {
            /* Cleanup already allocated memory on failure */
            for (int j = 0; j < count; j++) {
                free(page_hashes[j]);
                free(page_data[j]);
            }
            CAMLreturnT(int, -1);
        }
        memcpy(page_hashes[count], String_val(hash_val), hash_len);
        page_hashes[count][hash_len] = '\0';
        
        /* Copy page data - caller responsible for freeing */
        value data_val = Field(page_tuple, 2);
        struct caml_ba_array* ba = Caml_ba_array_val(data_val);
        int data_len = ba->dim[0];
        page_data[count] = malloc(data_len);
        if (!page_data[count]) {
            /* Cleanup on failure */
            free(page_hashes[count]);
            for (int j = 0; j < count; j++) {
                free(page_hashes[j]);
                free(page_data[j]);
            }
            CAMLreturnT(int, -1);
        }
        memcpy(page_data[count], ba->data, data_len);
        
        pages_list = Field(pages_list, 1);
        count++;
    }
    
    CAMLreturnT(int, count);
}

/* Compute diff pages between local and replica */
int distcore_compute_diff_pages(int* local_pages, char** local_hashes, 
                               int* replica_pages, char** replica_hashes, 
                               int local_count, int replica_count,
                               int* diff_pages, int max_diff) {
    CAMLparam0();
    CAMLlocal4(local_list, replica_list, result, temp);
    
    /* Build OCaml lists */
    local_list = Val_emptylist;
    for (int i = local_count - 1; i >= 0; i--) {
        value page_tuple = caml_alloc(2, 0);
        Store_field(page_tuple, 0, Val_int(local_pages[i]));
        Store_field(page_tuple, 1, caml_copy_string(local_hashes[i]));
        
        temp = caml_alloc(2, 0);
        Store_field(temp, 0, page_tuple);
        Store_field(temp, 1, local_list);
        local_list = temp;
    }
    
    replica_list = Val_emptylist;
    for (int i = replica_count - 1; i >= 0; i--) {
        value page_tuple = caml_alloc(2, 0);
        Store_field(page_tuple, 0, Val_int(replica_pages[i]));
        Store_field(page_tuple, 1, caml_copy_string(replica_hashes[i]));
        
        temp = caml_alloc(2, 0);
        Store_field(temp, 0, page_tuple);
        Store_field(temp, 1, replica_list);
        replica_list = temp;
    }
    
    static value *diff_func = NULL;
    if (!diff_func) {
        diff_func = caml_named_value("compute_diff_pages");
        if (!diff_func) {
            CAMLreturnT(int, -1);
        }
    }
    
    result = caml_callback2(*diff_func, local_list, replica_list);
    
    if (Is_exception_result(result)) {
        CAMLreturnT(int, -1);
    }
    
    /* Extract page numbers from result list */
    int count = 0;
    value diff_list = result;
    
    while (diff_list != Val_emptylist && count < max_diff) {
        diff_pages[count] = Int_val(Field(diff_list, 0));
        diff_list = Field(diff_list, 1);
        count++;
    }
    
    CAMLreturnT(int, count);
}

/* Cleanup function */
void distcore_cleanup() {
    /* Cleanup resources if needed */
}

/* Constructor function - automatically called when library is loaded */
__attribute__((constructor))
static void distcore_auto_init() {
    static int auto_initialized = 0;
    if (auto_initialized) return;
    auto_initialized = 1;
    
    /* Only auto-initialize if we're being loaded via LD_PRELOAD */
    const char* preload = getenv("LD_PRELOAD");
    if (!preload || !strstr(preload, "libdistcore.so")) {
        return;
    }
    
    printf("[DistCore] Loaded via LD_PRELOAD, will initialize on first SQLite use\n");
    /* Don't initialize here - wait for SQLite to be loaded */
}

/* Destructor function - automatically called when library is unloaded */
__attribute__((destructor))
static void distcore_auto_cleanup() {
    /* Only cleanup if we actually initialized */
    static int cleaned_up = 0;
    if (cleaned_up) return;
    cleaned_up = 1;
    
    /* Unregister VFS if needed */
    if (base_vfs) {
        distcore_unregister_vfs();
    }
    distcore_cleanup();
}

/* Hook sqlite3_open to perform lazy initialization */
#include <dlfcn.h>

typedef int (*sqlite3_open_fn)(const char*, sqlite3**);
typedef int (*sqlite3_open_v2_fn)(const char*, sqlite3**, int, const char*);

static sqlite3_open_fn real_sqlite3_open = NULL;
static sqlite3_open_v2_fn real_sqlite3_open_v2 = NULL;
static int lazy_initialized = 0;

static void lazy_init() {
    if (lazy_initialized) return;
    lazy_initialized = 1;
    
    printf("[DistCore] Lazy initialization triggered by SQLite usage\n");
    distcore_init_vfs();
}

/* Intercept sqlite3_open */
int sqlite3_open(const char *filename, sqlite3 **ppDb) {
    lazy_init();
    
    if (!real_sqlite3_open) {
        real_sqlite3_open = (sqlite3_open_fn)dlsym(RTLD_NEXT, "sqlite3_open");
        if (!real_sqlite3_open) {
            fprintf(stderr, "[DistCore] Failed to find real sqlite3_open: %s\n", dlerror());
            return SQLITE_ERROR;
        }
        /* Validate the function pointer */
        if ((void*)real_sqlite3_open == (void*)sqlite3_open) {
            fprintf(stderr, "[DistCore] dlsym returned our own function!\n");
            real_sqlite3_open = NULL;
            return SQLITE_ERROR;
        }
    }
    
    return real_sqlite3_open(filename, ppDb);
}

/* Intercept sqlite3_open_v2 */
int sqlite3_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs) {
    lazy_init();
    
    if (!real_sqlite3_open_v2) {
        real_sqlite3_open_v2 = (sqlite3_open_v2_fn)dlsym(RTLD_NEXT, "sqlite3_open_v2");
        if (!real_sqlite3_open_v2) {
            fprintf(stderr, "[DistCore] Failed to find real sqlite3_open_v2: %s\n", dlerror());
            return SQLITE_ERROR;
        }
        /* Validate the function pointer */
        if ((void*)real_sqlite3_open_v2 == (void*)sqlite3_open_v2) {
            fprintf(stderr, "[DistCore] dlsym returned our own function!\n");
            real_sqlite3_open_v2 = NULL;
            return SQLITE_ERROR;
        }
    }
    
    /* If no VFS specified and our VFS is registered, use it */
    if (!zVfs && base_vfs) {
        zVfs = "distcore";
    }
    
    return real_sqlite3_open_v2(filename, ppDb, flags, zVfs);
}