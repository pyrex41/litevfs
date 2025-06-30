/* DistSQLite Core - Minimal C stubs for OCaml integration
 * 
 * This file contains ONLY essential FFI functions for data conversion.
 * All business logic has been moved to OCaml for better maintainability.
 */

#include <caml/mlvalues.h>
#include <caml/memory.h>
#include <caml/alloc.h>
#include <caml/bigarray.h>
#include <caml/custom.h>
#include <caml/callback.h>
#include <caml/threads.h>
#include <string.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

/* Write event structure - matches OCaml record definition */
typedef struct {
    char* path;
    int64_t offset;
    int32_t length;
    char* data;
    int is_wal;
    int64_t fence_token;
} write_event_c;

/* Error handling */
#define STUB_OK 0
#define STUB_ERROR -1

/* Convert C write event to OCaml record */
value distcore_write_event_of_c(value v_event_ptr) {
    CAMLparam1(v_event_ptr);
    CAMLlocal3(ml_event, ml_path, ml_data);
    
    write_event_c* event = (write_event_c*)Nativeint_val(v_event_ptr);
    if (!event || !event->path || !event->data) {
        caml_failwith("distcore_write_event_of_c: invalid event pointer");
    }
    
    /* Create OCaml string for path */
    ml_path = caml_copy_string(event->path);
    
    /* Create bigarray for data (zero-copy view) */
    intnat dims[1] = {event->length};
    ml_data = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, event->data, dims);
    
    /* Create OCaml record: {path; offset; data; is_wal; fence_token} */
    ml_event = caml_alloc(5, 0);
    Store_field(ml_event, 0, ml_path);
    Store_field(ml_event, 1, caml_copy_int64(event->offset));
    Store_field(ml_event, 2, ml_data);
    Store_field(ml_event, 3, Val_bool(event->is_wal));
    Store_field(ml_event, 4, caml_copy_int64(event->fence_token));
    
    CAMLreturn(ml_event);
}

/* Convert OCaml record to C write event */
value distcore_write_event_to_c(value ml_event, value v_event_ptr) {
    CAMLparam2(ml_event, v_event_ptr);
    
    write_event_c* event = (write_event_c*)Nativeint_val(v_event_ptr);
    if (!event) {
        caml_failwith("distcore_write_event_to_c: invalid event pointer");
    }
    
    /* Extract fields from OCaml record */
    const char* path = String_val(Field(ml_event, 0));
    size_t path_len = strlen(path);
    
    /* Allocate and copy path */
    event->path = malloc(path_len + 1);
    if (!event->path) {
        caml_failwith("distcore_write_event_to_c: malloc failed for path");
    }
    memcpy(event->path, path, path_len + 1);
    
    event->offset = Int64_val(Field(ml_event, 1));
    event->is_wal = Bool_val(Field(ml_event, 3));
    event->fence_token = Int64_val(Field(ml_event, 4));
    
    /* Extract data from bigarray */
    value ml_data = Field(ml_event, 2);
    struct caml_ba_array* ba = Caml_ba_array_val(ml_data);
    if (!ba || !ba->data || ba->dim[0] < 0) {
        free(event->path);
        caml_failwith("distcore_write_event_to_c: invalid bigarray");
    }
    
    event->length = ba->dim[0];
    event->data = (char*)ba->data; /* Zero-copy reference */
    
    CAMLreturn(Val_unit);
}

/* Create bigarray from C memory pointer (zero-copy) */
value distcore_create_bigarray_from_ptr(value v_ptr, value v_len) {
    CAMLparam2(v_ptr, v_len);
    CAMLlocal1(ml_array);
    
    void* ptr = (void*)Nativeint_val(v_ptr);
    intnat len = Long_val(v_len);
    
    if (!ptr || len <= 0) {
        caml_failwith("distcore_create_bigarray_from_ptr: invalid pointer or length");
    }
    
    intnat dims[1] = {len};
    ml_array = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, ptr, dims);
    
    CAMLreturn(ml_array);
}

/* Create managed C memory from bigarray */
value distcore_bigarray_to_c_copy(value ml_array) {
    CAMLparam1(ml_array);
    CAMLlocal1(ml_ptr);
    
    struct caml_ba_array* ba = Caml_ba_array_val(ml_array);
    if (!ba || !ba->data || ba->dim[0] < 0) {
        caml_failwith("distcore_bigarray_to_c_copy: invalid bigarray");
    }
    
    size_t len = ba->dim[0];
    void* copy = malloc(len);
    if (!copy) {
        caml_failwith("distcore_bigarray_to_c_copy: malloc failed");
    }
    
    memcpy(copy, ba->data, len);
    ml_ptr = caml_copy_nativeint((intnat)copy);
    
    CAMLreturn(ml_ptr);
}

/* Free C memory allocated by stubs */
value distcore_free_c_memory(value v_ptr) {
    CAMLparam1(v_ptr);
    
    void* ptr = (void*)Nativeint_val(v_ptr);
    if (ptr) {
        free(ptr);
    }
    
    CAMLreturn(Val_unit);
}

/* Initialize minimal stubs - just verify OCaml runtime is working */
value distcore_stubs_init(value unit) {
    CAMLparam1(unit);
    
    /* Simple health check - create and free a small allocation */
    void* test = malloc(1024);
    if (!test) {
        CAMLreturn(Val_bool(0));
    }
    free(test);
    
    /* Verify bigarray creation works */
    char test_data[16] = "test data";
    intnat dims[1] = {16};
    value test_array = caml_ba_alloc(CAML_BA_UINT8 | CAML_BA_C_LAYOUT, 1, test_data, dims);
    
    /* If we get here, basic functionality works */
    CAMLreturn(Val_bool(1));
}

/* Get OCaml runtime information for debugging */
value distcore_get_runtime_info(value unit) {
    CAMLparam1(unit);
    CAMLlocal1(info_tuple);
    
    /* Create tuple with basic runtime info */
    info_tuple = caml_alloc_tuple(3);
    Store_field(info_tuple, 0, caml_copy_string("OCaml Runtime"));
    Store_field(info_tuple, 1, caml_copy_string(OCAML_VERSION));
    Store_field(info_tuple, 2, Val_bool(1)); /* Runtime active */
    
    CAMLreturn(info_tuple);
}

/* Performance-critical memory operations with bounds checking */
value distcore_safe_memcmp(value v_arr1, value v_offset1, value v_arr2, value v_offset2, value v_len) {
    CAMLparam5(v_arr1, v_offset1, v_arr2, v_offset2, v_len);
    
    struct caml_ba_array* ba1 = Caml_ba_array_val(v_arr1);
    struct caml_ba_array* ba2 = Caml_ba_array_val(v_arr2);
    intnat offset1 = Long_val(v_offset1);
    intnat offset2 = Long_val(v_offset2);
    intnat len = Long_val(v_len);
    
    /* Bounds checking */
    if (!ba1 || !ba2 || !ba1->data || !ba2->data) {
        caml_failwith("distcore_safe_memcmp: invalid bigarray");
    }
    
    if (offset1 < 0 || offset2 < 0 || len < 0 ||
        offset1 + len > ba1->dim[0] || offset2 + len > ba2->dim[0]) {
        caml_failwith("distcore_safe_memcmp: bounds check failed");
    }
    
    char* ptr1 = (char*)ba1->data + offset1;
    char* ptr2 = (char*)ba2->data + offset2;
    
    int result = memcmp(ptr1, ptr2, len);
    CAMLreturn(Val_int(result));
}

value distcore_safe_memcpy(value v_dst, value v_dst_offset, value v_src, value v_src_offset, value v_len) {
    CAMLparam5(v_dst, v_dst_offset, v_src, v_src_offset, v_len);
    
    struct caml_ba_array* dst_ba = Caml_ba_array_val(v_dst);
    struct caml_ba_array* src_ba = Caml_ba_array_val(v_src);
    intnat dst_offset = Long_val(v_dst_offset);
    intnat src_offset = Long_val(v_src_offset);
    intnat len = Long_val(v_len);
    
    /* Bounds checking */
    if (!dst_ba || !src_ba || !dst_ba->data || !src_ba->data) {
        caml_failwith("distcore_safe_memcpy: invalid bigarray");
    }
    
    if (dst_offset < 0 || src_offset < 0 || len < 0 ||
        dst_offset + len > dst_ba->dim[0] || src_offset + len > src_ba->dim[0]) {
        caml_failwith("distcore_safe_memcpy: bounds check failed");
    }
    
    char* dst_ptr = (char*)dst_ba->data + dst_offset;
    char* src_ptr = (char*)src_ba->data + src_offset;
    
    /* Use memmove for overlapping regions */
    memmove(dst_ptr, src_ptr, len);
    
    CAMLreturn(Val_unit);
}

/* Simple hash function for performance testing */
value distcore_simple_hash(value v_data, value v_offset, value v_len) {
    CAMLparam3(v_data, v_offset, v_len);
    
    struct caml_ba_array* ba = Caml_ba_array_val(v_data);
    intnat offset = Long_val(v_offset);
    intnat len = Long_val(v_len);
    
    if (!ba || !ba->data || offset < 0 || len < 0 || offset + len > ba->dim[0]) {
        caml_failwith("distcore_simple_hash: invalid parameters");
    }
    
    char* data = (char*)ba->data + offset;
    uint32_t hash = 0;
    
    /* Simple FNV-1a hash */
    for (intnat i = 0; i < len; i++) {
        hash ^= (uint32_t)data[i];
        hash *= 0x01000193;
    }
    
    CAMLreturn(caml_copy_int32(hash));
}