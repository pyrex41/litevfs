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
#include <immintrin.h>  /* For SIMD intrinsics */
#include <cpuid.h>      /* For CPU feature detection */

/* CPU feature detection */
static int cpu_has_avx2 = -1; /* -1 = not checked, 0 = no, 1 = yes */

static int check_avx2_support(void) {
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
}

/* Write event structure matching Go and OCaml */
typedef struct {
    char* path;
    int64_t offset;
    int32_t length;
    char* data;
    int is_wal;
    int64_t fence_token;
    int _needs_free;  /* Internal flag for memory management */
} write_event_t;

/* Cleanup function for write events */
void distcore_cleanup_write_events(write_event_t* events, int count) {
    if (!events) return;
    
    for (int i = 0; i < count; i++) {
        if (events[i]._needs_free && events[i].path) {
            free(events[i].path);
            events[i].path = NULL;
            events[i]._needs_free = 0;
        }
    }
}

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
        size_t path_len = strlen(path);
        
        /* Use stack allocation for small strings, heap for large ones */
        if (path_len < 256) {
            events[i].path = alloca(path_len + 1);
        } else {
            events[i].path = malloc(path_len + 1);
            /* Mark for cleanup - we'll track this */
            events[i]._needs_free = 1;
        }
        strcpy(events[i].path, path);
        
        events[i].offset = Int64_val(Field(ml_event, 1));
        
        /* Extract data from Bigarray - this is already managed by OCaml */
        value ml_data = Field(ml_event, 2);
        struct caml_ba_array* ba = Caml_ba_array_val(ml_data);
        events[i].data = (char*)ba->data;
        events[i].length = ba->dim[0];
        
        events[i].is_wal = Bool_val(Field(ml_event, 3));
        events[i].fence_token = Int64_val(Field(ml_event, 4));
        events[i]._needs_free = 0; /* Default to no cleanup needed */
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
    
    /* Copy encoded data from OCaml Bigarray to C buffer with bounds checking */
    struct caml_ba_array* ba = Caml_ba_array_val(result);
    int encoded_size = ba->dim[0];
    
    /* Validate sizes */
    if (encoded_size < 0 || buffer_size < 0) {
        CAMLreturnT(int, -2); /* Invalid size parameters */
    }
    
    if (encoded_size <= buffer_size && output_buffer != NULL && ba->data != NULL) {
        memcpy(output_buffer, ba->data, encoded_size);
        CAMLreturnT(int, encoded_size);
    } else if (encoded_size > buffer_size) {
        CAMLreturnT(int, -1); /* Buffer too small */
    } else {
        CAMLreturnT(int, -3); /* Null pointer error */
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

/* SIMD memory comparison with bounds checking */
value distcore_memcmp_stub(value v_arr1, value v_offset1, value v_arr2, value v_offset2, value v_len) {
    CAMLparam5(v_arr1, v_offset1, v_arr2, v_offset2, v_len);
    
    struct caml_ba_array* ba1 = Caml_ba_array_val(v_arr1);
    struct caml_ba_array* ba2 = Caml_ba_array_val(v_arr2);
    
    int offset1 = Int_val(v_offset1);
    int offset2 = Int_val(v_offset2);
    int len = Int_val(v_len);
    
    /* Bounds checking */
    if (offset1 < 0 || offset2 < 0 || len < 0) {
        CAMLreturn(Val_int(-1)); /* Invalid parameters */
    }
    
    if (offset1 + len > ba1->dim[0] || offset2 + len > ba2->dim[0]) {
        CAMLreturn(Val_int(-1)); /* Out of bounds */
    }
    
    if (!ba1->data || !ba2->data) {
        CAMLreturn(Val_int(-1)); /* Null pointer */
    }
    
    char* ptr1 = (char*)ba1->data + offset1;
    char* ptr2 = (char*)ba2->data + offset2;
    
    /* SIMD-optimized comparison for aligned data - check CPU support first */
    if (check_avx2_support() && len >= 32 && 
        ((uintptr_t)ptr1 % 32) == 0 && ((uintptr_t)ptr2 % 32) == 0) {
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

/* VFS interception functions exposed to Go */
int distcore_vfs_intercept_read(void* file, char* buffer, int64_t offset, int amount) {
    if (!file) return -1;
    
    sqlite3_file *sqlite_file = (sqlite3_file*)file;
    return vfs_read(sqlite_file, buffer, amount, offset);
}

int distcore_vfs_intercept_write(void* file, char* buffer, int64_t offset, int amount) {
    if (!file) return -1;
    
    sqlite3_file *sqlite_file = (sqlite3_file*)file;
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