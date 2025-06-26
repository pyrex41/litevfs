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
#include <immintrin.h>  /* For SIMD intrinsics */

/* Write event structure matching Go and OCaml */
typedef struct {
    char* path;
    int64_t offset;
    int32_t length;
    char* data;
    int is_wal;
    int64_t fence_token;
} write_event_t;

/* Initialize OCaml runtime - called once from Go */
void distcore_init() {
    char *argv[] = {"distcore", NULL};
    caml_startup(argv);
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

/* Dequeue batch of events - returns count of events dequeued */
int distcore_queue_dequeue_batch(void* queue, write_event_t* events, int max_events) {
    CAMLparam0();
    CAMLlocal2(result, batch_array);
    
    static value *dequeue_func = NULL;
    if (!dequeue_func) {
        dequeue_func = caml_named_value("queue_dequeue_batch");
        if (!dequeue_func) {
            CAMLreturnT(int, 0);
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
        events[i].path = malloc(strlen(path) + 1);
        strcpy(events[i].path, path);
        
        events[i].offset = Int64_val(Field(ml_event, 1));
        
        /* Extract data from Bigarray */
        value ml_data = Field(ml_event, 2);
        struct caml_ba_array* ba = Caml_ba_array_val(ml_data);
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

/* Encode write batch to binary format */
int distcore_encode_write_batch(const write_event_t* events, int count, 
                               char* output_buffer, int buffer_size) {
    CAMLparam0();
    CAMLlocal2(batch_array, result);
    
    /* Convert C array to OCaml array */
    batch_array = caml_alloc(count, 0);
    for (int i = 0; i < count; i++) {
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
            CAMLreturnT(int, 0);
        }
    }
    
    result = caml_callback(*encode_func, batch_array);
    
    /* Copy encoded data from OCaml Bigarray to C buffer */
    struct caml_ba_array* ba = Caml_ba_array_val(result);
    int encoded_size = ba->dim[0];
    
    if (encoded_size <= buffer_size) {
        memcpy(output_buffer, ba->data, encoded_size);
        CAMLreturnT(int, encoded_size);
    } else {
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

/* SIMD-optimized memory operations */

/* SIMD memory comparison */
value distcore_memcmp_stub(value v_arr1, value v_offset1, value v_arr2, value v_offset2, value v_len) {
    CAMLparam5(v_arr1, v_offset1, v_arr2, v_offset2, v_len);
    
    struct caml_ba_array* ba1 = Caml_ba_array_val(v_arr1);
    struct caml_ba_array* ba2 = Caml_ba_array_val(v_arr2);
    
    char* ptr1 = (char*)ba1->data + Int_val(v_offset1);
    char* ptr2 = (char*)ba2->data + Int_val(v_offset2);
    int len = Int_val(v_len);
    
    /* SIMD-optimized comparison for aligned data */
    if (len >= 32 && ((uintptr_t)ptr1 % 32) == 0 && ((uintptr_t)ptr2 % 32) == 0) {
        int simd_len = len & ~31; /* Round down to multiple of 32 */
        
        for (int i = 0; i < simd_len; i += 32) {
            __m256i a = _mm256_load_si256((__m256i*)(ptr1 + i));
            __m256i b = _mm256_load_si256((__m256i*)(ptr2 + i));
            __m256i cmp = _mm256_cmpeq_epi8(a, b);
            
            if (_mm256_movemask_epi8(cmp) != 0xFFFFFFFF) {
                /* Found difference, fall back to byte-by-byte comparison */
                for (int j = i; j < i + 32; j++) {
                    int diff = ptr1[j] - ptr2[j];
                    if (diff != 0) CAMLreturn(Val_int(diff));
                }
            }
        }
        
        /* Compare remaining bytes */
        for (int i = simd_len; i < len; i++) {
            int diff = ptr1[i] - ptr2[i];
            if (diff != 0) CAMLreturn(Val_int(diff));
        }
        
        CAMLreturn(Val_int(0));
    } else {
        /* Fall back to standard memcmp */
        int result = memcmp(ptr1, ptr2, len);
        CAMLreturn(Val_int(result));
    }
}

/* SIMD memory copy */
value distcore_memcpy_stub(value v_dst, value v_dst_offset, value v_src, value v_src_offset, value v_len) {
    CAMLparam5(v_dst, v_dst_offset, v_src, v_src_offset, v_len);
    
    struct caml_ba_array* dst_ba = Caml_ba_array_val(v_dst);
    struct caml_ba_array* src_ba = Caml_ba_array_val(v_src);
    
    char* dst = (char*)dst_ba->data + Int_val(v_dst_offset);
    char* src = (char*)src_ba->data + Int_val(v_src_offset);
    int len = Int_val(v_len);
    
    /* SIMD-optimized copy for aligned data */
    if (len >= 32 && ((uintptr_t)dst % 32) == 0 && ((uintptr_t)src % 32) == 0) {
        int simd_len = len & ~31;
        
        for (int i = 0; i < simd_len; i += 32) {
            __m256i data = _mm256_load_si256((__m256i*)(src + i));
            _mm256_store_si256((__m256i*)(dst + i), data);
        }
        
        /* Copy remaining bytes */
        memcpy(dst + simd_len, src + simd_len, len - simd_len);
    } else {
        /* Fall back to standard memcpy */
        memcpy(dst, src, len);
    }
    
    CAMLreturn(Val_unit);
}

/* SIMD hash computation */
value distcore_hash_simd_stub(value v_data, value v_offset, value v_len) {
    CAMLparam3(v_data, v_offset, v_len);
    
    struct caml_ba_array* ba = Caml_ba_array_val(v_data);
    char* data = (char*)ba->data + Int_val(v_offset);
    int len = Int_val(v_len);
    
    uint32_t hash = 0x9e3779b9U;
    
    /* SIMD hash computation for aligned data */
    if (len >= 32 && ((uintptr_t)data % 32) == 0) {
        __m256i hash_vec = _mm256_set1_epi32(hash);
        __m256i multiplier = _mm256_set1_epi32(0x9e3779b9U);
        
        int simd_len = len & ~31;
        for (int i = 0; i < simd_len; i += 32) {
            __m256i chunk = _mm256_load_si256((__m256i*)(data + i));
            
            /* Convert bytes to 32-bit integers and accumulate hash */
            __m256i lo = _mm256_unpacklo_epi8(chunk, _mm256_setzero_si256());
            __m256i hi = _mm256_unpackhi_epi8(chunk, _mm256_setzero_si256());
            
            lo = _mm256_unpacklo_epi16(lo, _mm256_setzero_si256());
            hi = _mm256_unpacklo_epi16(hi, _mm256_setzero_si256());
            
            hash_vec = _mm256_mullo_epi32(hash_vec, multiplier);
            hash_vec = _mm256_add_epi32(hash_vec, lo);
            hash_vec = _mm256_mullo_epi32(hash_vec, multiplier);
            hash_vec = _mm256_add_epi32(hash_vec, hi);
        }
        
        /* Reduce hash vector to single value */
        uint32_t hash_array[8];
        _mm256_storeu_si256((__m256i*)hash_array, hash_vec);
        
        hash = hash_array[0];
        for (int i = 1; i < 8; i++) {
            hash ^= hash_array[i];
        }
        
        /* Process remaining bytes */
        for (int i = simd_len; i < len; i++) {
            hash = hash * 0x9e3779b9U + (unsigned char)data[i];
        }
    } else {
        /* Fall back to scalar hash */
        for (int i = 0; i < len; i++) {
            hash = hash * 0x9e3779b9U + (unsigned char)data[i];
        }
    }
    
    CAMLreturn(caml_copy_int32(hash));
}

/* Cleanup function */
void distcore_cleanup() {
    /* Cleanup resources if needed */
}