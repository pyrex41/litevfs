/* DistCore Error String Implementation */

#include "distcore_errors.h"

const char* distcore_error_string(int error_code) {
    switch (error_code) {
        /* Success */
        case DISTCORE_OK:
            return "Success";
            
        /* Initialization errors */
        case DISTCORE_ERR_ALREADY_INIT:
            return "Already initialized";
        case DISTCORE_ERR_OCAML_INIT:
            return "OCaml runtime initialization failed";
        case DISTCORE_ERR_VFS_REGISTER:
            return "VFS registration failed";
        case DISTCORE_ERR_VFS_NOT_FOUND:
            return "VFS not found";
        case DISTCORE_ERR_DLSYM_FAILED:
            return "Dynamic symbol lookup failed";
            
        /* Memory errors */
        case DISTCORE_ERR_OUT_OF_MEMORY:
            return "Out of memory";
        case DISTCORE_ERR_NULL_POINTER:
            return "Null pointer";
        case DISTCORE_ERR_BUFFER_TOO_SMALL:
            return "Buffer too small";
        case DISTCORE_ERR_STACK_OVERFLOW:
            return "Stack overflow";
        case DISTCORE_ERR_HEAP_CORRUPTION:
            return "Heap corruption detected";
            
        /* Parameter errors */
        case DISTCORE_ERR_INVALID_PARAM:
            return "Invalid parameter";
        case DISTCORE_ERR_OUT_OF_RANGE:
            return "Parameter out of range";
        case DISTCORE_ERR_PATH_TOO_LONG:
            return "Path too long";
        case DISTCORE_ERR_NEGATIVE_SIZE:
            return "Negative size";
        case DISTCORE_ERR_OVERFLOW:
            return "Integer overflow";
            
        /* I/O errors */
        case DISTCORE_ERR_IO_READ:
            return "Read error";
        case DISTCORE_ERR_IO_WRITE:
            return "Write error";
        case DISTCORE_ERR_IO_SEEK:
            return "Seek error";
        case DISTCORE_ERR_IO_TRUNCATE:
            return "Truncate error";
        case DISTCORE_ERR_FILE_NOT_FOUND:
            return "File not found";
        case DISTCORE_ERR_PERMISSION:
            return "Permission denied";
            
        /* Queue errors */
        case DISTCORE_ERR_QUEUE_FULL:
            return "Queue full";
        case DISTCORE_ERR_QUEUE_EMPTY:
            return "Queue empty";
        case DISTCORE_ERR_QUEUE_CORRUPT:
            return "Queue corrupted";
            
        /* OCaml integration errors */
        case DISTCORE_ERR_OCAML_CALLBACK:
            return "OCaml callback failed";
        case DISTCORE_ERR_OCAML_EXCEPTION:
            return "OCaml exception";
        case DISTCORE_ERR_OCAML_GC:
            return "OCaml GC error";
        case DISTCORE_ERR_OCAML_SYMBOL:
            return "OCaml symbol not found";
            
        /* VFS errors */
        case DISTCORE_ERR_VFS_INIT:
            return "VFS initialization failed";
        case DISTCORE_ERR_VFS_OPEN:
            return "VFS open failed";
        case DISTCORE_ERR_VFS_CLOSE:
            return "VFS close failed";
        case DISTCORE_ERR_VFS_SYNC:
            return "VFS sync failed";
            
        /* Thread safety errors */
        case DISTCORE_ERR_LOCK_FAILED:
            return "Lock acquisition failed";
        case DISTCORE_ERR_DEADLOCK:
            return "Deadlock detected";
        case DISTCORE_ERR_RACE_CONDITION:
            return "Race condition detected";
            
        default:
            return "Unknown error";
    }
}