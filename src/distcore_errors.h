/* DistCore Error Codes - Specific error codes for debugging */

#ifndef DISTCORE_ERRORS_H
#define DISTCORE_ERRORS_H

/* Success */
#define DISTCORE_OK                    0

/* Initialization errors (1-99) */
#define DISTCORE_ERR_ALREADY_INIT      1
#define DISTCORE_ERR_OCAML_INIT        2
#define DISTCORE_ERR_VFS_REGISTER      3
#define DISTCORE_ERR_VFS_NOT_FOUND     4
#define DISTCORE_ERR_DLSYM_FAILED      5

/* Memory errors (100-199) */
#define DISTCORE_ERR_OUT_OF_MEMORY     100
#define DISTCORE_ERR_NULL_POINTER      101
#define DISTCORE_ERR_BUFFER_TOO_SMALL  102
#define DISTCORE_ERR_STACK_OVERFLOW    103
#define DISTCORE_ERR_HEAP_CORRUPTION   104

/* Parameter errors (200-299) */
#define DISTCORE_ERR_INVALID_PARAM     200
#define DISTCORE_ERR_OUT_OF_RANGE      201
#define DISTCORE_ERR_PATH_TOO_LONG     202
#define DISTCORE_ERR_NEGATIVE_SIZE     203
#define DISTCORE_ERR_OVERFLOW          204

/* I/O errors (300-399) */
#define DISTCORE_ERR_IO_READ           300
#define DISTCORE_ERR_IO_WRITE          301
#define DISTCORE_ERR_IO_SEEK           302
#define DISTCORE_ERR_IO_TRUNCATE       303
#define DISTCORE_ERR_FILE_NOT_FOUND    304
#define DISTCORE_ERR_PERMISSION        305

/* Queue errors (400-499) */
#define DISTCORE_ERR_QUEUE_FULL        400
#define DISTCORE_ERR_QUEUE_EMPTY       401
#define DISTCORE_ERR_QUEUE_CORRUPT     402

/* OCaml integration errors (500-599) */
#define DISTCORE_ERR_OCAML_CALLBACK    500
#define DISTCORE_ERR_OCAML_EXCEPTION   501
#define DISTCORE_ERR_OCAML_GC          502
#define DISTCORE_ERR_OCAML_SYMBOL      503

/* VFS errors (600-699) */
#define DISTCORE_ERR_VFS_INIT          600
#define DISTCORE_ERR_VFS_OPEN          601
#define DISTCORE_ERR_VFS_CLOSE         602
#define DISTCORE_ERR_VFS_SYNC          603

/* Thread safety errors (700-799) */
#define DISTCORE_ERR_LOCK_FAILED       700
#define DISTCORE_ERR_DEADLOCK          701
#define DISTCORE_ERR_RACE_CONDITION    702

/* Error string function */
const char* distcore_error_string(int error_code);

#endif /* DISTCORE_ERRORS_H */